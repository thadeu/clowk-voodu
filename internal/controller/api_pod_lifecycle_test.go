package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.voodu.clowk.in/internal/containers"
)

// fakePodLifecycle records Stop/Start/Remove calls and returns a
// canned label map so the API can recover voodu identity for
// the freeze persistence path.
type fakePodLifecycle struct {
	labelsByName map[string]map[string]string
	stops        []string
	starts       []string
	removes      []string
	stopErr      error
	startErr     error
	removeErr    error
}

func (f *fakePodLifecycle) Stop(name string) error {
	f.stops = append(f.stops, name)
	return f.stopErr
}

func (f *fakePodLifecycle) Start(name string) error {
	f.starts = append(f.starts, name)
	return f.startErr
}

func (f *fakePodLifecycle) Remove(name string) error {
	f.removes = append(f.removes, name)
	return f.removeErr
}

func (f *fakePodLifecycle) InspectLabels(name string) (map[string]string, error) {
	return f.labelsByName[name], nil
}

func setupPodLifecycleTestAPI(t *testing.T, lifecycle *fakePodLifecycle) (*API, *memStore, *httptest.Server) {
	t.Helper()

	api, store := newTestAPI(t)
	api.PodLifecycle = lifecycle

	ts := httptest.NewServer(api.Handler())

	t.Cleanup(ts.Close)

	return api, store, ts
}

