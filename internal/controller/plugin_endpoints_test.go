package controller

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/containers"
	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// recordingPlugin writes whatever it receives on stdin to a file, so a
// test can assert on the exact request the controller sent.
func recordingPlugin(t *testing.T, name string) (*fakePluginRegistry, func() string) {
	t.Helper()

	dir := t.TempDir()
	out := dir + "/seen.json"
	path := dir + "/apply"

	writePluginScript(t, path, `#!/usr/bin/env bash
cat > `+out+`
echo '{"status":"ok"}'
`)

	reg := &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{
		name: {
			Manifest: plugin.Manifest{Name: name},
			Dir:      dir,
			Commands: map[string]string{"apply": path},
		},
	}}

	return reg, func() string {
		b, err := os.ReadFile(out)

		if err != nil {
			t.Fatalf("plugin was never called: %v", err)
		}

		return string(b)
	}
}

func replicaSlots(scope, name string, ids ...string) ([]ContainerSlot, map[string]string) {
	var (
		slots []ContainerSlot
		ips   = map[string]string{}
	)

	for i, id := range ids {
		cn := containers.ContainerName(scope, name, id)

		slots = append(slots, ContainerSlot{
			Name:     cn,
			Running:  true,
			Identity: containers.Identity{Kind: containers.KindDeployment, Scope: scope, Name: name, ReplicaID: id},
		})

		ips[cn] = "172.18.0." + string(rune('2'+i))
	}

	return slots, ips
}

func publisherFor(reg PluginBlockRegistry, ips map[string]string) *endpointPublisher {
	return &endpointPublisher{
		Registry:   reg,
		Containers: &fakeContainers{ips: ips},
		logf:       func(string, ...any) {},
	}
}

// TestPublishesLiveEndpointsToOwningPlugin is the reconcile half of the
// nested contract. The plugin declared a block; it now has to learn
// which replicas are actually up, because that set changes on every
// roll and nothing in the manifest describes it.
func TestPublishesLiveEndpointsToOwningPlugin(t *testing.T) {
	reg, seen := recordingPlugin(t, "traffik")

	slots, ips := replicaSlots("prod", "esl", "a1", "b2")

	spec := json.RawMessage(`{"plugin_blocks":[{"type":"traffik","spec":{"port":8084,"bind":"0.0.0.0:8084"}}]}`)

	err := publisherFor(reg, ips).publish(context.Background(), KindDeployment, "prod", "esl", spec, slots)

	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := seen()

	for _, want := range []string{
		`"kind":"deployment"`,
		`"name":"esl"`,
		`"bind":"0.0.0.0:8084"`,
		`"replica_id":"a1"`,
		`"replica_id":"b2"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("request missing %s\ngot: %s", want, got)
		}
	}
}

// TestEndpointsAreIPsByDefault — a plugin on the host network resolves
// nothing docker knows, so an address is the only thing it can dial.
// That is the common case, so it is the default.
func TestEndpointsAreIPsByDefault(t *testing.T) {
	reg, seen := recordingPlugin(t, "traffik")

	slots, ips := replicaSlots("prod", "esl", "a1")

	spec := json.RawMessage(`{"plugin_blocks":[{"type":"traffik","spec":{"port":8084}}]}`)

	if err := publisherFor(reg, ips).publish(context.Background(), KindDeployment, "prod", "esl", spec, slots); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if got := seen(); !strings.Contains(got, `"address":"172.18.0.2:8084"`) {
		t.Errorf("want the replica's voodu0 address, got: %s", got)
	}
}

// TestEndpointsUseNamesWhenAddressingIsDNS — a plugin that sits on the
// bridge should get names instead: a name survives the container being
// recreated at a different address, so it stays correct between
// reconciles.
func TestEndpointsUseNamesWhenAddressingIsDNS(t *testing.T) {
	reg, seen := recordingPlugin(t, "caddyish")

	slots, ips := replicaSlots("prod", "esl", "a1")

	spec := json.RawMessage(`{"plugin_blocks":[{"type":"caddyish","spec":{"port":8080,"addressing":"dns"}}]}`)

	if err := publisherFor(reg, ips).publish(context.Background(), KindDeployment, "prod", "esl", spec, slots); err != nil {
		t.Fatalf("publish: %v", err)
	}

	want := containers.ContainerName("prod", "esl", "a1") + ":8080"

	if got := seen(); !strings.Contains(got, `"address":"`+want+`"`) {
		t.Errorf("want %q, got: %s", want, got)
	}
}

// TestOnlyRunningReplicasArePublished keeps an address with nothing
// listening out of a load balancer's rotation.
func TestOnlyRunningReplicasArePublished(t *testing.T) {
	reg, seen := recordingPlugin(t, "traffik")

	slots, ips := replicaSlots("prod", "esl", "a1", "b2")
	slots[1].Running = false

	spec := json.RawMessage(`{"plugin_blocks":[{"type":"traffik","spec":{"port":8084}}]}`)

	if err := publisherFor(reg, ips).publish(context.Background(), KindDeployment, "prod", "esl", spec, slots); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := seen()

	if strings.Contains(got, `"replica_id":"b2"`) {
		t.Errorf("a stopped replica was published: %s", got)
	}

	if !strings.Contains(got, `"replica_id":"a1"`) {
		t.Errorf("the running replica is missing: %s", got)
	}
}

// TestNoPluginBlocksMeansNoPluginCall keeps the overwhelmingly common
// deployment free of a subprocess it has no use for.
func TestNoPluginBlocksMeansNoPluginCall(t *testing.T) {
	reg, _ := recordingPlugin(t, "traffik")

	slots, ips := replicaSlots("prod", "esl", "a1")

	spec := json.RawMessage(`{"image":"esl:1"}`)

	if err := publisherFor(reg, ips).publish(context.Background(), KindDeployment, "prod", "esl", spec, slots); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// TestPluginApplyFailureIsReported — a load balancer that never learned
// the new replicas is a silent outage. The reconcile has to surface it
// so it retries.
func TestPluginApplyFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/apply"

	writePluginScript(t, path, `#!/usr/bin/env bash
cat >/dev/null
echo '{"status":"error","error":"listener bind refused"}'
exit 1
`)

	reg := &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{
		"traffik": {Manifest: plugin.Manifest{Name: "traffik"}, Dir: dir, Commands: map[string]string{"apply": path}},
	}}

	slots, ips := replicaSlots("prod", "esl", "a1")

	spec := json.RawMessage(`{"plugin_blocks":[{"type":"traffik","spec":{"port":8084}}]}`)

	err := publisherFor(reg, ips).publish(context.Background(), KindDeployment, "prod", "esl", spec, slots)

	if err == nil || !strings.Contains(err.Error(), "listener bind refused") {
		t.Fatalf("want the plugin's reason surfaced, got: %v", err)
	}
}
