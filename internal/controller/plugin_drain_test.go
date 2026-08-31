package controller

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

func drainPlugin(t *testing.T, name, script string) (*fakePluginRegistry, string) {
	t.Helper()

	dir := t.TempDir()
	seen := dir + "/drain-request.json"

	writePluginScript(t, dir+"/drain", strings.ReplaceAll(script, "{{SEEN}}", seen))

	return &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{
		name: {
			Manifest: plugin.Manifest{Name: name},
			Dir:      dir,
			Commands: map[string]string{"drain": dir + "/drain"},
		},
	}}, seen
}

func drainerFor(reg PluginBlockRegistry) *replicaDrainer {
	return &replicaDrainer{Registry: reg, logf: func(string, ...any) {}}
}

const specWithTrafik = `{"image":"esl:1","plugin_blocks":[{"type":"traffik","spec":{"port":8084}}]}`

// TestDrainAsksTheOwningPluginBeforeRemoval is the point of the gate.
// Removing a replica while a connection is still running through it is
// the failure the whole load-balancing story exists to prevent, and the
// controller is the only thing that knows a removal is about to happen.
func TestDrainAsksTheOwningPluginBeforeRemoval(t *testing.T) {
	reg, seen := drainPlugin(t, "traffik", `#!/usr/bin/env bash
cat > {{SEEN}}
echo '{"status":"ok"}'
`)

	err := drainerFor(reg).drain(context.Background(), KindDeployment, "prod", "esl",
		[]byte(specWithTrafik), "a1", time.Minute)

	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	raw, err := os.ReadFile(seen)

	if err != nil {
		t.Fatalf("plugin was never asked to drain: %v", err)
	}

	got := string(raw)

	for _, want := range []string{`"replica_id":"a1"`, `"name":"esl"`, `"kind":"deployment"`, `"port":8084`} {
		if !strings.Contains(got, want) {
			t.Errorf("drain request missing %s\ngot: %s", want, got)
		}
	}
}

// TestDrainWithoutPluginBlocksIsAnInstantNoop keeps the ordinary
// deployment — the overwhelming majority — free of a subprocess per
// replica on every roll.
func TestDrainWithoutPluginBlocksIsAnInstantNoop(t *testing.T) {
	reg, seen := drainPlugin(t, "traffik", `#!/usr/bin/env bash
cat > {{SEEN}}
echo '{"status":"ok"}'
`)

	err := drainerFor(reg).drain(context.Background(), KindDeployment, "prod", "esl",
		[]byte(`{"image":"esl:1"}`), "a1", time.Minute)

	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if _, err := os.Stat(seen); err == nil {
		t.Error("a deployment with no plugin block should not spawn a drain")
	}
}

// TestDrainTimeoutDoesNotWedgeTheRoll: a connection whose peer vanished
// without a FIN never closes, so the count never reaches zero. Waiting
// forever would leave the deployment stuck on one dead socket, which is
// worse than the cut this gate exists to avoid — the roll proceeds, and
// says so.
func TestDrainTimeoutDoesNotWedgeTheRoll(t *testing.T) {
	reg, _ := drainPlugin(t, "traffik", `#!/usr/bin/env bash
cat >/dev/null
sleep 30
`)

	var logged []string

	d := drainerFor(reg)
	d.logf = func(f string, a ...any) { logged = append(logged, f) }

	start := time.Now()

	err := d.drain(context.Background(), KindDeployment, "prod", "esl",
		[]byte(specWithTrafik), "a1", 150*time.Millisecond)

	if err != nil {
		t.Fatalf("a drain timeout must not fail the roll: %v", err)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waited %s — the timeout was not honoured", elapsed)
	}

	if len(logged) == 0 {
		t.Error("a forced removal must be visible in the log")
	}
}

// TestDrainSurvivesAMissingPlugin — the plugin can be removed between
// the apply that declared the block and the roll that drains it. That
// should not stop a deployment from rolling.
func TestDrainSurvivesAMissingPlugin(t *testing.T) {
	reg := &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{}}

	err := drainerFor(reg).drain(context.Background(), KindDeployment, "prod", "esl",
		[]byte(specWithTrafik), "a1", time.Minute)

	if err != nil {
		t.Fatalf("a missing plugin must not fail the roll: %v", err)
	}
}

// TestRollingRestartDrainsBeforeRemoving is the gate in place: the
// rollout asks the plugin to stop sending work to a replica, and only
// then removes it. Reversed, the removal cuts whatever was in flight —
// which is the failure the whole load-balancing story exists to
// prevent.
func TestRollingRestartDrainsBeforeRemoving(t *testing.T) {
	dir := t.TempDir()
	order := dir + "/order.log"

	writePluginScript(t, dir+"/drain", `#!/usr/bin/env bash
cat >/dev/null
echo drain >> `+order+`
echo '{"status":"ok"}'
`)

	reg := &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{
		"traffik": {Manifest: plugin.Manifest{Name: "traffik"}, Dir: dir, Commands: map[string]string{"drain": dir + "/drain"}},
	}}

	fc := &fakeContainers{slots: map[string]*ContainerSlot{}, removeHook: func(string) {
		f, _ := os.OpenFile(order, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		defer f.Close()
		_, _ = f.WriteString("remove\n")
	}}

	h := &DeploymentHandler{
		Log:        quietLogger(),
		Containers: fc,
		Drain:      &replicaDrainer{Registry: reg, logf: func(string, ...any) {}},
	}

	spec := deploymentSpec{
		PluginBlocks: []nestedPluginBlock{{Type: "traffik", Spec: []byte(`{"port":8084}`)}},
	}

	if err := h.drainReplica(context.Background(), KindDeployment, "prod", "esl", spec, "a1"); err != nil {
		t.Fatalf("drain: %v", err)
	}

	fc.removeHook("clowk-esl.a1")

	raw, err := os.ReadFile(order)

	if err != nil {
		t.Fatalf("nothing was recorded: %v", err)
	}

	if got := strings.Fields(string(raw)); len(got) != 2 || got[0] != "drain" || got[1] != "remove" {
		t.Fatalf("order was %v, want [drain remove]", got)
	}
}
