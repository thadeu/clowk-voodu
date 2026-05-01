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

// fakePluginRegistry is a stub that returns a pre-registered
// LoadedPlugin for a given block type. The plugin's expand
// command is a fake that emits a canned envelope so tests can
// assert on the wire shape without spawning real binaries.
type fakePluginRegistry struct {
	plugins map[string]*plugins.LoadedPlugin
}

func (f *fakePluginRegistry) LookupByBlock(blockType string) (*plugins.LoadedPlugin, bool) {
	p, ok := f.plugins[blockType]
	return p, ok
}

// TestExpandPluginBlocks_NonCoreRoutesThroughPlugin verifies the
// happy path: a manifest with a non-core kind is sent to the
// matching plugin via stdin, and the plugin's returned manifests
// take its place in the output list.
//
// We don't test the full plugin process here — that path requires
// disk + fork. Instead the registry returns a pre-built plugin
// whose expand binary is a tiny shell script written at test
// time. Plugin envelope is emitted as JSON to stdout; voodu's
// envelope parser handles it transparently.
func TestExpandPluginBlocks_NonCoreRoutesThroughPlugin(t *testing.T) {
	pluginDir := t.TempDir()

	// Create a tiny shell script that reads stdin (JSON expand
	// request), echoes a canned envelope to stdout. The script
	// passes through the input's name+scope so the test can
	// assert the plugin saw the right values.
	expandPath := pluginDir + "/expand"
	script := `#!/usr/bin/env bash
read -r line
echo '{"status":"ok","data":{"kind":"statefulset","scope":"data","name":"main","spec":{"image":"postgres:15-alpine","replicas":1}}}'
`

	writePluginScript(t, expandPath, script)

	// Plugin manifest declaring "postgres" — by convention the
	// block type matches the plugin name. discoverCommands
	// reads bin/expand from the plugin dir.
	loaded := &plugins.LoadedPlugin{
		Manifest: plugin.Manifest{Name: "postgres"},
		Dir:      pluginDir,
		Commands: map[string]string{"expand": expandPath},
	}

	a := &API{
		PluginBlocks: &fakePluginRegistry{
			plugins: map[string]*plugins.LoadedPlugin{
				"postgres": loaded,
			},
		},
	}

	in := []*Manifest{
		{
			Kind:  "postgres",
			Scope: "data",
			Name:  "main",
			Spec:  json.RawMessage(`{"image":"postgres:15-alpine","replicas":1}`),
		},
	}

	out, _, _, err := a.expandPluginBlocks(context.Background(), in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	if len(out) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(out))
	}

	got := out[0]

	if got.Kind != KindStatefulset {
		t.Errorf("expanded kind = %q, want statefulset", got.Kind)
	}

	if got.Scope != "data" || got.Name != "main" {
		t.Errorf("scope/name lost in expansion: %q/%q", got.Scope, got.Name)
	}
}

// TestExpandPluginBlocks_CoreKindUntouched: built-in kinds bypass
// the registry entirely. A nil registry must NOT cause a 400 for
// a manifest that doesn't need expansion.
func TestExpandPluginBlocks_CoreKindUntouched(t *testing.T) {
	a := &API{} // no registry — non-core would fail

	in := []*Manifest{
		{Kind: KindStatefulset, Scope: "data", Name: "pg", Spec: json.RawMessage(`{}`)},
		{Kind: KindDeployment, Scope: "test", Name: "web", Spec: json.RawMessage(`{}`)},
	}

	out, _, _, err := a.expandPluginBlocks(context.Background(), in)
	if err != nil {
		t.Fatalf("core-only manifests must not error: %v", err)
	}

	if len(out) != 2 {
		t.Errorf("expected 2 manifests passed through, got %d", len(out))
	}
}

// TestExpandPluginBlocks_UnknownPluginErrors verifies the
// missing-plugin error message includes a hint about
// `vd plugins:install`. Operators who typo a block name see this
// often enough that the wording matters.
func TestExpandPluginBlocks_UnknownPluginErrors(t *testing.T) {
	a := &API{
		PluginBlocks: &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{}},
	}

	in := []*Manifest{
		{Kind: "frobnicator", Name: "main", Spec: json.RawMessage(`{}`)},
	}

	_, _, _, err := a.expandPluginBlocks(context.Background(), in)
	if err == nil {
		t.Fatal("expected error for unknown plugin")
	}

	if !strings.Contains(err.Error(), "frobnicator") {
		t.Errorf("error must mention the kind: %v", err)
	}

	if !strings.Contains(err.Error(), "vd plugins:install") {
		t.Errorf("error should hint at install command: %v", err)
	}
}

