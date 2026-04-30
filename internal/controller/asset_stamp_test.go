package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/paths"
)

// withTempVooduRoot points VOODU_ROOT at a fresh tempdir for the
// duration of the test. Stamping now materialises assets to disk
// at apply time, so tests can't rely on the default `/opt/voodu`
// location (no perms in CI, leaks across runs in dev). t.TempDir
// auto-cleans, env var restores via t.Cleanup.
func withTempVooduRoot(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	prev, had := os.LookupEnv(paths.EnvRoot)

	t.Setenv(paths.EnvRoot, dir)

	t.Cleanup(func() {
		if had {
			_ = os.Setenv(paths.EnvRoot, prev)
		} else {
			_ = os.Unsetenv(paths.EnvRoot)
		}
	})

	return dir
}

// helper: build an asset manifest with inline-string keys (simplest
// source type — bytes verbatim, no base64, no URL fetch). Each entry
// in `files` becomes a key=value pair on the asset spec.
func mkAssetManifest(scope, name string, files map[string]string) *Manifest {
	pairs := make(map[string]any, len(files))
	for k, v := range files {
		pairs[k] = v
	}

	spec, _ := json.Marshal(map[string]any{"files": pairs})

	return &Manifest{
		Kind:  KindAsset,
		Scope: scope,
		Name:  name,
		Spec:  spec,
	}
}

// helper: sha256 hex of an inline string. Mirrors what the stamping
// pipeline computes for inline sources.
func sumOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// helper: extract the stamped _asset_digests map from a consumer
// manifest's spec. Returns nil when the field is absent.
func stampedDigests(t *testing.T, m *Manifest) map[string]string {
	t.Helper()

	var spec map[string]any
	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}

	raw, ok := spec["_asset_digests"]
	if !ok {
		return nil
	}

	m2, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("_asset_digests is %T, want map", raw)
	}

	out := make(map[string]string, len(m2))
	for k, v := range m2 {
		s, _ := v.(string)
		out[k] = s
	}

	return out
}

// TestStampAssetDigests_SameBatch is the happy path: an asset and
// a consumer applied together, consumer references the asset
// textually via `${asset.…}` in volumes. Stamping must hash the
// asset's bytes and embed the digest under _asset_digests.
func TestStampAssetDigests_SameBatch(t *testing.T) {
	withTempVooduRoot(t)

	store := newMemStore()

	asset := mkAssetManifest("data", "redis", map[string]string{
		"cfg": "save 900 1\n",
	})

	consumerSpec, _ := json.Marshal(map[string]any{
		"image": "redis:7",
		"volumes": []string{
			"${asset.data.redis.cfg}:/etc/redis/redis.conf:ro",
		},
	})

	consumer := &Manifest{
		Kind:  KindStatefulset,
		Scope: "data",
		Name:  "redis",
		Spec:  consumerSpec,
	}

	manifests := []*Manifest{asset, consumer}

	if err := StampAssetDigests(context.Background(), store, nil, nil, manifests); err != nil {
		t.Fatalf("stamping: %v", err)
	}

	digests := stampedDigests(t, consumer)

	if want := sumOf("save 900 1\n"); digests["data.redis.cfg"] != want {
		t.Errorf("digest data.redis.cfg = %q, want %q", digests["data.redis.cfg"], want)
	}
}

// TestStampAssetDigests_CrossBatchStaleGood: the consumer references
// an asset that is NOT in this apply, but exists in /status from a
// prior reconcile. The stamping pipeline must pull the prior digest
// and stamp it, so the apply doesn't fail just because the asset is
// in a different file.
func TestStampAssetDigests_CrossBatchStaleGood(t *testing.T) {
	store := newMemStore()

	// Pre-seed /status as if a prior apply had reconciled the asset.
	priorStatus := AssetStatus{
		Files: map[string]string{
			"cfg": sumOf("prior-content"),
		},
	}
	blob, _ := json.Marshal(priorStatus)
	_ = store.PutStatus(context.Background(), KindAsset, AppID("data", "redis"), blob)

	consumerSpec, _ := json.Marshal(map[string]any{
		"image":   "redis:7",
		"volumes": []string{"${asset.data.redis.cfg}:/etc/redis/redis.conf:ro"},
	})

	consumer := &Manifest{
		Kind:  KindStatefulset,
		Scope: "data",
		Name:  "redis",
		Spec:  consumerSpec,
	}

	if err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{consumer}); err != nil {
		t.Fatalf("stamping: %v", err)
	}

	digests := stampedDigests(t, consumer)

	if got, want := digests["data.redis.cfg"], sumOf("prior-content"); got != want {
		t.Errorf("digest = %q, want %q (from /status)", got, want)
	}
}

