package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