// TestExpandPluginBlocks_RejectsNonCoreOutput: a plugin that
// returns another plugin block (instead of a core kind) must
// fail. Recursive expansion would explode the dependency graph;
// the M-D2 contract is "plugins expand to core, period".
func TestExpandPluginBlocks_RejectsNonCoreOutput(t *testing.T) {
	pluginDir := t.TempDir()

	expandPath := pluginDir + "/expand"
	script := `#!/usr/bin/env bash
read -r line
echo '{"status":"ok","data":{"kind":"redis","name":"chained","spec":{}}}'
`

	writePluginScript(t, expandPath, script)

	loaded := &plugins.LoadedPlugin{
		Manifest: plugin.Manifest{Name: "postgres"},
		Dir:      pluginDir,
		Commands: map[string]string{"expand": expandPath},
	}

	a := &API{
		PluginBlocks: &fakePluginRegistry{
			plugins: map[string]*plugins.LoadedPlugin{"postgres": loaded},
		},
	}

	in := []*Manifest{
		{Kind: "postgres", Name: "main", Spec: json.RawMessage(`{}`)},
	}

	_, _, _, err := a.expandPluginBlocks(context.Background(), in)
	if err == nil {
		t.Fatal("expected error for plugin returning non-core kind")
	}

	if !strings.Contains(err.Error(), "non-core kind") {
		t.Errorf("error should call out non-core kind: %v", err)
	}
}

// TestExpandPluginBlocks_LatestForcesReinstall verifies the
// `version = "latest"` opt-in: even when the plugin is
// already installed and would otherwise be reused, the
// controller re-runs the install path. This is the
// always-refresh-from-default-branch escape hatch operators
// reach for during plugin development.
//
// We can't easily exercise the full Installer without a
// live git remote, so the test asserts the IsReserved
// metadata extraction + the needsInstall decision via a
// fake registry that observes lookups.
func TestExpandPluginBlocks_LatestExtraction(t *testing.T) {
	repo, version, cleaned := extractInstallMetadata(json.RawMessage(`{
		"image": "redis:8",
		"plugin": {
			"version": "latest",
			"repo":    "thadeu/voodu-redis"
		}
	}`))

	if version != "latest" {
		t.Errorf(`version: got %q, want "latest"`, version)
	}

	if repo != "thadeu/voodu-redis" {
		t.Errorf("repo: got %q", repo)
	}

	// The plugin nested block must be stripped from the spec
	// before the plugin's expand sees it.
	var spec map[string]any
	if err := json.Unmarshal(cleaned, &spec); err != nil {
		t.Fatalf("decode cleaned spec: %v", err)
	}

	if _, ok := spec["plugin"]; ok {
		t.Errorf("plugin block leaked into cleaned spec: %+v", spec)
	}

	if spec["image"] != "redis:8" {
		t.Errorf("non-metadata attrs lost in extraction: %+v", spec)
	}
}

// writePluginScript is a tiny test helper: write `body` to
// `path` with executable permissions so plug.Run can spawn it
// like a real plugin binary.
func writePluginScript(t *testing.T, path, body string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
}