// TestStampAssetDigests_CrossScope: consumer in scope A references
// an asset in scope B. The stamping pipeline routes via the
// (scope, name) key in batchDigests — must NOT fall into "same scope
// only" thinking.
func TestStampAssetDigests_CrossScope(t *testing.T) {
	withTempVooduRoot(t)

	store := newMemStore()

	asset := mkAssetManifest("clowk-cdn", "redis", map[string]string{
		"cfg": "shared-config",
	})

	consumerSpec, _ := json.Marshal(map[string]any{
		"image":   "redis:7",
		"volumes": []string{"${asset.clowk-cdn.redis.cfg}:/etc/redis/redis.conf:ro"},
	})

	consumer := &Manifest{
		Kind:  KindStatefulset,
		Scope: "clowk-lp", // different from the asset's scope
		Name:  "redis",
		Spec:  consumerSpec,
	}

	if err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{asset, consumer}); err != nil {
		t.Fatalf("stamping: %v", err)
	}

	digests := stampedDigests(t, consumer)

	if got, want := digests["clowk-cdn.redis.cfg"], sumOf("shared-config"); got != want {
		t.Errorf("digest = %q, want %q (cross-scope ref)", got, want)
	}
}

// TestStampAssetDigests_DependsOnExplicit: consumer has NO textual
// ${asset.…} ref in any field, but declares the dep via
// depends_on.assets. Stamping must still fold the digest into
// _asset_digests so asset content drift triggers restart.
func TestStampAssetDigests_DependsOnExplicit(t *testing.T) {
	withTempVooduRoot(t)

	store := newMemStore()

	asset := mkAssetManifest("data", "redis", map[string]string{
		"acls": "user default on >password",
	})

	// No textual ${asset.…} anywhere — only depends_on declares the link.
	consumerSpec, _ := json.Marshal(map[string]any{
		"image":   "redis:7",
		"volumes": []string{"/local/path:/etc/redis/something:ro"},
		"depends_on": map[string]any{
			"assets": []any{"data.redis.acls"},
		},
	})

	consumer := &Manifest{
		Kind:  KindStatefulset,
		Scope: "data",
		Name:  "redis",
		Spec:  consumerSpec,
	}

	if err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{asset, consumer}); err != nil {
		t.Fatalf("stamping: %v", err)
	}

	digests := stampedDigests(t, consumer)

	if got, want := digests["data.redis.acls"], sumOf("user default on >password"); got != want {
		t.Errorf("digest = %q, want %q (explicit depends_on)", got, want)
	}
}

// TestStampAssetDigests_DependsOnUnscopedRef: depends_on accepts the
// 2-segment unscoped form `name.key` for global assets. Mirrors how
// `${asset.<name>.<key>}` (3-seg) addresses unscoped assets.
func TestStampAssetDigests_DependsOnUnscopedRef(t *testing.T) {
	withTempVooduRoot(t)

	store := newMemStore()

	asset := mkAssetManifest("", "ca-bundle", map[string]string{
		"pem": "-----BEGIN CERT-----",
	})

	consumerSpec, _ := json.Marshal(map[string]any{
		"image": "app:1",
		"depends_on": map[string]any{
			"assets": []any{"ca-bundle.pem"},
		},
	})

	consumer := &Manifest{
		Kind:  KindDeployment,
		Scope: "app",
		Name:  "web",
		Spec:  consumerSpec,
	}

	if err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{asset, consumer}); err != nil {
		t.Fatalf("stamping: %v", err)
	}

	digests := stampedDigests(t, consumer)

	if got, want := digests["ca-bundle.pem"], sumOf("-----BEGIN CERT-----"); got != want {
		t.Errorf("digest = %q, want %q (unscoped depends_on)", got, want)
	}
}

