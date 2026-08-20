package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/paths"
)

// TestRegistryHandler_WritesConfigJSON pins the happy path: one
// registry manifest in the store produces one entry under `auths`
// in the rendered config.json, keyed by URL, with `auth` set to
// base64(username:token). This is the contract every `docker pull`
// against a private registry depends on — break it and image
// pulls fail with "unauthorized" on the first reconcile.
func TestRegistryHandler_WritesConfigJSON(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	store := newMemStore()

	h := &RegistryHandler{
		Store:            store,
		Log:              quietLogger(),
		DockerConfigPath: configPath,
	}

	spec := registrySpec{URL: "ghcr.io", Username: "thadeu", Token: "ghp_secret"}
	specJSON, _ := json.Marshal(spec)

	m := &Manifest{
		Kind: KindRegistry,
		Name: "ghcr",
		Spec: specJSON,
	}

	if _, err := store.Put(context.Background(), m); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ev := WatchEvent{Type: WatchPut, Kind: KindRegistry, Name: "ghcr", Manifest: m}

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}

	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, raw)
	}

	got, ok := cfg.Auths["ghcr.io"]
	if !ok {
		t.Fatalf("missing auths[ghcr.io] in %s", raw)
	}

	wantAuth := base64.StdEncoding.EncodeToString([]byte("thadeu:ghp_secret"))
	if got.Auth != wantAuth {
		t.Errorf("auths[ghcr.io].auth = %q, want %q (base64 of `username:token`)", got.Auth, wantAuth)
	}

	// Perm check: docker login writes 0600 to keep credentials
	// out of world-readable land. We must match.
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0600 {
		t.Errorf("config.json mode = %o, want 0600 (matches docker login posture)", info.Mode().Perm())
	}
}

// TestRegistryHandler_MergesMultiple verifies that two registry
// manifests both land in the same config.json under distinct
// `auths` keys — the canonical "ghcr + dockerhub" mixed shape.
// A regression where the second apply clobbers the first would
// surface here as a missing key in the rendered config.
func TestRegistryHandler_MergesMultiple(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	store := newMemStore()

	h := &RegistryHandler{
		Store:            store,
		Log:              quietLogger(),
		DockerConfigPath: configPath,
	}

	type reg struct {
		name, url, user, token string
	}

	regs := []reg{
		{name: "ghcr", url: "ghcr.io", user: "alice", token: "ght_1"},
		{name: "dockerhub", url: "registry-1.docker.io", user: "bob", token: "dckr_2"},
	}

	for _, r := range regs {
		specJSON, _ := json.Marshal(registrySpec{URL: r.url, Username: r.user, Token: r.token})

		m := &Manifest{Kind: KindRegistry, Name: r.name, Spec: specJSON}
		if _, err := store.Put(context.Background(), m); err != nil {
			t.Fatalf("seed %s: %v", r.name, err)
		}

		ev := WatchEvent{Type: WatchPut, Kind: KindRegistry, Name: r.name, Manifest: m}
		if err := h.Handle(context.Background(), ev); err != nil {
			t.Fatalf("Handle %s: %v", r.name, err)
		}
	}

	raw, _ := os.ReadFile(configPath)

	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}

	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(cfg.Auths) != 2 {
		t.Errorf("auths count = %d, want 2 (one per registry manifest)", len(cfg.Auths))
	}

	for _, r := range regs {
		got, ok := cfg.Auths[r.url]
		if !ok {
			t.Errorf("missing auths[%s]", r.url)
			continue
		}

		want := base64.StdEncoding.EncodeToString([]byte(r.user + ":" + r.token))
		if got.Auth != want {
			t.Errorf("auths[%s].auth = %q, want %q", r.url, got.Auth, want)
		}
	}
}

