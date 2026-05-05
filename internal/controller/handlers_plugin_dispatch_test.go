package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePluginYAML drops a minimal plugin.yml + bin script under
// PluginsRoot/<name>/. The shell script reads stdin and writes a
// canned envelope to stdout — enough for the dispatch handler
// to exercise its full pipeline (load → invoke → parse →
// apply actions) without needing a real plugin binary.
func writePluginYAML(t *testing.T, root, name, commandName, scriptBody string) {
	t.Helper()

	dir := filepath.Join(root, name)
	binDir := filepath.Join(dir, "bin")

	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}

	yaml := "name: " + name + "\nversion: 0.1.0\ncommands:\n  - name: " + commandName + "\n    help: test command\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(binDir, commandName)

	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0755); err != nil {
		t.Fatal(err)
	}
}

// dispatchTestServer wires a minimal API around a memStore + the
// given PluginsRoot, and returns an httptest.Server hitting it.
// Caller closes the server.
func dispatchTestServer(t *testing.T, root string) (*httptest.Server, *memStore) {
	t.Helper()

	store := newMemStore()

	api := &API{
		Store:       store,
		PluginsRoot: root,
	}

	return httptest.NewServer(api.Handler()), store
}

// TestPluginDispatch_HappyPath_AppliesConfigSet covers the full
// chain: CLI POSTs link payload, plugin returns a config_set
// action, server applies it and the store reflects the new
// config. Pins the contract every plugin link command relies on.
func TestPluginDispatch_HappyPath_AppliesConfigSet(t *testing.T) {
	root := t.TempDir()

	// Plugin reads stdin, ignores it, emits a canned envelope.
	script := `#!/bin/sh
cat > /dev/null
cat <<EOF
{
  "status": "ok",
  "data": {
    "message": "linked redis to web",
    "actions": [
      {
        "type": "config_set",
        "scope": "clowk-lp",
        "name": "web",
        "kv": { "REDIS_URL": "redis://default:s3cret@redis.clowk-lp.voodu:6379" }
      }
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "redis", "link", script)

	srv, store := dispatchTestServer(t, root)
	defer srv.Close()

	// Pre-seed a redis statefulset so fetchPluginCtxForRef has
	// something to attach (kind+spec) — the dispatch path
	// shouldn't fail on missing manifests but we want to
	// exercise the spec-attach branch.
	_, _ = store.Put(context.Background(), &Manifest{
		Kind:  KindStatefulset,
		Scope: "clowk-lp",
		Name:  "redis",
		Spec:  json.RawMessage(`{"image":"redis:8","ports":["6379"]}`),
	})

	body := bytes.NewBufferString(`{
		"from": {"kind": "statefulset", "scope": "clowk-lp", "name": "redis"},
		"to":   {"scope": "clowk-lp", "name": "web"}
	}`)

	resp, err := http.Post(srv.URL+"/plugin/redis/link", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	var out struct {
		Status string `json:"status"`
		Data   struct {
			Message string   `json:"message"`
			Applied []string `json:"applied"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}

	if out.Status != "ok" {
		t.Errorf("status: %q", out.Status)
	}

	if out.Data.Message != "linked redis to web" {
		t.Errorf("message: %q", out.Data.Message)
	}

	if len(out.Data.Applied) != 1 || !strings.Contains(out.Data.Applied[0], "REDIS_URL") {
		t.Errorf("applied: %v", out.Data.Applied)
	}

	// Confirm the store has the new config — the action must
	// actually have been applied, not just acknowledged.
	cfg, err := store.GetConfig(context.Background(), "clowk-lp", "web")
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg["REDIS_URL"]; got != "redis://default:s3cret@redis.clowk-lp.voodu:6379" {
		t.Errorf("REDIS_URL not stored: %q", got)
	}
}

