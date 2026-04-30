package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/paths"
)

// TestAssetHandler_MaterialisesAllSources is the canonical
// happy path: an asset block with one entry per source kind
// (inline, file, url) lands three sibling files on disk with
// the right content.
//
// VOODU_ROOT is pointed at a temp dir so the materialiser
// writes under the test sandbox; cleanup happens via t.TempDir.
func TestAssetHandler_MaterialisesAllSources(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)

	urlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# users.acl from r2"))
	}))
	defer urlServer.Close()

	store := newMemStore()

	h := &AssetHandler{
		Store: store,
		Log:   quietLogger(),
	}

	spec := map[string]any{
		"files": map[string]any{
			"motd": "Welcome",
			"configuration": map[string]any{
				"_source":  "file",
				"content":  base64.StdEncoding.EncodeToString([]byte("maxmemory 256mb\n")),
				"filename": "redis.conf",
			},
			"acls": map[string]any{
				"_source": "url",
				"url":     urlServer.URL,
			},
		},
	}

	specJSON, _ := json.Marshal(spec)

	ev := WatchEvent{
		Type: WatchPut,
		Kind: KindAsset,
		Scope: "data", Name: "redis",
		Manifest: &Manifest{
			Kind:  KindAsset,
			Scope: "data",
			Name:  "redis",
			Spec:  specJSON,
		},
	}

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	cases := map[string]string{
		"motd":          "Welcome",
		"configuration": "maxmemory 256mb\n",
		"acls":          "# users.acl from r2",
	}

	for key, want := range cases {
		path := paths.AssetFile("data", "redis", key)

		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}

		if string(got) != want {
			t.Errorf("%s content = %q, want %q", key, got, want)
		}

		// Mode must be world-readable. Non-root containers
		// (redis as UID 999, postgres as UID 70, nginx as
		// UID 101) bind-mount these files :ro and need read
		// access — a 0600 here would fail at boot with
		// "Permission denied" inside the container.
		info, err := os.Stat(path)
		if err == nil && info.Mode().Perm() != 0644 {
			t.Errorf("%s mode = %o, want 0644 (world-readable)", key, info.Mode().Perm())
		}
	}

	// Status carries digests for each key so consumers fold
	// them into spec hashes. Empty status would defeat
	// drift-triggered rolling restarts.
	statusBlob, _ := store.GetStatus(context.Background(), KindAsset, "data-redis")

	var st AssetStatus
	if err := json.Unmarshal(statusBlob, &st); err != nil {
		t.Fatal(err)
	}

	for key := range cases {
		if st.Files[key] == "" {
			t.Errorf("status missing digest for %q", key)
		}
	}
}

// TestAssetHandler_SweepsStaleKeys: a re-apply that drops a
// key removes the corresponding on-disk file. Without the
// sweep, renamed keys would leave the old file mounted on
// running containers indefinitely.
func TestAssetHandler_SweepsStaleKeys(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)

	store := newMemStore()
	h := &AssetHandler{Store: store, Log: quietLogger()}

	apply := func(files map[string]any) {
		spec, _ := json.Marshal(map[string]any{"files": files})

		ev := WatchEvent{
			Type: WatchPut,
			Kind: KindAsset,
			Scope: "data", Name: "redis",
			Manifest: &Manifest{
				Kind:  KindAsset,
				Scope: "data",
				Name:  "redis",
				Spec:  spec,
			},
		}

		if err := h.Handle(context.Background(), ev); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}

	// Apply 1: two keys.
	apply(map[string]any{
		"keep":   "stays",
		"remove": "goes",
	})

	if _, err := os.Stat(paths.AssetFile("data", "redis", "remove")); err != nil {
		t.Fatalf("apply 1 didn't create 'remove': %v", err)
	}

	// Apply 2: only `keep` survives. `remove` should be
	// swept off disk.
	apply(map[string]any{
		"keep": "stays again",
	})

	if _, err := os.Stat(paths.AssetFile("data", "redis", "remove")); err == nil {
		t.Errorf("stale key 'remove' still on disk after re-apply")
	}

	if got, _ := os.ReadFile(paths.AssetFile("data", "redis", "keep")); string(got) != "stays again" {
		t.Errorf("'keep' content not refreshed: %q", got)
	}
}

// TestAssetHandler_DeleteRemovesEverything: WatchDelete drops
// the asset directory and clears /status. Resources still
// referencing the asset will fail loudly on next reconcile,
// which is the intended signal to the operator.
func TestAssetHandler_DeleteRemovesEverything(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)

	store := newMemStore()
	h := &AssetHandler{Store: store, Log: quietLogger()}

	specJSON, _ := json.Marshal(map[string]any{
		"files": map[string]any{"k": "v"},
	})

	put := WatchEvent{
		Type: WatchPut, Kind: KindAsset, Scope: "data", Name: "x",
		Manifest: &Manifest{Kind: KindAsset, Scope: "data", Name: "x", Spec: specJSON},
	}

	if err := h.Handle(context.Background(), put); err != nil {
		t.Fatal(err)
	}

	del := WatchEvent{
		Type: WatchDelete, Kind: KindAsset, Scope: "data", Name: "x",
	}

	if err := h.Handle(context.Background(), del); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(paths.AssetDir("data", "x")); !os.IsNotExist(err) {
		t.Errorf("asset dir still present after delete: %v", err)
	}

	if blob, _ := store.GetStatus(context.Background(), KindAsset, "data-x"); blob != nil {
		t.Errorf("status not cleared after delete")
	}
}