// TestRegistryHandler_RemovesOnDelete pins the delete path: when
// the last registry manifest goes away, its URL key disappears
// from config.json. The handler ALWAYS regenerates from the
// store's current List — so deleted-from-store means
// absent-from-file on the next reconcile. Without this, a
// removed pull-secret would linger on disk forever, and an
// operator rotating credentials by deleting + re-adding would
// silently keep the old creds.
func TestRegistryHandler_RemovesOnDelete(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	store := newMemStore()

	h := &RegistryHandler{
		Store:            store,
		Log:              quietLogger(),
		DockerConfigPath: configPath,
	}

	specJSON, _ := json.Marshal(registrySpec{URL: "ghcr.io", Username: "alice", Token: "x"})
	m := &Manifest{Kind: KindRegistry, Name: "ghcr", Spec: specJSON}

	if _, err := store.Put(context.Background(), m); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := h.Handle(context.Background(), WatchEvent{Type: WatchPut, Kind: KindRegistry, Name: "ghcr", Manifest: m}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Sanity: it's there before delete.
	raw, _ := os.ReadFile(configPath)
	if !strings.Contains(string(raw), "ghcr.io") {
		t.Fatalf("pre-delete: expected ghcr.io in config, got %s", raw)
	}

	if _, err := store.Delete(context.Background(), KindRegistry, "", "ghcr"); err != nil {
		t.Fatalf("delete store: %v", err)
	}

	if err := h.Handle(context.Background(), WatchEvent{Type: WatchDelete, Kind: KindRegistry, Name: "ghcr"}); err != nil {
		t.Fatalf("delete handle: %v", err)
	}

	raw, _ = os.ReadFile(configPath)

	var cfg struct {
		Auths map[string]any `json:"auths"`
	}

	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("post-delete unmarshal: %v", err)
	}

	if len(cfg.Auths) != 0 {
		t.Errorf("post-delete auths = %v, want empty (only registry was removed)", cfg.Auths)
	}
}

// TestRegistryHandler_AtomicWrite pins the no-partial-state
// guarantee: while a regenerate is in progress, an external
// reader of config.json must NEVER see a half-written file.
// The contract is "either old contents or new contents, never
// a mix" — concurrent docker processes pull the file on every
// `docker pull`, and a partial parse would surface as flaky
// "unauthorized" or "invalid character" errors.
//
// We can't easily race the writer mid-flight in a unit test,
// so instead we verify the implementation strategy: after
// regenerate runs, no `.tmp-*` sibling file should remain in
// the parent directory (atomicity hinges on the temp+rename
// pattern; a left-behind temp file is the loud signal that
// the implementation drifted to a different strategy).
func TestRegistryHandler_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	store := newMemStore()

	h := &RegistryHandler{
		Store:            store,
		Log:              quietLogger(),
		DockerConfigPath: configPath,
	}

	specJSON, _ := json.Marshal(registrySpec{URL: "ghcr.io", Username: "u", Token: "t"})
	m := &Manifest{Kind: KindRegistry, Name: "ghcr", Spec: specJSON}

	if _, err := store.Put(context.Background(), m); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := h.Handle(context.Background(), WatchEvent{Type: WatchPut, Kind: KindRegistry, Name: "ghcr", Manifest: m}); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		name := e.Name()

		if strings.Contains(name, ".tmp") || strings.HasSuffix(name, "~") {
			t.Errorf("leftover temp/swap file %q in %s — atomic write must clean up after itself", name, dir)
		}
	}

	// And the final file must still be valid JSON with the expected entry.
	raw, _ := os.ReadFile(configPath)

	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}

	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("final config is not valid JSON: %v (raw=%s)", err, raw)
	}

	if _, ok := cfg.Auths["ghcr.io"]; !ok {
		t.Errorf("after 3 reconciles, ghcr.io entry missing from final state")
	}
}