// TestPluginDispatch_SkipRestartSuppressesFanOut pins the
// per-action "skip restart" hatch the sentinel auto-failover
// path uses. With SkipRestart=true, the config write still
// lands but maybeRestartAffected is NOT called, so the target
// manifest's revision stays put (no restart-fan-out re-Put).
//
// Without this gate, sentinel's callback would roll the redis
// statefulset → drop active connections on the freshly promoted
// master → risk ping-pong with sentinel re-electing during the
// reboot window. The flag is the sentinel-aware path's only
// way to record state without triggering side-effects.
func TestPluginDispatch_SkipRestartSuppressesFanOut(t *testing.T) {
	root := t.TempDir()

	// Plugin emits one config_set with skip_restart: true. Only
	// thing the dispatch handler needs to exercise.
	script := `#!/bin/sh
cat > /dev/null
cat <<EOF
{
  "status": "ok",
  "data": {
    "message": "sentinel sync",
    "actions": [
      {
        "type": "config_set",
        "scope": "clowk-lp",
        "name": "redis",
        "kv": { "REDIS_MASTER_ORDINAL": "1" },
        "skip_restart": true
      }
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "redis", "failover", script)

	srv, store := dispatchTestServer(t, root)
	defer srv.Close()

	// Pre-store the redis statefulset so we have something to
	// observe revision on. memStore Put bumps revision on every
	// successful write, so a non-bump after the dispatch proves
	// the fan-out was suppressed.
	pre, err := store.Put(context.Background(), &Manifest{
		Kind:  KindStatefulset,
		Scope: "clowk-lp",
		Name:  "redis",
		Spec:  json.RawMessage(`{"image":"redis:8","replicas":3}`),
	})

	if err != nil {
		t.Fatal(err)
	}

	preRevision := pre.Metadata.Revision

	body := bytes.NewBufferString(`{"args":["clowk-lp/redis","--replica","1","--no-restart"]}`)

	resp, err := http.Post(srv.URL+"/plugin/redis/failover", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	// The config write MUST have landed — SkipRestart only
	// suppresses the fan-out, not the write itself.
	cfg, err := store.GetConfig(context.Background(), "clowk-lp", "redis")
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg["REDIS_MASTER_ORDINAL"]; got != "1" {
		t.Errorf("config write didn't land; REDIS_MASTER_ORDINAL=%q", got)
	}

	// And the manifest revision MUST NOT bump — that's the proof
	// that maybeRestartAffected was skipped (it re-Puts every
	// matching manifest, which would bump revision).
	post, err := store.Get(context.Background(), KindStatefulset, "clowk-lp", "redis")
	if err != nil || post == nil {
		t.Fatalf("manifest missing post-dispatch: %v", err)
	}

	if post.Metadata.Revision != preRevision {
		t.Errorf("statefulset revision changed (%d → %d); SkipRestart=true should have suppressed the restart fan-out",
			preRevision, post.Metadata.Revision)
	}
}

// TestPluginDispatch_NoSkipRestartFiresFanOut is the inverse pin.
// Same wire shape as the SkipRestart test but with the field
// omitted (default false) — the manifest revision MUST bump,
// proving the historical "config_set rolls affected workloads"
// behaviour is still the default for plugin actions.
func TestPluginDispatch_NoSkipRestartFiresFanOut(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
cat <<EOF
{
  "status": "ok",
  "data": {
    "message": "manual failover",
    "actions": [
      {
        "type": "config_set",
        "scope": "clowk-lp",
        "name": "redis",
        "kv": { "REDIS_MASTER_ORDINAL": "1" }
      }
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "redis", "failover", script)

	srv, store := dispatchTestServer(t, root)
	defer srv.Close()

	pre, err := store.Put(context.Background(), &Manifest{
		Kind:  KindStatefulset,
		Scope: "clowk-lp",
		Name:  "redis",
		Spec:  json.RawMessage(`{"image":"redis:8","replicas":3}`),
	})

	if err != nil {
		t.Fatal(err)
	}

	preRevision := pre.Metadata.Revision

	body := bytes.NewBufferString(`{"args":["clowk-lp/redis","--replica","1"]}`)

	resp, err := http.Post(srv.URL+"/plugin/redis/failover", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	post, err := store.Get(context.Background(), KindStatefulset, "clowk-lp", "redis")
	if err != nil || post == nil {
		t.Fatalf("manifest missing: %v", err)
	}

	if post.Metadata.Revision <= preRevision {
		t.Errorf("statefulset revision didn't bump (%d → %d); default-no-skip should fire fan-out",
			preRevision, post.Metadata.Revision)
	}
}

// TestPluginDispatch_UnknownCommand pins the 400 path: a plugin
// whose plugin.yml doesn't declare the command must reject the
// dispatch even though the binary might exist on disk. Prevents
// shadow commands from being invoked invisibly.
func TestPluginDispatch_UnknownCommand(t *testing.T) {
	root := t.TempDir()

	// Plugin only declares "link" — `unlink` should 400.
	writePluginYAML(t, root, "redis", "link", "#!/bin/sh\necho '{}'\n")

	srv, _ := dispatchTestServer(t, root)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/plugin/redis/unlink", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	raw, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(raw), "does not have an executable") {
		t.Errorf("error mismatch: %s", raw)
	}
}

// TestPluginDispatch_PluginNotInstalled covers the operator
// typo'ing a plugin name. Plain 400 with a clear message
// pointing at the missing plugin dir.
func TestPluginDispatch_PluginNotInstalled(t *testing.T) {
	root := t.TempDir()

	srv, _ := dispatchTestServer(t, root)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/plugin/ghost/link", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d", resp.StatusCode)
	}
}

// TestPluginDispatch_PluginExitNonZero surfaces the plugin's
// stderr to the operator. A redis:link that errors mid-script
// (URL build failed, password lookup failed) should reach the
// CLI with the actual reason, not a generic "plugin failed".
func TestPluginDispatch_PluginExitNonZero(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
echo "boom" >&2
exit 7
`

	writePluginYAML(t, root, "redis", "link", script)

	srv, _ := dispatchTestServer(t, root)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/plugin/redis/link", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d", resp.StatusCode)
	}

	raw, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(raw), "exited 7") {
		t.Errorf("error should include exit code: %s", raw)
	}

	if !strings.Contains(string(raw), "boom") {
		t.Errorf("error should include stderr: %s", raw)
	}
}