// TestStampAssetDigests_UnresolvedRefRejects: consumer references
// an asset that exists nowhere (not in batch, not in /status). The
// strict posture means the apply rejects with the formatted ref in
// the error message — operator sees the same shape they typed.
func TestStampAssetDigests_UnresolvedRefRejects(t *testing.T) {
	store := newMemStore()

	consumerSpec, _ := json.Marshal(map[string]any{
		"image":   "redis:7",
		"volumes": []string{"${asset.data.ghost.cfg}:/etc/x:ro"},
	})

	consumer := &Manifest{
		Kind:  KindStatefulset,
		Scope: "data",
		Name:  "redis",
		Spec:  consumerSpec,
	}

	err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{consumer})
	if err == nil {
		t.Fatal("expected error for unresolved ref")
	}

	if !strings.Contains(err.Error(), "data.ghost.cfg") {
		t.Errorf("error should mention the unresolved ref: %v", err)
	}
}

// TestStampAssetDigests_ReStampClearsStale: applying a consumer
// whose spec already carries _asset_digests from a previous apply
// must overwrite — stale entries from the previous round don't
// leak into the new stamp.
func TestStampAssetDigests_ReStampClearsStale(t *testing.T) {
	withTempVooduRoot(t)

	store := newMemStore()

	asset := mkAssetManifest("data", "redis", map[string]string{
		"cfg": "new-bytes",
	})

	consumerSpec, _ := json.Marshal(map[string]any{
		"image":   "redis:7",
		"volumes": []string{"${asset.data.redis.cfg}:/etc/redis/redis.conf:ro"},
		// Stale stamp from a previous apply round (different bytes
		// AND a key that's no longer referenced).
		"_asset_digests": map[string]any{
			"data.redis.cfg":         "stale-digest-from-old-apply",
			"data.redis.removed_key": "ghost",
		},
	})

	consumer := &Manifest{
		Kind:  KindStatefulset,
		Scope: "data",
		Name:  "redis",
		Spec:  consumerSpec,
	}

	if err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{asset, consumer}); err != nil {
		t.Fatalf("stamping: %v", err)
	}

	digests := stampedDigests(t, consumer)

	if got := digests["data.redis.cfg"]; got != sumOf("new-bytes") {
		t.Errorf("expected fresh digest, got %q", got)
	}

	if _, present := digests["data.redis.removed_key"]; present {
		t.Error("stale removed_key digest should have been swept on re-stamp")
	}
}

// TestStampAssetDigests_NoRefsNoStamp: a consumer without any
// asset refs and no depends_on doesn't get an _asset_digests
// field added. Keeps the spec minimal for kinds that don't
// participate in asset machinery.
func TestStampAssetDigests_NoRefsNoStamp(t *testing.T) {
	store := newMemStore()

	consumerSpec, _ := json.Marshal(map[string]any{
		"image":   "redis:7",
		"volumes": []string{"/local:/data"},
	})

	consumer := &Manifest{
		Kind:  KindStatefulset,
		Scope: "data",
		Name:  "redis",
		Spec:  consumerSpec,
	}

	if err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{consumer}); err != nil {
		t.Fatalf("stamping: %v", err)
	}

	var spec map[string]any
	_ = json.Unmarshal(consumer.Spec, &spec)

	if _, present := spec["_asset_digests"]; present {
		t.Error("consumer with no refs should not get _asset_digests stamped")
	}
}

