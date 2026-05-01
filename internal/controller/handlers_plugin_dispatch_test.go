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

	if !strings.Contains(string(raw), "does not declare command") {
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

// TestPluginDispatch_StdinCarriesContext: the plugin's stdin
// must contain the {plugin, command, from, to} envelope so it
// can act on real state. Confirmed by having the plugin write
// its stdin to a known file (avoids JSON-in-JSON escape pain
// of trying to inline stdin into a stdout envelope) and the
// test reads it back.
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

	_ = store.PatchConfig(context.Background(), "clowk-lp", "redis", map[string]string{"REDIS_PASSWORD": "s3cret"})

	body := bytes.NewBufferString(`{
		"from": {"kind": "statefulset", "scope": "clowk-lp", "name": "redis"},
		"to":   {"scope": "clowk-lp", "name": "web"}
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

	if stdin["plugin"] != "redis" {
		t.Errorf("plugin field: %v", stdin["plugin"])
	}

	if stdin["command"] != "link" {
		t.Errorf("command field: %v", stdin["command"])
	}

	from, _ := stdin["from"].(map[string]any)

	if from["kind"] != "statefulset" || from["scope"] != "clowk-lp" || from["name"] != "redis" {
		t.Errorf("from kind/scope/name wrong: %+v", from)
	}

	if spec, _ := from["spec"].(map[string]any); spec["image"] != "redis:8" {
		t.Errorf("from.spec.image: %v", spec)
	}

	if cfg, _ := from["config"].(map[string]any); cfg["REDIS_PASSWORD"] != "s3cret" {
		t.Errorf("from.config.REDIS_PASSWORD: %v", cfg)
	}

	to, _ := stdin["to"].(map[string]any)

	if to["scope"] != "clowk-lp" || to["name"] != "web" {
		t.Errorf("to scope/name wrong: %+v", to)
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