// TestPluginDispatch_UnknownActionType pins the strict-type
// posture: a plugin emitting an action the controller doesn't
// recognise (typo, future feature) is a hard error. Better than
// silently ignoring — operator might assume the link succeeded.
func TestPluginDispatch_UnknownActionType(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "actions": [{"type": "weird_thing", "scope": "x", "name": "y"}]
  }
}
EOF
`

	writePluginYAML(t, root, "redis", "link", script)

	srv, _ := dispatchTestServer(t, root)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/plugin/redis/link", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status=%d body=%s", resp.StatusCode, raw)
	}

	if !strings.Contains(string(raw), "unknown action type") {
		t.Errorf("error should mention unknown action: %s", raw)
	}
}

// TestPluginDispatch_PassesArgsAsArgv pins the passthrough
// contract: the operator's args arrive at the plugin via
// os.Args (i.e. RunOptions.Args), NOT stdin. The plugin
// parses its own argv just like any other CLI tool.
func TestPluginDispatch_PassesArgsAsArgv(t *testing.T) {
	root := t.TempDir()
	argvSink := root + "/captured-argv.txt"

	// Plugin writes its positional args to a file (one per
	// line). "$@" already excludes $0, so no shift needed.
	script := `#!/bin/sh
for a in "$@"; do
  echo "$a" >> ` + argvSink + `