// TestStampAssetDigests_MultipleRefsDeduplicated: a consumer that
// references the same (scope, name, key) twice (e.g. mounted at
// two paths via `volumes = ["${asset.X.Y}:/a", "${asset.X.Y}:/b"]`)
// stamps the digest once. The hash machinery already dedupes via
// flattenAssetDigests's sort, but stamping should produce a clean
// map on its own.
func TestStampAssetDigests_MultipleRefsDeduplicated(t *testing.T) {
	withTempVooduRoot(t)

	store := newMemStore()

	asset := mkAssetManifest("data", "redis", map[string]string{
		"cfg": "config-bytes",
	})

	consumerSpec, _ := json.Marshal(map[string]any{
		"image": "redis:7",
		"volumes": []string{
			"${asset.data.redis.cfg}:/etc/path-a",
			"${asset.data.redis.cfg}:/etc/path-b",
		},
	})

	consumer := &Manifest{
		Kind:  KindStatefulset,
		Scope: "data",
		Name:  "redis",
		Spec:  consumerSpec,
	}

	if err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{asset, consumer}); err != nil {
		t.Fatalf("stamping: %v", err)
	}

	digests := stampedDigests(t, consumer)

	if len(digests) != 1 {
		t.Errorf("duplicate refs should produce single digest entry, got %d: %v", len(digests), digests)
	}
}