// TestPodStop_FreezePersistsOrdinal pins the core contract: a
// `vd stop --freeze` (POST /pods/{name}/stop?freeze=true) must
// add the pod's ordinal to the frozen-ordinals annotation in the
// store, so subsequent reconciles skip it. Without this, freeze
// would be a no-op and the next env-change would re-spawn.
func TestPodStop_FreezePersistsOrdinal(t *testing.T) {
	lifecycle := &fakePodLifecycle{
		labelsByName: map[string]map[string]string{
			"clowk-lp-redis.2": {
				containers.LabelCreatedBy:      containers.LabelCreatedByValue,
				containers.LabelKind:           containers.KindStatefulset,
				containers.LabelScope:          "clowk-lp",
				containers.LabelName:           "redis",
				containers.LabelReplicaID:      "2",
				containers.LabelReplicaOrdinal: "2",
			},
		},
	}

	_, store, ts := setupPodLifecycleTestAPI(t, lifecycle)

	resp, err := http.Post(ts.URL+"/pods/clowk-lp-redis.2/stop?freeze=true", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	if len(lifecycle.stops) != 1 || lifecycle.stops[0] != "clowk-lp-redis.2" {
		t.Errorf("Stop calls = %v, want [clowk-lp-redis.2]", lifecycle.stops)
	}

	got, err := store.GetFrozenReplicaIDs(context.Background(), KindStatefulset, "clowk-lp", "redis")
	if err != nil {
		t.Fatalf("GetFrozenReplicaIDs: %v", err)
	}

	if len(got) != 1 || got[0] != "2" {
		t.Errorf("frozen replicas = %v, want [\"2\"]", got)
	}
}

// TestPodStop_NoFreezeSkipsStore: when freeze=false the stop is
// transient — the store must NOT receive any freeze annotation
// (otherwise the operator's `--no-freeze` becomes meaningless and
// next reconcile leaves the pod offline anyway).
func TestPodStop_NoFreezeSkipsStore(t *testing.T) {
	lifecycle := &fakePodLifecycle{}

	_, store, ts := setupPodLifecycleTestAPI(t, lifecycle)

	resp, err := http.Post(ts.URL+"/pods/clowk-lp-redis.2/stop?freeze=false", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	if len(lifecycle.stops) != 1 {
		t.Errorf("expected one Stop call, got %d", len(lifecycle.stops))
	}

	// Store stays empty.
	got, _ := store.GetFrozenReplicaIDs(context.Background(), KindStatefulset, "clowk-lp", "redis")
	if len(got) != 0 {
		t.Errorf("frozen replicas should be empty for --no-freeze, got %v", got)
	}
}

// TestPodStart_RecreatesContainerForManagedPod pins the core
// of the bug fix: starting a frozen pod (or any voodu-managed
// pod) goes through Remove + re-Put-manifest, NOT plain `docker
// start`. This is the only way to force a fresh `--env-file`
// read after env vars changed (e.g., REDIS_MASTER_ORDINAL flipped
// during a failover while the pod was frozen).
//
// Without this fix, plain `docker start` would boot the container
// with stale env vars from its original `docker run` — surfacing
// as "redis-0 boots as master after failover-to-1" and causing
// async-replication data loss.
func TestPodStart_RecreatesContainerForManagedPod(t *testing.T) {
	lifecycle := &fakePodLifecycle{
		labelsByName: map[string]map[string]string{
			"clowk-lp-redis.2": {
				containers.LabelCreatedBy:      containers.LabelCreatedByValue,
				containers.LabelKind:           containers.KindStatefulset,
				containers.LabelScope:          "clowk-lp",
				containers.LabelName:           "redis",
				containers.LabelReplicaID:      "2",
				containers.LabelReplicaOrdinal: "2",
			},
		},
	}

	_, store, ts := setupPodLifecycleTestAPI(t, lifecycle)

	// Pre-seed: ordinal 2 frozen + a manifest the start path
	// can re-Put to fire the reconciler.
	if err := store.SetFrozenReplicaIDs(context.Background(), KindStatefulset, "clowk-lp", "redis", []string{"2"}); err != nil {
		t.Fatalf("seed frozen: %v", err)
	}

	manifest := &Manifest{
		Kind:  KindStatefulset,
		Scope: "clowk-lp",
		Name:  "redis",
		Spec:  []byte(`{"image":"redis:7","replicas":3}`),
	}

	if _, err := store.Put(context.Background(), manifest); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	preRevision := int64(0)

	if got, _ := store.Get(context.Background(), KindStatefulset, "clowk-lp", "redis"); got != nil && got.Metadata != nil {
		preRevision = got.Metadata.Revision
	}

	resp, err := http.Post(ts.URL+"/pods/clowk-lp-redis.2/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	// The recreate path: Remove the stale container + re-Put
	// the manifest. NO plain Start call — that would skip the
	// env-file refresh.
	if len(lifecycle.removes) != 1 || lifecycle.removes[0] != "clowk-lp-redis.2" {
		t.Errorf("Remove calls = %v, want [clowk-lp-redis.2]", lifecycle.removes)
	}

	if len(lifecycle.starts) != 0 {
		t.Errorf("plain Start should NOT fire for managed pods; got %v", lifecycle.starts)
	}

	// Annotation was cleared so the upcoming reconcile doesn't
	// re-skip the slot.
	got, _ := store.GetFrozenReplicaIDs(context.Background(), KindStatefulset, "clowk-lp", "redis")
	if len(got) != 0 {
		t.Errorf("frozen replicas should be empty after start, got %v", got)
	}

	// Manifest revision bumped — proves the Put fired (which is
	// what triggers the watcher and the reconciler's apply).
	post, _ := store.Get(context.Background(), KindStatefulset, "clowk-lp", "redis")
	if post == nil || post.Metadata == nil || post.Metadata.Revision <= preRevision {
		t.Errorf("manifest revision should bump after start (was %d), got %v",
			preRevision, post)
	}

	// Response surfaces recreate=true and unfroze=true so CLI
	// can render the appropriate status line.
	var env struct {
		Data struct {
			Recreated bool `json:"recreated"`
			Unfroze   bool `json:"unfroze"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !env.Data.Recreated {
		t.Errorf("response.recreated should be true for managed-pod start")
	}

	if !env.Data.Unfroze {
		t.Errorf("response.unfroze should be true (freeze was cleared)")
	}
}

// TestPodStart_PlainStartForUnmanagedPod: containers without a
// voodu identity (legacy / hand-spawned) fall back to plain
// `docker start`. We have no manifest to re-Put, so the env-
// file-refresh path doesn't apply.
func TestPodStart_PlainStartForUnmanagedPod(t *testing.T) {
	lifecycle := &fakePodLifecycle{
		// No labels — orphan / legacy container.
	}

	_, _, ts := setupPodLifecycleTestAPI(t, lifecycle)

	resp, err := http.Post(ts.URL+"/pods/legacy-app/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	if len(lifecycle.starts) != 1 || lifecycle.starts[0] != "legacy-app" {
		t.Errorf("Start should fire for unmanaged pod; got %v", lifecycle.starts)
	}

	if len(lifecycle.removes) != 0 {
		t.Errorf("Remove should NOT fire for unmanaged pod; got %v", lifecycle.removes)
	}
}

// TestPodStart_GoneWhenManifestMissing: managed pod, but its
// manifest was deleted between freeze and start. We removed the
// container (it's gone) but can't re-Put a missing manifest —
// 410 Gone tells the operator to `vd apply` first.
func TestPodStart_GoneWhenManifestMissing(t *testing.T) {
	lifecycle := &fakePodLifecycle{
		labelsByName: map[string]map[string]string{
			"clowk-lp-redis.2": {
				containers.LabelCreatedBy:      containers.LabelCreatedByValue,
				containers.LabelKind:           containers.KindStatefulset,
				containers.LabelScope:          "clowk-lp",
				containers.LabelName:           "redis",
				containers.LabelReplicaID:      "2",
				containers.LabelReplicaOrdinal: "2",
			},
		},
	}

	_, _, ts := setupPodLifecycleTestAPI(t, lifecycle)

	// No manifest seeded.
	resp, err := http.Post(ts.URL+"/pods/clowk-lp-redis.2/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusGone {
		t.Errorf("status=%d want 410", resp.StatusCode)
	}
}

// TestPodStop_FreezeWorksForDeployment: deployment replica IDs
// are hex strings (regenerated per spawn) but the freeze list
// stores them as-is. The rolling-restart path skips containers
// whose ID is in the list, leaving the frozen pod intact across
// any reconcile that recreates its siblings. Operator's
// `vd start <ref>` clears the entry.
//
// Trade-off documented elsewhere: the frozen pod keeps its
// original image/env across rolling restarts (stale config),
// since we can't recreate it without losing the ID. Operator
// runs `vd restart` after `vd start` if they need fresh config.
func TestPodStop_FreezeWorksForDeployment(t *testing.T) {
	lifecycle := &fakePodLifecycle{
		labelsByName: map[string]map[string]string{
			"clowk-lp-web.a3f9": {
				containers.LabelCreatedBy: containers.LabelCreatedByValue,
				containers.LabelKind:      containers.KindDeployment,
				containers.LabelScope:     "clowk-lp",
				containers.LabelName:      "web",
				containers.LabelReplicaID: "a3f9",
			},
		},
	}

	_, store, ts := setupPodLifecycleTestAPI(t, lifecycle)

	resp, err := http.Post(ts.URL+"/pods/clowk-lp-web.a3f9/stop?freeze=true", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", resp.StatusCode)
	}

	if len(lifecycle.stops) != 1 || lifecycle.stops[0] != "clowk-lp-web.a3f9" {
		t.Errorf("Stop calls = %v, want [clowk-lp-web.a3f9]", lifecycle.stops)
	}

	got, _ := store.GetFrozenReplicaIDs(context.Background(), KindDeployment, "clowk-lp", "web")
	if len(got) != 1 || got[0] != "a3f9" {
		t.Errorf("frozen replicas for deployment = %v, want [\"a3f9\"]", got)
	}
}

// TestRestart_ClearsFreezesPreEmptively pins the operator-
// recovery contract: `vd restart` is the explicit "rebuild
// everything in this resource" command, so it CLEARS frozen
// annotations before invoking the handler's Restart. Without
// this, a frozen pod stays parked even after the operator
// explicitly asked to restart, surprising them.
//
// The rolling restart underneath still uses its frozen-set
// load (which now returns empty), so all replicas — including
// the previously-frozen one — get recreated with fresh env.
//
// Internal reconcile triggers (config_set fan-out, env-change)
// don't go through this path; they hit Store.Put directly,
// where the handler's frozen-set check still fires.
func TestRestart_ClearsFreezesPreEmptively(t *testing.T) {
	api, store := newTestAPI(t)

	// Wire a stub StatefulsetRestarter that just records the
	// call — we're testing the API-level freeze-clear, not the
	// rolling-restart machinery.
	called := false
	api.Statefulsets = &fakeStatefulsetRestarter{
		restartFn: func(ctx context.Context, scope, name string) error {
			called = true
			return nil
		},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	// Pre-seed: pod-2 frozen.
	if err := store.SetFrozenReplicaIDs(context.Background(), KindStatefulset, "clowk-lp", "redis", []string{"2"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Pre-seed manifest so resolveScope finds the resource.
	if _, err := store.Put(context.Background(), &Manifest{
		Kind:  KindStatefulset,
		Scope: "clowk-lp",
		Name:  "redis",
		Spec:  []byte(`{"image":"redis:7","replicas":3}`),
	}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	resp, err := http.Post(ts.URL+"/restart?kind=statefulset&scope=clowk-lp&name=redis", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	if !called {
		t.Errorf("Restart handler was not invoked")
	}

	// Freeze annotation cleared — the rolling restart that
	// follows treats every ordinal as eligible for recreate.
	got, _ := store.GetFrozenReplicaIDs(context.Background(), KindStatefulset, "clowk-lp", "redis")
	if len(got) != 0 {
		t.Errorf("frozen replicas should be empty after restart, got %v", got)
	}

	// Response surfaces the unfreeze list so CLI can render
	// "restarted (unfroze N replicas)".
	var env struct {
		Data struct {
			Unfroze []string `json:"unfroze"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(env.Data.Unfroze) != 1 || env.Data.Unfroze[0] != "2" {
		t.Errorf("unfroze list = %v, want [2]", env.Data.Unfroze)
	}
}

// fakeStatefulsetRestarter is a minimal stub for the
// StatefulsetRestarter interface — only Restart is exercised.
type fakeStatefulsetRestarter struct {
	restartFn func(ctx context.Context, scope, name string) error
}

func (f *fakeStatefulsetRestarter) Restart(ctx context.Context, scope, name string) error {
	return f.restartFn(ctx, scope, name)
}

func (f *fakeStatefulsetRestarter) Rollback(ctx context.Context, scope, name, targetID string) (string, error) {
	return "", nil
}

func (f *fakeStatefulsetRestarter) PruneVolumes(scope, name string) ([]string, error) {
	return nil, nil
}

func (f *fakeStatefulsetRestarter) Volumes(scope, name string) ([]string, error) {
	return nil, nil
}

// TestPodStop_NotConfigured: API without PodLifecycle wired
// returns 503, not a panic. Lets server bootstrap order be
// flexible — if PodLifecycle isn't set, the endpoint just says
// "not configured".
func TestPodStop_NotConfigured(t *testing.T) {
	api, _ := newTestAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/pods/anything.0/stop", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", resp.StatusCode)
	}
}