done
echo '{"status":"ok","data":{"message":"saved"}}'
`

	writePluginYAML(t, root, "redis", "link", script)

	srv, _ := dispatchTestServer(t, root)
	defer srv.Close()

	body := bytes.NewBufferString(`{"args":["clowk-lp/redis","clowk-lp/web","--debug"]}`)

	resp, err := http.Post(srv.URL+"/plugin/redis/link", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	captured, err := os.ReadFile(argvSink)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}

	got := strings.Split(strings.TrimRight(string(captured), "\n"), "\n")

	want := []string{"clowk-lp/redis", "clowk-lp/web", "--debug"}

	if len(got) != len(want) {
		t.Fatalf("argv=%v want %v", got, want)
	}

	for i, w := range want {
		if got[i] != w {
			t.Errorf("argv[%d]=%q want %q", i, got[i], w)
		}
	}
}

// TestPluginDispatch_StdinCarriesContext: the plugin's stdin
// must contain the {plugin, command, controller_url, plugin_dir}
// envelope so it can call back to the controller for state.
// Args flow through os.Args[2:] (RunOptions.Args), NOT stdin —
// that's the passthrough contract: plugin parses its own argv.
func TestPluginDispatch_StdinCarriesContext(t *testing.T) {
	root := t.TempDir()

	stdinSink := filepath.Join(root, "captured-stdin.json")

	// Plugin saves its stdin to disk, returns a noop envelope.
	script := `#!/bin/sh