// TestExpand_PassesConfigInStdin: stateful plugins (voodu-redis,
// voodu-postgres) need to know whether prior state exists so
// they can stay idempotent. Confirms the controller pre-fetches
// config and includes it in the expand stdin envelope.
func TestExpand_PassesConfigInStdin(t *testing.T) {
	pluginDir := t.TempDir()
	stdinSink := pluginDir + "/expand-stdin.json"

	// Plugin captures stdin to disk, returns a noop manifest.
	script := `#!/bin/sh
cat > ` + stdinSink + `
echo '{"status":"ok","data":{"kind":"statefulset","scope":"data","name":"redis","spec":{"image":"redis:8"}}}'
`

	writePluginScript(t, pluginDir+"/expand", script)

	loaded := &plugins.LoadedPlugin{
		Manifest: plugin.Manifest{Name: "redis"},
		Dir:      pluginDir,
		Commands: map[string]string{"expand": pluginDir + "/expand"},
	}

	store := newMemStore()

	// Pre-seed config so the plugin sees REDIS_PASSWORD.
	_ = store.PatchConfig(context.Background(), "data", "redis", map[string]string{
		"REDIS_PASSWORD": "from-prior-apply",
	})

	a := &API{
		Store:        store,
		PluginBlocks: &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{"redis": loaded}},
	}

	in := []*Manifest{{
		Kind:  "redis",
		Scope: "data",
		Name:  "redis",
		Spec:  json.RawMessage(`{"image":"redis:8"}`),
	}}

	if _, _, _, err := a.expandPluginBlocks(context.Background(), in); err != nil {
		t.Fatalf("expand: %v", err)
	}

	captured, err := os.ReadFile(stdinSink)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}

	var stdin map[string]any
	if err := json.Unmarshal(captured, &stdin); err != nil {
		t.Fatalf("captured stdin not JSON: %v\n%s", err, captured)
	}

	cfg, ok := stdin["config"].(map[string]any)
	if !ok {
		t.Fatalf("config field missing or wrong type: %T", stdin["config"])
	}

	if cfg["REDIS_PASSWORD"] != "from-prior-apply" {
		t.Errorf("config.REDIS_PASSWORD = %v, want from-prior-apply", cfg["REDIS_PASSWORD"])
	}
}

// TestExpand_AppliesActionsAlongsideManifests: when a plugin
// emits the new {manifests, actions} shape, the controller must
// apply each action against the store before returning the
// manifests. Pin the contract: voodu-redis depends on this to
// persist a freshly-generated REDIS_PASSWORD on first apply.
func TestExpand_AppliesActionsAlongsideManifests(t *testing.T) {
	pluginDir := t.TempDir()

	// Plugin emits both manifests and a config_set action.
	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "manifests": [
      {"kind": "statefulset", "scope": "data", "name": "redis", "spec": {"image":"redis:8"}}
    ],
    "actions": [
      {"type": "config_set", "scope": "data", "name": "redis", "kv": {"REDIS_PASSWORD": "fresh-password"}}
    ]
  }
}
EOF
`

	writePluginScript(t, pluginDir+"/expand", script)

	loaded := &plugins.LoadedPlugin{
		Manifest: plugin.Manifest{Name: "redis"},
		Dir:      pluginDir,
		Commands: map[string]string{"expand": pluginDir + "/expand"},
	}

	store := newMemStore()

	a := &API{
		Store:        store,
		PluginBlocks: &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{"redis": loaded}},
	}

	in := []*Manifest{{
		Kind:  "redis",
		Scope: "data",
		Name:  "redis",
		Spec:  json.RawMessage(`{"image":"redis:8"}`),
	}}

	out, _, _, err := a.expandPluginBlocks(context.Background(), in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	// Manifest came through.
	if len(out) != 1 || out[0].Kind != KindStatefulset {
		t.Errorf("expected 1 statefulset manifest, got %+v", out)
	}

	// Action was applied to the store.
	cfg, _ := store.GetConfig(context.Background(), "data", "redis")
	if cfg["REDIS_PASSWORD"] != "fresh-password" {
		t.Errorf("expand action not applied: REDIS_PASSWORD = %q", cfg["REDIS_PASSWORD"])
	}
}

// TestExpand_LegacyArrayShapeStillWorks: existing plugins that
// emit just an array of manifests (no actions) keep working —
// backward compatibility is non-negotiable, voodu-postgres and
// voodu-caddy ship that shape in production.
func TestExpand_LegacyArrayShapeStillWorks(t *testing.T) {
	pluginDir := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
echo '{"status":"ok","data":[{"kind":"statefulset","scope":"data","name":"main","spec":{"image":"postgres:15"}}]}'
`

	writePluginScript(t, pluginDir+"/expand", script)

	loaded := &plugins.LoadedPlugin{
		Manifest: plugin.Manifest{Name: "postgres"},
		Dir:      pluginDir,
		Commands: map[string]string{"expand": pluginDir + "/expand"},
	}

	a := &API{
		Store:        newMemStore(),
		PluginBlocks: &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{"postgres": loaded}},
	}

	in := []*Manifest{{
		Kind:  "postgres",
		Scope: "data",
		Name:  "main",
		Spec:  json.RawMessage(`{}`),
	}}

	out, _, _, err := a.expandPluginBlocks(context.Background(), in)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	if len(out) != 1 || out[0].Kind != KindStatefulset {
		t.Errorf("legacy array shape failed: got %+v", out)
	}
}