// TestAssetHandler_RejectsInvalidKey: keys with dots /
// whitespace / slashes break ${asset.X.Y} parsing and
// filesystem assumptions. Validate at apply time so the
// operator sees a clear error before the manifest lands in
// production.
func TestAssetHandler_RejectsInvalidKey(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)

	store := newMemStore()
	h := &AssetHandler{Store: store, Log: quietLogger()}

	cases := []string{
		"redis.conf",   // dot — would shadow the var separator
		"with space",   // whitespace — illegal filename chars on some FS
		"path/sub",     // slash — escapes the asset dir
		"",             // empty — meaningless
	}

	for _, key := range cases {
		spec, _ := json.Marshal(map[string]any{
			"files": map[string]any{key: "x"},
		})

		ev := WatchEvent{
			Type: WatchPut, Kind: KindAsset,
			Scope: "data", Name: "test",
			Manifest: &Manifest{Kind: KindAsset, Scope: "data", Name: "test", Spec: spec},
		}

		if err := h.Handle(context.Background(), ev); err == nil {
			t.Errorf("accepted invalid key %q", key)
		}
	}
}

// TestAssetHandler_PartialFailureKeepsSuccesses pins the
// best-effort contract: a single failing source (unreachable
// URL, decode error) doesn't abort the rest of the bundle.
// Successful keys land on disk; failing keys land in
// /status as Errors. Critical for prod where dozens of
// assets share a controller — one flaky URL must not
// derail every other apply.
func TestAssetHandler_PartialFailureKeepsSuccesses(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)

	// Server that ALWAYS returns 500 — exercises the URL
	// failure path without depending on a specific port
	// being closed on the test runner.
	failingURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failingURL.Close()

	store := newMemStore()
	h := &AssetHandler{Store: store, Log: quietLogger()}

	spec, _ := json.Marshal(map[string]any{
		"files": map[string]any{
			"good": "the good one (inline source, always succeeds)",
			"bad": map[string]any{
				"_source": "url",
				"url":     failingURL.URL,
			},
		},
	})

	ev := WatchEvent{
		Type: WatchPut, Kind: KindAsset,
		Scope: "data", Name: "mixed",
		Manifest: &Manifest{Kind: KindAsset, Scope: "data", Name: "mixed", Spec: spec},
	}

	// Best-effort: 1 succeeded, 1 failed → reconcile returns
	// success (not Transient — partial state is durable).
	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("partial failure should not return error: %v", err)
	}

	// Successful key landed on disk.
	good, err := os.ReadFile(paths.AssetFile("data", "mixed", "good"))
	if err != nil {
		t.Errorf("good key not materialised: %v", err)
	}

	if !strings.Contains(string(good), "the good one") {
		t.Errorf("good key content wrong: %q", good)
	}

	// Failed key NOT on disk (no previous apply succeeded
	// for it; nothing to preserve).
	if _, err := os.Stat(paths.AssetFile("data", "mixed", "bad")); !os.IsNotExist(err) {
		t.Errorf("bad key should not be on disk on first failure: %v", err)
	}

	// Status reflects both: Files has good, Errors has bad.
	statusBlob, _ := store.GetStatus(context.Background(), KindAsset, "data-mixed")

	var st AssetStatus
	if err := json.Unmarshal(statusBlob, &st); err != nil {
		t.Fatal(err)
	}

	if _, ok := st.Files["good"]; !ok {
		t.Errorf("Files map missing 'good': %+v", st.Files)
	}

	if _, ok := st.Files["bad"]; ok {
		t.Errorf("Files map should NOT carry failed keys: %+v", st.Files)
	}

	if msg := st.Errors["bad"]; msg == "" {
		t.Errorf("Errors map missing 'bad' key error message")
	}

	if _, ok := st.Errors["good"]; ok {
		t.Errorf("Errors map should not include successful keys: %+v", st.Errors)
	}
}

