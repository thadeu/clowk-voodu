package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.voodu.clowk.in/internal/containers"
)

// fakePodLifecycle records Stop/Start calls and returns a
// canned label map so the API can recover voodu identity for
// the freeze persistence path.
type fakePodLifecycle struct {
	labelsByName map[string]map[string]string
	stops        []string
	starts       []string
	stopErr      error
	startErr     error
}

func (f *fakePodLifecycle) Stop(name string) error {
	f.stops = append(f.stops, name)
	return f.stopErr
}

func (f *fakePodLifecycle) Start(name string) error {
	f.starts = append(f.starts, name)
	return f.startErr
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

// TestPodStart_ClearsFreeze: starting a frozen pod must clear
// its ordinal from the annotation, so future reconciles include
// it again. Without this, `vd start` would unstick the docker
// container but the controller's reconcile path would still
// skip the ordinal forever (operator-confusing).
func TestPodStart_ClearsFreeze(t *testing.T) {
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

	// Pre-seed: ordinal 2 frozen.
	if err := store.SetFrozenReplicaIDs(context.Background(), KindStatefulset, "clowk-lp", "redis", []string{"2"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, err := http.Post(ts.URL+"/pods/clowk-lp-redis.2/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	if len(lifecycle.starts) != 1 || lifecycle.starts[0] != "clowk-lp-redis.2" {
		t.Errorf("Start calls = %v, want [clowk-lp-redis.2]", lifecycle.starts)
	}

	// Annotation was cleared.
	got, _ := store.GetFrozenReplicaIDs(context.Background(), KindStatefulset, "clowk-lp", "redis")
	if len(got) != 0 {
		t.Errorf("frozen replicas should be empty after start, got %v", got)
	}

	// Response surfaces the unfreeze signal so CLI can render
	// "started (unfrozen)" vs plain "started".
	// Second start with no remaining freeze: unfroze should be
	// false. Re-fire the request and decode the response body.
	resp2, err := http.Post(ts.URL+"/pods/clowk-lp-redis.2/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp2.Body.Close()

	var env struct {
		Data struct {
			Unfroze bool `json:"unfroze"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp2.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if env.Data.Unfroze {
		t.Errorf("expected unfroze=false on second start (already cleared); got true")
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