cat > ` + stdinSink + `
echo '{"status":"ok","data":{"message":"saved"}}'
`

	writePluginYAML(t, root, "redis", "link", script)

	srv, store := dispatchTestServer(t, root)
	defer srv.Close()

	_, _ = store.Put(context.Background(), &Manifest{
		Kind:  KindStatefulset,
		Scope: "clowk-lp",
		Name:  "redis",
		Spec:  json.RawMessage(`{"image":"redis:8"}`),
	})

	body := bytes.NewBufferString(`{
		"args": ["clowk-lp/redis", "clowk-lp/web"]
	}`)

	resp, err := http.Post(srv.URL+"/plugin/redis/link", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	captured, err := os.ReadFile(stdinSink)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}

	var stdin map[string]any
	if err := json.Unmarshal(captured, &stdin); err != nil {
		t.Fatalf("captured stdin not JSON: %v\n%s", err, captured)
	}

	// Stdin envelope has plugin/command/controller_url/plugin_dir
	// — the context needed for the plugin to call back. Args are
	// NOT in stdin; they arrive via os.Args[2:].
	if stdin["plugin"] != "redis" {
		t.Errorf("plugin field: %v", stdin["plugin"])
	}

	if stdin["command"] != "link" {
		t.Errorf("command field: %v", stdin["command"])
	}

	if _, present := stdin["plugin_dir"]; !present {
		t.Errorf("plugin_dir should be present for plugins that read bundled files")
	}

	// from/to fields no longer exist — plugin parses os.Args[2:].
	if _, present := stdin["from"]; present {
		t.Error("from field should not be in passthrough stdin (plugin parses args itself)")
	}
}

// TestPluginDispatch_ConfigUnset confirms the inverse action
// type lands too. Important because `redis:unlink` will use it
// to clear REDIS_URL from a consumer.
func TestPluginDispatch_ConfigUnset(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "message": "unlinked",
    "actions": [
      {"type": "config_unset", "scope": "clowk-lp", "name": "web", "keys": ["REDIS_URL"]}
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "redis", "unlink", script)

	srv, store := dispatchTestServer(t, root)
	defer srv.Close()

	_ = store.PatchConfig(context.Background(), "clowk-lp", "web", map[string]string{
		"REDIS_URL": "redis://old",
		"OTHER":     "keep-me",
	})

	resp, err := http.Post(srv.URL+"/plugin/redis/unlink", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	cfg, _ := store.GetConfig(context.Background(), "clowk-lp", "web")

	if _, present := cfg["REDIS_URL"]; present {
		t.Error("REDIS_URL should have been unset")
	}

	if cfg["OTHER"] != "keep-me" {
		t.Errorf("OTHER should be preserved: %q", cfg["OTHER"])
	}
}

// TestPluginDispatch_ApplyManifest pins the apply_manifest action:
// plugin emits a manifest spec inline, controller persists it via
// Store.Put. Same write path as `vd apply`, so the watch loop's
// reconcile handlers pick up the new resource on the next tick.
//
// Used (in production) by plugins that materialise runtime
// resources — e.g. `vd pg:backups:capture` emitting a job manifest
// that voodu's job runner spawns as a sibling container.
func TestPluginDispatch_ApplyManifest(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "message": "backup b008 capturing",
    "actions": [
      {
        "type": "apply_manifest",
        "scope": "clowk-lp",
        "name": "db",
        "manifest": {
          "kind": "job",
          "scope": "clowk-lp",
          "name": "db-backup-b008",
          "spec": {
            "image": "postgres:16",
            "command": ["bash", "-c", "pg_dump ..."],
            "volumes": ["/opt/voodu/backups/clowk-lp/db:/backups:rw"]
          }
        }
      }
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "postgres", "backups:capture", script)

	srv, store := dispatchTestServer(t, root)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/plugin/postgres/backups:capture", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	var out struct {
		Data struct {
			Message string   `json:"message"`
			Applied []string `json:"applied"`
		} `json:"data"`
	}

	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}

	// Applied summary should mention the manifest's kind/scope/name.
	if len(out.Data.Applied) != 1 || !strings.Contains(out.Data.Applied[0], "job/clowk-lp/db-backup-b008") {
		t.Errorf("applied: %v", out.Data.Applied)
	}

	// Store reflects the new manifest.
	got, err := store.Get(context.Background(), KindJob, "clowk-lp", "db-backup-b008")
	if err != nil {
		t.Fatal(err)
	}

	if got == nil {
		t.Fatal("manifest was not persisted")
	}

	if got.Kind != KindJob {
		t.Errorf("kind: got %q, want job", got.Kind)
	}

	// Spec round-trips as JSON; check one field landed.
	var spec map[string]any
	if err := json.Unmarshal(got.Spec, &spec); err != nil {
		t.Fatalf("spec decode: %v", err)
	}

	if spec["image"] != "postgres:16" {
		t.Errorf("spec image: got %v", spec["image"])
	}
}

// TestPluginDispatch_ApplyManifest_RejectsMissingPayload covers
// the validator path: action.Type=apply_manifest without an
// embedded manifest is a plugin authoring bug, must surface as
// a 500 with a clear error.
func TestPluginDispatch_ApplyManifest_RejectsMissingPayload(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "message": "buggy plugin emitted apply_manifest with no payload",
    "actions": [
      {"type": "apply_manifest", "scope": "x", "name": "y"}
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "demo", "buggy", script)

	srv, _ := dispatchTestServer(t, root)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/plugin/demo/buggy", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected error status, got 200 with body=%s", raw)
	}

	if !bytes.Contains(raw, []byte("manifest payload")) {
		t.Errorf("error should mention missing manifest, got: %s", raw)
	}
}

// TestPluginDispatch_ApplyManifest_RejectsInvalidManifest covers
// validation passthrough: malformed manifest (e.g. job without a
// scope, missing required field) surfaces Store.Put's error to
// the operator. Plugin authors get loud feedback for invalid
// generated manifests.
func TestPluginDispatch_ApplyManifest_RejectsInvalidManifest(t *testing.T) {
	root := t.TempDir()

	// Job kind requires scope (it's in ScopedKinds). Empty scope
	// should fail Store.Put -> Manifest.Validate().
	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "message": "buggy: job without scope",
    "actions": [
      {
        "type": "apply_manifest",
        "scope": "x",
        "name": "y",
        "manifest": {"kind": "job", "name": "no-scope-job", "spec": {}}
      }
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "demo", "bad-job", script)

	srv, _ := dispatchTestServer(t, root)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/plugin/demo/bad-job", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected error status for unscoped job, got 200")
	}
}

// TestPluginDispatch_DeleteManifest pins the delete_manifest
// action: plugin asks the controller to remove a manifest by
// (kind, scope, name). Used by `vd pg:backups:cancel` (eventually)
// to remove an in-flight job, triggering the watch loop's remove
// path which tears down the docker container.
func TestPluginDispatch_DeleteManifest(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "message": "backup b008 cancelled",
    "actions": [
      {
        "type": "delete_manifest",
        "scope": "clowk-lp",
        "name": "db-backup-b008",
        "kind": "job"
      }
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "postgres", "backups:cancel", script)

	srv, store := dispatchTestServer(t, root)
	defer srv.Close()

	// Pre-seed the manifest the plugin will ask us to delete.
	_, _ = store.Put(context.Background(), &Manifest{
		Kind:  KindJob,
		Scope: "clowk-lp",
		Name:  "db-backup-b008",
		Spec:  json.RawMessage(`{"image":"postgres:16"}`),
	})

	resp, err := http.Post(srv.URL+"/plugin/postgres/backups:cancel", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	// Manifest should be gone.
	got, _ := store.Get(context.Background(), KindJob, "clowk-lp", "db-backup-b008")
	if got != nil {
		t.Errorf("manifest still present after delete: %+v", got)
	}
}

// TestPluginDispatch_DeleteManifest_NoOpOnMissing pins the
// idempotent semantics: deleting a non-existent manifest is not
// an error. Mirrors `vd delete` behaviour so action queues can
// re-run safely after partial failure.
func TestPluginDispatch_DeleteManifest_NoOpOnMissing(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "message": "delete (idempotent)",
    "actions": [
      {"type": "delete_manifest", "scope": "x", "name": "ghost", "kind": "job"}
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "demo", "delete-missing", script)

	srv, _ := dispatchTestServer(t, root)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/plugin/demo/delete-missing", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 (idempotent), got %d: %s", resp.StatusCode, raw)
	}

	var out struct {
		Data struct {
			Applied []string `json:"applied"`
		} `json:"data"`
	}

	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)

	if len(out.Data.Applied) != 1 || !strings.Contains(out.Data.Applied[0], "no-op") {
		t.Errorf("applied should mention no-op: %v", out.Data.Applied)
	}
}

// TestPluginDispatch_DeleteManifest_RejectsMissingKind covers the
// validator path. Scope/Name without Kind is ambiguous (could
// match any kind at that name) — must be rejected.
func TestPluginDispatch_DeleteManifest_RejectsMissingKind(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "message": "buggy: delete without kind",
    "actions": [
      {"type": "delete_manifest", "scope": "x", "name": "y"}
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "demo", "delete-no-kind", script)

	srv, _ := dispatchTestServer(t, root)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/plugin/demo/delete-no-kind", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatal("expected error status, got 200")
	}

	raw, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(raw, []byte("requires kind")) {
		t.Errorf("error should mention 'requires kind', got: %s", raw)
	}
}

// TestPluginDispatch_ApplyManifest_DoesNotFanOutRestart pins the
// restart-fanout suppression: applying a manifest is reconciled
// via the watch loop's WatchPut → kind handler path, NOT via
// restarting (action.Scope, action.Name). Without this guard, a
// plugin emitting `apply_manifest{scope: postgres-owner}` would
// roll the postgres pod on every backup capture — wrong.
//
// We assert the suppression by writing a deployment manifest at
// (action.Scope, action.Name), then applying an apply_manifest
// action with the same scope/name: the deployment's revision
// should NOT change (no restart-induced re-Put), confirming the
// fanout was skipped.
func TestPluginDispatch_ApplyManifest_DoesNotFanOutRestart(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "message": "backup",
    "actions": [
      {
        "type": "apply_manifest",
        "scope": "clowk-lp",
        "name": "db",
        "manifest": {
          "kind": "job",
          "scope": "clowk-lp",
          "name": "db-backup-b008",
          "spec": {"image": "postgres:16"}
        }
      }
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "postgres", "backups:capture", script)

	srv, store := dispatchTestServer(t, root)
	defer srv.Close()

	// Pre-seed the "owner context" manifest the action.Scope/Name
	// points to. If restart fanout fired, this manifest's revision
	// would bump.
	put, _ := store.Put(context.Background(), &Manifest{
		Kind:  KindStatefulset,
		Scope: "clowk-lp",
		Name:  "db",
		Spec:  json.RawMessage(`{"image":"postgres:16"}`),
	})
	revBefore := put.Metadata.Revision

	resp, err := http.Post(srv.URL+"/plugin/postgres/backups:capture", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	got, _ := store.Get(context.Background(), KindStatefulset, "clowk-lp", "db")

	if got == nil {
		t.Fatal("postgres manifest disappeared")
	}

	if got.Metadata.Revision != revBefore {
		t.Errorf("apply_manifest must NOT bump owner revision (restart suppression): before=%d, after=%d",
			revBefore, got.Metadata.Revision)
	}
}

// TestPluginDispatch_RunJob_QueuesAsyncRun pins the run_job
// action: plugin asks the controller to start a registered job;
// controller spawns a goroutine that calls Jobs.RunOnce. The
// dispatch response returns "queued" instantly — operator is NOT
// blocked on the run's duration.
//
// Used (in production) by `vd pg:backups:capture` to chain
// apply_manifest + run_job: register a one-shot backup job, then
// kick it off, all in one operator command.
func TestPluginDispatch_RunJob_QueuesAsyncRun(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "message": "backup b008 capturing",
    "actions": [
      {"type": "run_job", "scope": "clowk-lp", "name": "db-backup-b008"}
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "postgres", "backups:capture", script)

	store := newMemStore()
	runner := &fakeRunner{run: JobRun{RunID: "run-xyz", Status: JobStatusSucceeded}}

	api := &API{
		Store:       store,
		PluginsRoot: root,
		Jobs:        runner,
	}

	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/plugin/postgres/backups:capture", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	var out struct {
		Data struct {
			Applied []string `json:"applied"`
		} `json:"data"`
	}

	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)

	if len(out.Data.Applied) != 1 || !strings.Contains(out.Data.Applied[0], "queued") {
		t.Errorf("expected 'queued' summary, got: %v", out.Data.Applied)
	}

	// Goroutine ran in the background; runner saw the call. Use
	// Eventually-style poll because the goroutine fires off the
	// HTTP-handler thread.
	deadline := 0
	for runner.gotScope == "" && deadline < 50 {
		// busy-wait briefly — fakeRunner.RunOnce returns instantly,
		// but goroutine scheduling has a few microseconds of slop.
		deadline++
	}

	if runner.gotScope != "clowk-lp" || runner.gotName != "db-backup-b008" {
		t.Errorf("runner called with scope=%q name=%q, want clowk-lp/db-backup-b008",
			runner.gotScope, runner.gotName)
	}
}

// TestPluginDispatch_RunJob_NoRunnerErrors covers the safety
// path: a controller without a JobRunner configured (e.g. during
// bring-up of a new node) must fail loudly instead of silently
// no-op'ing the run.
func TestPluginDispatch_RunJob_NoRunnerErrors(t *testing.T) {
	root := t.TempDir()

	script := `#!/bin/sh
cat > /dev/null
cat <<'EOF'
{
  "status": "ok",
  "data": {
    "message": "queued",
    "actions": [
      {"type": "run_job", "scope": "x", "name": "y"}
    ]
  }
}
EOF
`

	writePluginYAML(t, root, "demo", "run-no-runner", script)

	// API without Jobs configured.
	api := &API{Store: newMemStore(), PluginsRoot: root}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/plugin/demo/run-no-runner", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected error status with no JobRunner, got 200: %s", raw)
	}

	if !bytes.Contains(raw, []byte("no job runner")) {
		t.Errorf("error should mention missing runner, got: %s", raw)
	}
}