// TestRegistryHandler_CreatesParentDir verifies the first-apply
// path on a host where `~/.docker/` doesn't exist yet — the
// handler must MkdirAll the parent so the rename target has a
// directory to land in. A regression here surfaces as "no such
// file or directory" on a fresh box's first `vd apply` with a
// registry block.
func TestRegistryHandler_CreatesParentDir(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "newly-created", ".docker", "config.json")

	store := newMemStore()

	h := &RegistryHandler{
		Store:            store,
		Log:              quietLogger(),
		DockerConfigPath: configPath,
	}

	specJSON, _ := json.Marshal(registrySpec{URL: "ghcr.io", Username: "u", Token: "t"})
	m := &Manifest{Kind: KindRegistry, Name: "ghcr", Spec: specJSON}

	if _, err := store.Put(context.Background(), m); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := h.Handle(context.Background(), WatchEvent{Type: WatchPut, Kind: KindRegistry, Name: "ghcr", Manifest: m}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config.json not created under fresh parent: %v", err)
	}
}

// TestRegistryHandlerDefaultsToVooduRoot pins the path contract. The
// handler and docker.UseVooduDockerConfig() must resolve to the SAME
// file: the handler writes the auths, DOCKER_CONFIG tells the docker
// CLI where to read them. If these two ever diverge the symptom is
// silent — every `registry {}` reconciles green while every private
// pull fails with "no basic auth credentials".
//
// It must NOT be $HOME/.docker/config.json. The controller runs as
// root under a unit with ProtectHome=yes, so /root is empty and
// unwritable for the whole service cgroup.
func TestRegistryHandlerDefaultsToVooduRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)
	t.Setenv("HOME", "/root")

	h := &RegistryHandler{}

	want := filepath.Join(root, "docker", "config.json")
	if got := h.ensureConfigPath(); got != want {
		t.Errorf("ensureConfigPath() = %q, want %q", got, want)
	}

	if got := h.ensureConfigPath(); got != paths.DockerConfigFile() {
		t.Errorf("ensureConfigPath() = %q, drifted from paths.DockerConfigFile() = %q", got, paths.DockerConfigFile())
	}
}

// TestRegistryHandlerExplicitPathWins keeps the test seam intact —
// production leaves DockerConfigPath empty, tests inject a tempdir.
func TestRegistryHandlerExplicitPathWins(t *testing.T) {
	t.Setenv(paths.EnvRoot, t.TempDir())

	explicit := filepath.Join(t.TempDir(), "injected.json")
	h := &RegistryHandler{DockerConfigPath: explicit}

	if got := h.ensureConfigPath(); got != explicit {
		t.Errorf("ensureConfigPath() = %q, want the injected %q", got, explicit)
	}
}

// seedRegistry puts one registry manifest and reconciles it, returning
// the raw config.json the handler produced.
func seedRegistry(t *testing.T, configPath string, specs map[string]registrySpec) map[string]json.RawMessage {
	t.Helper()

	store := newMemStore()

	h := &RegistryHandler{Store: store, Log: quietLogger(), DockerConfigPath: configPath}

	var last string

	for name, spec := range specs {
		blob, _ := json.Marshal(spec)

		m := &Manifest{Kind: KindRegistry, Name: name, Spec: blob}
		if _, err := store.Put(context.Background(), m); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}

		last = name
	}

	ev := WatchEvent{Type: WatchPut, Kind: KindRegistry, Name: last}
	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("decode config: %v\n%s", err, raw)
	}

	return top
}

// TestRegistryHandler_HelperModeWritesCredHelpers is the EC2
// instance-role path: no credential in the manifest at all, docker
// execs the helper and it resolves the host's own identity.
func TestRegistryHandler_HelperModeWritesCredHelpers(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")

	top := seedRegistry(t, configPath, map[string]registrySpec{
		"ecr": {URL: "889332165767.dkr.ecr.sa-east-1.amazonaws.com", Helper: "ecr-login"},
	})

	var helpers map[string]string
	if err := json.Unmarshal(top["credHelpers"], &helpers); err != nil {
		t.Fatalf("decode credHelpers: %v (config: %v)", err, top)
	}

	if got := helpers["889332165767.dkr.ecr.sa-east-1.amazonaws.com"]; got != "ecr-login" {
		t.Errorf("credHelpers entry = %q, want ecr-login", got)
	}

	// No auths entry — there is no credential to encode, and an empty
	// one would shadow nothing but confuse anyone reading the file.
	var auths map[string]any
	_ = json.Unmarshal(top["auths"], &auths)

	if len(auths) != 0 {
		t.Errorf("auths = %v, want empty in helper mode", auths)
	}
}

