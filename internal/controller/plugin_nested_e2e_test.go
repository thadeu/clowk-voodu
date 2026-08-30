package controller

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// TestNestedPluginBlockEndToEnd walks the whole contract with a plugin
// that behaves the way voodu-trafik will: it refuses a block with no
// bind at apply time, fills in the addressing default, and then
// receives the live replicas once the workload has reconciled.
//
// The spec this starts from is what the parser produces for:
//
//	deployment "prod" "esl" {
//	  image    = "esl:1"
//	  replicas = 2
//
//	  trafik {
//	    bind = "0.0.0.0:8084"
//	    port = 8084
//	  }
//	}
//
// The parser half is pinned by TestDeploymentCollectsNestedPluginBlock
// in internal/manifest; the two meet at this JSON.
func TestNestedPluginBlockEndToEnd(t *testing.T) {
	dir := t.TempDir()
	seenApply := dir + "/apply-request.json"

	// expand: validate + normalise. Refuses a block with no bind, and
	// fills the addressing default — the two things a plugin owning
	// its own block is for.
	writePluginScript(t, dir+"/expand", `#!/usr/bin/env bash
req="$(cat)"
if ! grep -q '"bind"' <<<"$req"; then
  echo '{"status":"error","error":"bind is required"}'
  exit 1
fi
port="$(sed -n 's/.*"port":\([0-9]*\).*/\1/p' <<<"$req" | head -1)"
echo "{\"status\":\"ok\",\"data\":{\"bind\":\"0.0.0.0:8084\",\"port\":${port},\"addressing\":\"ip\"}}"
`)

	// apply: what a real trafik would turn into PUT /config.
	writePluginScript(t, dir+"/apply", `#!/usr/bin/env bash
cat > `+seenApply+`
echo '{"status":"ok"}'
`)

	registry := &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{
		"trafik": {
			Manifest: plugin.Manifest{Name: "trafik"},
			Dir:      dir,
			Commands: map[string]string{"expand": dir + "/expand", "apply": dir + "/apply"},
		},
	}}

	// ── 1. apply-time: the block reaches its plugin and is normalised
	parsed := &Manifest{
		Kind:  KindDeployment,
		Scope: "prod",
		Name:  "esl",
		Spec: json.RawMessage(`{"image":"esl:1","replicas":2,` +
			`"plugin_blocks":[{"type":"trafik","spec":{"bind":"0.0.0.0:8084","port":8084}}]}`),
	}

	api := &API{PluginBlocks: registry}

	out, _, _, err := api.expandPluginBlocks(context.Background(), []*Manifest{parsed})

	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	if !strings.Contains(string(out[0].Spec), `"addressing":"ip"`) {
		t.Fatalf("the plugin's normalisation did not survive: %s", out[0].Spec)
	}

	if !strings.Contains(string(out[0].Spec), `"image":"esl:1"`) {
		t.Fatalf("the rest of the deployment spec was lost: %s", out[0].Spec)
	}

	// ── 2. reconcile-time: the live replicas reach the same plugin
	slots, ips := replicaSlots("prod", "esl", "a1", "b2")

	pub := &endpointPublisher{
		Registry:   registry,
		Containers: &fakeContainers{ips: ips},
		logf:       func(string, ...any) {},
	}

	if err := pub.publish(context.Background(), KindDeployment, "prod", "esl", out[0].Spec, slots); err != nil {
		t.Fatalf("publish: %v", err)
	}

	raw, err := os.ReadFile(seenApply)

	if err != nil {
		t.Fatalf("plugin never received the endpoints: %v", err)
	}

	var got pluginApplyRequest

	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode apply request: %v", err)
	}

	if got.Kind != "deployment" || got.Name != "esl" || got.Scope != "prod" {
		t.Errorf("wrong workload: %+v", got)
	}

	if len(got.Endpoints) != 2 {
		t.Fatalf("got %d endpoints, want one per live replica: %+v", len(got.Endpoints), got.Endpoints)
	}

	for i, want := range []replicaEndpoint{
		{ReplicaID: "a1", Address: "172.18.0.2:8084"},
		{ReplicaID: "b2", Address: "172.18.0.3:8084"},
	} {
		if got.Endpoints[i] != want {
			t.Errorf("endpoint %d = %+v, want %+v", i, got.Endpoints[i], want)
		}
	}

	// ── 3. the block's own config travelled with it
	if !strings.Contains(string(got.Block.Spec), `"bind":"0.0.0.0:8084"`) {
		t.Errorf("the plugin was not told its own config back: %s", got.Block.Spec)
	}
}

// TestNestedPluginBlockEndToEndRejectsInvalidBlock is the other half of
// the promise: a block the plugin refuses fails the apply, with the
// plugin's reason, instead of reaching a container.
func TestNestedPluginBlockEndToEndRejectsInvalidBlock(t *testing.T) {
	dir := t.TempDir()

	writePluginScript(t, dir+"/expand", `#!/usr/bin/env bash
req="$(cat)"
if ! grep -q '"bind"' <<<"$req"; then
  echo '{"status":"error","error":"bind is required"}'
  exit 1
fi
echo '{"status":"ok","data":{}}'
`)

	api := &API{PluginBlocks: &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{
		"trafik": {Manifest: plugin.Manifest{Name: "trafik"}, Dir: dir, Commands: map[string]string{"expand": dir + "/expand"}},
	}}}

	m := &Manifest{
		Kind: KindDeployment, Scope: "prod", Name: "esl",
		Spec: json.RawMessage(`{"image":"esl:1","plugin_blocks":[{"type":"trafik","spec":{"port":8084}}]}`),
	}

	_, _, _, err := api.expandPluginBlocks(context.Background(), []*Manifest{m})

	if err == nil || !strings.Contains(err.Error(), "bind is required") {
		t.Fatalf("want the plugin's own refusal, got: %v", err)
	}

	if !strings.Contains(err.Error(), "deployment/prod/esl") {
		t.Errorf("error should locate the workload: %v", err)
	}
}