// TestAssetHandler_PartialFailurePreservesStaleSuccess: when
// a key SUCCEEDED on a previous apply and FAILS on a later
// one (URL flaky), the file from the last successful apply
// stays on disk. Consumers don't break suddenly — they keep
// the previous content until the source recovers.
//
// The Errors map flags the key as "broken right now"
// regardless of disk state, so operators see the issue
// without having to diff content themselves.
func TestAssetHandler_PartialFailurePreservesStaleSuccess(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)

	// First-pass server returns content; subsequent calls 500.
	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte("first-good-content"))
			return
		}

		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := newMemStore()
	h := &AssetHandler{Store: store, Log: quietLogger()}

	specJSON, _ := json.Marshal(map[string]any{
		"files": map[string]any{
			"acl": map[string]any{
				"_source": "url",
				"url":     srv.URL,
			},
		},
	})

	ev := WatchEvent{
		Type: WatchPut, Kind: KindAsset,
		Scope: "data", Name: "stale",
		Manifest: &Manifest{Kind: KindAsset, Scope: "data", Name: "stale", Spec: specJSON},
	}

	// Apply 1 — success.
	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("apply 1: %v", err)
	}

	got, _ := os.ReadFile(paths.AssetFile("data", "stale", "acl"))
	if string(got) != "first-good-content" {
		t.Fatalf("apply 1 content: %q", got)
	}

	// Wipe URL cache so apply 2 actually hits the server
	// (and gets the 500). Otherwise If-None-Match would
	// short-circuit to cached bytes.
	if err := os.RemoveAll(paths.CacheDir()); err != nil {
		t.Fatal(err)
	}

	// Apply 2 — fails. File on disk MUST stay.
	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("apply 2 should succeed (best-effort): %v", err)
	}

	got, err := os.ReadFile(paths.AssetFile("data", "stale", "acl"))
	if err != nil {
		t.Fatalf("stale file should still be on disk after failure: %v", err)
	}

	if string(got) != "first-good-content" {
		t.Errorf("stale content corrupted: %q", got)
	}

	// Status reflects the dual reality:
	//   - Files HAS the key with the stale-good digest, so
	//     consumer spec hashes (statefulset/deployment) stay
	//     stable instead of churning on every failed refresh
	//   - Errors flags the failure regardless, so operators
	//     see the issue even though disk has content
	statusBlob, _ := store.GetStatus(context.Background(), KindAsset, "data-stale")

	var st AssetStatus
	_ = json.Unmarshal(statusBlob, &st)

	if msg := st.Errors["acl"]; msg == "" {
		t.Error("Errors should reflect the recent failure even though disk has stale content")
	}

	if _, ok := st.Files["acl"]; !ok {
		t.Error("Files should include stale-recovered digest so consumers keep stable hashes")
	}
}

// TestAssetHandler_AllKeysFailReturnsTransient: when EVERY
// key fails (no successful materialisation, nothing to
// graceful-degrade to), the reconciler returns Transient so
// the watch retries. Without this, a totally-broken asset
// would silently sit in /status with all errors and consumers
// would mount empty paths forever.
func TestAssetHandler_AllKeysFailReturnsTransient(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := newMemStore()
	h := &AssetHandler{Store: store, Log: quietLogger()}

	spec, _ := json.Marshal(map[string]any{
		"files": map[string]any{
			"only_one": map[string]any{
				"_source": "url",
				"url":     srv.URL,
			},
		},
	})

	ev := WatchEvent{
		Type: WatchPut, Kind: KindAsset,
		Scope: "data", Name: "broken",
		Manifest: &Manifest{Kind: KindAsset, Scope: "data", Name: "broken", Spec: spec},
	}

	err := h.Handle(context.Background(), ev)
	if err == nil {
		t.Fatal("expected error when all keys fail")
	}

	if !isTransient(err) {
		t.Errorf("all-keys-failed error should be transient (so reconciler retries): %T %v", err, err)
	}
}

// TestAssetHandler_URLCacheRespects304: a second fetch with
// an unchanged URL response (server returns 304 with no body)
// reuses the cached bytes. Without this, every reconcile
// would re-download — fine for small configs but pricey for
// any non-trivial asset.
func TestAssetHandler_URLCacheRespects304(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)

	hits := 0
	body := []byte("v1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++

		// Honor If-None-Match: return 304 on the second
		// request to exercise the cache path.
		if r.Header.Get("If-None-Match") == "etag-1" {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("ETag", "etag-1")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	store := newMemStore()
	h := &AssetHandler{Store: store, Log: quietLogger()}

	specJSON, _ := json.Marshal(map[string]any{
		"files": map[string]any{
			"k": map[string]any{
				"_source": "url",
				"url":     srv.URL,
			},
		},
	})

	apply := func() {
		ev := WatchEvent{
			Type: WatchPut, Kind: KindAsset,
			Scope: "data", Name: "test",
			Manifest: &Manifest{Kind: KindAsset, Scope: "data", Name: "test", Spec: specJSON},
		}

		if err := h.Handle(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}

	apply() // hits=1, full GET
	apply() // hits=2, 304 → reuses cache

	if hits != 2 {
		t.Fatalf("expected 2 server hits (full + conditional), got %d", hits)
	}

	got, _ := os.ReadFile(paths.AssetFile("data", "test", "k"))
	if string(got) != "v1" {
		t.Errorf("cache content drift: %q", got)
	}

	// Cache files exist where expected.
	cacheKey := sha256OfString(srv.URL)

	for _, ext := range []string{".bytes", ".meta"} {
		path := filepath.Join(paths.CacheDir(), cacheKey+ext)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing cache file %s: %v", path, err)
		}
	}
}