// TestRegistryHandler_MixedHelperAndToken — a host can pull ECR off
// its instance role AND ghcr.io with a bot token. The two sections
// must not interfere.
func TestRegistryHandler_MixedHelperAndToken(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")

	top := seedRegistry(t, configPath, map[string]registrySpec{
		"ecr":  {URL: "123.dkr.ecr.sa-east-1.amazonaws.com", Helper: "ecr-login"},
		"ghcr": {URL: "ghcr.io", Username: "bot", Token: "ghp_x"},
	})

	var helpers map[string]string
	_ = json.Unmarshal(top["credHelpers"], &helpers)

	if len(helpers) != 1 || helpers["123.dkr.ecr.sa-east-1.amazonaws.com"] != "ecr-login" {
		t.Errorf("credHelpers = %v, want exactly the ECR entry", helpers)
	}

	var auths map[string]dockerAuth
	_ = json.Unmarshal(top["auths"], &auths)

	if len(auths) != 1 || auths["ghcr.io"].Auth == "" {
		t.Errorf("auths = %v, want exactly the ghcr entry", auths)
	}
}

// TestRegistryHandler_CredHelpersOmittedWhenEmpty keeps the file clean
// on the overwhelmingly common host that uses no helper at all.
func TestRegistryHandler_CredHelpersOmittedWhenEmpty(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")

	top := seedRegistry(t, configPath, map[string]registrySpec{
		"ghcr": {URL: "ghcr.io", Username: "bot", Token: "ghp_x"},
	})

	if _, present := top["credHelpers"]; present {
		t.Errorf("credHelpers emitted with no helper-mode registry: %s", top["credHelpers"])
	}
}

// TestRegistryHandler_CredHelpersIsOwned — the ownership contract.
// A helper entry voodu did not declare is removed, exactly as an
// undeclared auths entry is. Asymmetric ownership between the two
// sibling keys would be impossible to reason about.
func TestRegistryHandler_CredHelpersIsOwned(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")

	stale := `{"credHelpers":{"stale.example.com":"someone-elses-helper"},"auths":{}}`
	if err := os.WriteFile(configPath, []byte(stale), 0600); err != nil {
		t.Fatal(err)
	}

	top := seedRegistry(t, configPath, map[string]registrySpec{
		"ecr": {URL: "123.dkr.ecr.sa-east-1.amazonaws.com", Helper: "ecr-login"},
	})

	var helpers map[string]string
	_ = json.Unmarshal(top["credHelpers"], &helpers)

	if _, survived := helpers["stale.example.com"]; survived {
		t.Errorf("undeclared helper survived the reconcile: %v", helpers)
	}
}

// TestRegistryHandler_PreservesUnknownTopLevelKeys — voodu claims
// auths and credHelpers, nothing else. An operator's credsStore or
// HTTPHeaders must round-trip.
func TestRegistryHandler_PreservesUnknownTopLevelKeys(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")

	existing := `{"auths":{},"credsStore":"osxkeychain","HTTPHeaders":{"X-Foo":"bar"}}`
	if err := os.WriteFile(configPath, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	top := seedRegistry(t, configPath, map[string]registrySpec{
		"ecr": {URL: "123.dkr.ecr.sa-east-1.amazonaws.com", Helper: "ecr-login"},
	})

	if string(top["credsStore"]) != `"osxkeychain"` {
		t.Errorf("credsStore = %s, want it preserved", top["credsStore"])
	}

	if _, ok := top["HTTPHeaders"]; !ok {
		t.Errorf("HTTPHeaders dropped: %v", top)
	}
}