// TestParseDependsOnRef pins the parser semantics — 2 segments
// produce an unscoped ref (Scope=""), 3 segments produce a scoped
// ref. Anything else is an error.
func TestParseDependsOnRef(t *testing.T) {
	cases := []struct {
		in        string
		want      assetRef
		expectErr bool
	}{
		{"name.key", assetRef{Name: "name", Key: "key"}, false},
		{"scope.name.key", assetRef{Scope: "scope", Name: "name", Key: "key"}, false},
		{"only-one", assetRef{}, true},
		{"a.b.c.d", assetRef{}, true},
		{"", assetRef{}, true},
		{".name.key", assetRef{}, true},
		{"name.", assetRef{}, true},
	}

	for _, tc := range cases {
		got, err := parseDependsOnRef(tc.in)

		if tc.expectErr {
			if err == nil {
				t.Errorf("parseDependsOnRef(%q): expected error, got %+v", tc.in, got)
			}

			continue
		}

		if err != nil {
			t.Errorf("parseDependsOnRef(%q): unexpected error %v", tc.in, err)
			continue
		}

		if got != tc.want {
			t.Errorf("parseDependsOnRef(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// TestStampAssetDigests_BatchOverridesStaleStatus: when an asset
// is in BOTH the batch and /status, the batch digest wins (it's
// the about-to-be-applied value, more recent than the prior
// reconcile's). Without this the new bytes would never propagate.
func TestStampAssetDigests_BatchOverridesStaleStatus(t *testing.T) {
	withTempVooduRoot(t)

	store := newMemStore()

	// /status has the OLD digest from a prior reconcile.
	old := AssetStatus{Files: map[string]string{"cfg": sumOf("old-bytes")}}
	blob, _ := json.Marshal(old)
	_ = store.PutStatus(context.Background(), KindAsset, AppID("data", "redis"), blob)

	// Batch carries the NEW asset bytes.
	asset := mkAssetManifest("data", "redis", map[string]string{
		"cfg": "new-bytes",
	})

	consumerSpec, _ := json.Marshal(map[string]any{
		"image":   "redis:7",
		"volumes": []string{"${asset.data.redis.cfg}:/etc/redis/redis.conf:ro"},
	})

	consumer := &Manifest{
		Kind:  KindStatefulset,
		Scope: "data",
		Name:  "redis",
		Spec:  consumerSpec,
	}

	if err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{asset, consumer}); err != nil {
		t.Fatalf("stamping: %v", err)
	}

	digests := stampedDigests(t, consumer)

	if got, want := digests["data.redis.cfg"], sumOf("new-bytes"); got != want {
		t.Errorf("digest = %q, want %q (batch must win over /status)", got, want)
	}
}

// TestCollectRefsFromSpecMap_SkipsInternalAndDependsOn pins the
// walker invariant: top-level "_*" keys and "depends_on" are NOT
// scanned for textual refs. The "_asset_digests" stamped field
// would otherwise look like "data.redis.cfg" and self-amplify
// through re-stamping; the depends_on block is handled by the
// explicit parser which has stricter format checks.
func TestCollectRefsFromSpecMap_SkipsInternalAndDependsOn(t *testing.T) {
	// Mirror production shape: walker is fed the result of
	// json.Unmarshal into map[string]any, where slices come
	// out as []any (not []string).
	rawSpec, _ := json.Marshal(map[string]any{
		"image":   "redis:7",
		"volumes": []string{"${asset.data.redis.cfg}:/etc/redis/redis.conf:ro"},
		"_asset_digests": map[string]any{
			"this-text-should-not-be-scanned": "${asset.ghost.X.Y}",
		},
		"depends_on": map[string]any{
			"assets": []any{"${asset.ghost.X.Y}"},
		},
	})

	var spec map[string]any
	if err := json.Unmarshal(rawSpec, &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	refs := collectRefsFromSpecMap(spec)

	if len(refs) != 1 {
		t.Errorf("expected 1 ref (only volumes), got %d: %+v", len(refs), refs)
	}

	if len(refs) > 0 && refs[0].Name != "redis" {
		t.Errorf("expected redis ref, got %+v", refs[0])
	}
}

// TestStampAssetDigests_MaterializesBytesToDisk pins the
// race-avoidance contract: stamping (called inline from the
// apply pipeline before /desired persist) must write asset bytes
// to disk before returning. Otherwise the consumer reconcile
// fires its watch-event and tries to bind-mount a non-existent
// source path, docker creates the path as a directory, and the
// bytes are stuck out forever (rename onto a dir errors EISDIR
// on every retry).
func TestStampAssetDigests_MaterializesBytesToDisk(t *testing.T) {
	root := withTempVooduRoot(t)
	store := newMemStore()

	asset := mkAssetManifest("data", "redis", map[string]string{
		"cfg": "save 900 1\n",
	})

	if err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{asset}); err != nil {
		t.Fatalf("stamping: %v", err)
	}

	wantPath := filepath.Join(root, "assets", "data", "redis", "cfg")

	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat %s: %v (asset bytes should be on disk after stamping)", wantPath, err)
	}

	if info.IsDir() {
		t.Errorf("%s is a directory; expected regular file", wantPath)
	}

	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(got) != "save 900 1\n" {
		t.Errorf("on-disk content = %q, want %q", got, "save 900 1\n")
	}
}

// TestMaterializeAssetSpec_RecoverFromDirectoryAtDestination is
// the regression test for the user's bug: a previous race left
// /opt/voodu/assets/<scope>/<name>/<key> as a directory (docker
// bind mount created it before the asset reconcile got there).
// Subsequent applies must clean up the dir and write the file
// — otherwise os.Rename onto a dir errors EISDIR forever.
func TestMaterializeAssetSpec_RecoverFromDirectoryAtDestination(t *testing.T) {
	root := withTempVooduRoot(t)
	store := newMemStore()

	// Simulate the bad state: dst path exists as a directory.
	dstParent := filepath.Join(root, "assets", "clowk-lp", "cdn")
	if err := os.MkdirAll(filepath.Join(dstParent, "acls"), 0755); err != nil {
		t.Fatalf("seed bad state: %v", err)
	}

	asset := mkAssetManifest("clowk-lp", "cdn", map[string]string{
		"acls": "user thadeu on allkeys +@all >mysecretpass\n",
	})

	digests, errs, err := materializeAssetSpec(context.Background(), store, nil, nil, asset)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	if len(errs) != 0 {
		t.Errorf("expected no per-key errors after dir cleanup, got: %v", errs)
	}

	if _, ok := digests["acls"]; !ok {
		t.Errorf("expected digest for acls, got: %v", digests)
	}

	// File should now be a regular file with the new bytes.
	dst := filepath.Join(dstParent, "acls")
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if info.IsDir() {
		t.Fatalf("%s still a directory after cleanup", dst)
	}

	got, _ := os.ReadFile(dst)
	if string(got) != "user thadeu on allkeys +@all >mysecretpass\n" {
		t.Errorf("content after recovery = %q", got)
	}
}

// TestMaterializeAssetSpec_HandlerAndStampingProduceSameState is
// the contract that AssetHandler.apply (async path) and
// StampAssetDigests (sync path) materialise to byte-identical
// disk state. Drift here would mean the reconciler-driven retry
// undoes what the apply-pipeline did or vice versa.
func TestMaterializeAssetSpec_HandlerAndStampingProduceSameState(t *testing.T) {
	root := withTempVooduRoot(t)
	store := newMemStore()

	asset := mkAssetManifest("data", "shared", map[string]string{
		"cfg":  "key=value\n",
		"acls": "user admin\n",
	})

	// Run the sync path (stamping).
	if err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{asset}); err != nil {
		t.Fatalf("sync stamping: %v", err)
	}

	syncCfg, _ := os.ReadFile(filepath.Join(root, "assets", "data", "shared", "cfg"))
	syncAcls, _ := os.ReadFile(filepath.Join(root, "assets", "data", "shared", "acls"))
	syncStatus, _ := store.GetStatus(context.Background(), KindAsset, AppID("data", "shared"))

	// Wipe disk + status, then run the async handler path.
	_ = os.RemoveAll(filepath.Join(root, "assets", "data", "shared"))
	_ = store.DeleteStatus(context.Background(), KindAsset, AppID("data", "shared"))

	handler := &AssetHandler{Store: store}
	if err := handler.apply(context.Background(), WatchEvent{Type: WatchPut, Manifest: asset, Scope: "data", Name: "shared"}); err != nil {
		t.Fatalf("async handler: %v", err)
	}

	asyncCfg, _ := os.ReadFile(filepath.Join(root, "assets", "data", "shared", "cfg"))
	asyncAcls, _ := os.ReadFile(filepath.Join(root, "assets", "data", "shared", "acls"))
	asyncStatus, _ := store.GetStatus(context.Background(), KindAsset, AppID("data", "shared"))

	if string(syncCfg) != string(asyncCfg) {
		t.Errorf("cfg bytes diverge:\n  sync:  %q\n  async: %q", syncCfg, asyncCfg)
	}

	if string(syncAcls) != string(asyncAcls) {
		t.Errorf("acls bytes diverge:\n  sync:  %q\n  async: %q", syncAcls, asyncAcls)
	}

	// Status blob should have the same Files map (timestamps
	// will differ — strip them before comparing).
	var sStatus, aStatus AssetStatus
	_ = json.Unmarshal(syncStatus, &sStatus)
	_ = json.Unmarshal(asyncStatus, &aStatus)

	if !mapsEqual(sStatus.Files, aStatus.Files) {
		t.Errorf("status digests diverge:\n  sync:  %v\n  async: %v", sStatus.Files, aStatus.Files)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}

	for k, v := range a {
		if b[k] != v {
			return false
		}
	}

	return true
}

// TestStampAssetDigests_PluginExpansionDependsOn simulates what
// happens when a `redis "..." "..." { depends_on = {...} }` macro
// expands: the plugin-emitted statefulset inherits depends_on
// via the operator-spec shallow-merge, so stamping picks it up
// post-expansion exactly like an operator-authored statefulset.
//
// Test isn't actually invoking a plugin (that's heavier infra),
// but it does mirror the post-expansion shape: a statefulset
// spec carrying depends_on as a plain field.
func TestStampAssetDigests_PluginExpansionDependsOn(t *testing.T) {
	withTempVooduRoot(t)

	store := newMemStore()

	asset := mkAssetManifest("data", "redis", map[string]string{
		"acls": "user default on",
	})

	// Post-expansion shape: voodu-redis emits this when the operator
	// declares `redis "data" "redis" { depends_on { assets = [...] } }`.
	consumerSpec, _ := json.Marshal(map[string]any{
		"image": "redis:8",
		"command": []string{
			"redis-server",
			"/etc/redis/redis.conf",
		},
		"volumes": []string{
			fmt.Sprintf("/some/path:/etc/redis/redis.conf:ro"),
		},
		"depends_on": map[string]any{
			"assets": []any{"data.redis.acls"},
		},
	})

	consumer := &Manifest{
		Kind:  KindStatefulset,
		Scope: "data",
		Name:  "redis",
		Spec:  consumerSpec,
	}

	if err := StampAssetDigests(context.Background(), store, nil, nil, []*Manifest{asset, consumer}); err != nil {
		t.Fatalf("stamping: %v", err)
	}

	digests := stampedDigests(t, consumer)

	if got, want := digests["data.redis.acls"], sumOf("user default on"); got != want {
		t.Errorf("plugin-expanded depends_on: digest = %q, want %q", got, want)
	}
}
