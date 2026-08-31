package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// nestedPluginFixture builds an API whose registry knows one plugin,
// whose expand command is the given script.
func nestedPluginFixture(t *testing.T, name, script string) *API {
	t.Helper()

	dir := t.TempDir()
	path := dir + "/expand"

	writePluginScript(t, path, script)

	return &API{
		PluginBlocks: &fakePluginRegistry{
			plugins: map[string]*plugins.LoadedPlugin{
				name: {
					Manifest: plugin.Manifest{Name: name},
					Dir:      dir,
					Commands: map[string]string{"expand": path},
				},
			},
		},
	}
}

func deploymentWithBlocks(t *testing.T, blocks string) *Manifest {
	t.Helper()

	return &Manifest{
		Kind:  KindDeployment,
		Scope: "prod",
		Name:  "esl",
		Spec:  json.RawMessage(`{"image":"esl:1","replicas":3,"plugin_blocks":` + blocks + `}`),
	}
}

func pluginBlocksOf(t *testing.T, m *Manifest) []map[string]any {
	t.Helper()

	var spec struct {
		PluginBlocks []map[string]any `json:"plugin_blocks"`
	}

	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}

	return spec.PluginBlocks
}

// TestNestedBlockReachesItsPluginWithParentContext is the point of the
// nested contract: the plugin that owns the block gets to see it, and
// gets enough of the parent to make sense of it — a listener needs to
// know which workload's replicas it is fronting.
func TestNestedBlockReachesItsPluginWithParentContext(t *testing.T) {
	// Echoes the request back as the normalized spec, so the test can
	// assert on exactly what the plugin received.
	a := nestedPluginFixture(t, "traffik", `#!/usr/bin/env bash
req="$(cat)"
printf '{"status":"ok","data":%s}\n' "$req"
`)

	m := deploymentWithBlocks(t, `[{"type":"traffik","spec":{"bind":"0.0.0.0:8084"}}]`)

	out, _, _, err := a.expandPluginBlocks(context.Background(), []*Manifest{m})

	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	if len(out) != 1 || out[0].Kind != KindDeployment {
		t.Fatalf("core manifest should pass through, got %+v", out)
	}

	blocks := pluginBlocksOf(t, out[0])

	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}

	seen, _ := json.Marshal(blocks[0]["spec"])
	got := string(seen)

	for _, want := range []string{
		`"position":"nested"`,
		`"kind":"traffik"`,
		`"bind":"0.0.0.0:8084"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plugin request missing %s\ngot: %s", want, got)
		}
	}

	if !strings.Contains(got, `"parent"`) || !strings.Contains(got, `"esl"`) {
		t.Errorf("plugin was not told which workload it belongs to\ngot: %s", got)
	}
}

// TestNestedBlockNormalizationIsPersisted keeps the plugin's answer.
// Validating and then throwing the result away would mean the defaults
// a plugin fills in never reach the reconciler.
func TestNestedBlockNormalizationIsPersisted(t *testing.T) {
	a := nestedPluginFixture(t, "traffik", `#!/usr/bin/env bash
cat >/dev/null
echo '{"status":"ok","data":{"bind":"0.0.0.0:8084","port":8084,"addressing":"ip"}}'
`)

	m := deploymentWithBlocks(t, `[{"type":"traffik","spec":{"bind":"0.0.0.0:8084"}}]`)

	out, _, _, err := a.expandPluginBlocks(context.Background(), []*Manifest{m})

	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	spec, _ := json.Marshal(pluginBlocksOf(t, out[0])[0]["spec"])

	if !strings.Contains(string(spec), `"addressing":"ip"`) {
		t.Errorf("plugin defaults were dropped: %s", spec)
	}
}

// TestNestedBlockRejectedWhenPluginMissing is where a typo lands now
// that the parser carries unknown blocks instead of refusing them. The
// message has to name the block and say what to do, or relaxing the
// parser traded a clear error for a mystery.
func TestNestedBlockRejectedWhenPluginMissing(t *testing.T) {
	a := &API{PluginBlocks: &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{}}}

	m := deploymentWithBlocks(t, `[{"type":"probs","spec":{}}]`)

	_, _, _, err := a.expandPluginBlocks(context.Background(), []*Manifest{m})

	if err == nil {
		t.Fatal("a block belonging to no installed plugin must fail the apply")
	}

	msg := err.Error()

	for _, want := range []string{"probs", "deployment", "esl", "plugins:install"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got: %s", want, msg)
		}
	}
}

// TestNestedBlockFailureSurfacesPluginError — a plugin that refuses the
// config must fail the apply, not be ignored. Validation that can be
// skipped is not validation.
func TestNestedBlockFailureSurfacesPluginError(t *testing.T) {
	a := nestedPluginFixture(t, "traffik", `#!/usr/bin/env bash
cat >/dev/null
echo '{"status":"error","error":"bind is required"}'
exit 1
`)

	m := deploymentWithBlocks(t, `[{"type":"traffik","spec":{}}]`)

	_, _, _, err := a.expandPluginBlocks(context.Background(), []*Manifest{m})

	if err == nil || !strings.Contains(err.Error(), "bind is required") {
		t.Fatalf("plugin refusal should fail the apply with its reason, got: %v", err)
	}
}

// TestCoreManifestWithoutPluginBlocksIsUntouched keeps the common case
// free: no plugin blocks means no lookup, no subprocess, and a spec
// that comes out byte-identical.
func TestCoreManifestWithoutPluginBlocksIsUntouched(t *testing.T) {
	a := &API{PluginBlocks: &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{}}}

	before := json.RawMessage(`{"image":"esl:1","replicas":3}`)

	m := &Manifest{Kind: KindDeployment, Scope: "prod", Name: "esl", Spec: before}

	out, _, _, err := a.expandPluginBlocks(context.Background(), []*Manifest{m})

	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	if string(out[0].Spec) != string(before) {
		t.Errorf("spec was rewritten:\n before %s\n after  %s", before, out[0].Spec)
	}
}
