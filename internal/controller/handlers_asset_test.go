package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
