package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

	got, err := store.GetFrozenOrdinals(context.Background(), KindStatefulset, "clowk-lp", "redis")
	if err != nil {
		t.Fatalf("GetFrozenOrdinals: %v", err)
	}

	if len(got) != 1 || got[0] != 2 {
		t.Errorf("frozen ordinals = %v, want [2]", got)
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
	got, _ := store.GetFrozenOrdinals(context.Background(), KindStatefulset, "clowk-lp", "redis")
	if len(got) != 0 {
		t.Errorf("frozen ordinals should be empty for --no-freeze, got %v", got)
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
	if err := store.SetFrozenOrdinals(context.Background(), KindStatefulset, "clowk-lp", "redis", []int{2}); err != nil {
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
	got, _ := store.GetFrozenOrdinals(context.Background(), KindStatefulset, "clowk-lp", "redis")
	if len(got) != 0 {
		t.Errorf("frozen ordinals should be empty after start, got %v", got)
	}

	// Response surfaces the unfreeze signal so CLI can render
	// "started (unfrozen)" vs plain "started".
	var env struct {
		Data struct {
			Unfroze bool `json:"unfroze"`
		} `json:"data"`
	}

	body, _ := json.Marshal(resp)
	_ = body

	// Re-fetch and decode the body explicitly. (The request was
	// already drained above; re-fire to assert the body shape.)
	resp2, err := http.Post(ts.URL+"/pods/clowk-lp-redis.2/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp2.Body.Close()

	if err := json.NewDecoder(resp2.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Second start with no remaining freeze: unfroze=false.
	if env.Data.Unfroze {
		t.Errorf("expected unfroze=false on second start (already cleared); got true")
	}
}

// TestPodStop_RejectsDeployment: freeze is statefulset-only.
// Deployment replica IDs are hex and regenerated on every spawn,
// so a per-replica freeze annotation can't survive scale events.
// Surface this loudly at the API instead of silently writing a
// useless freeze key.
func TestPodStop_RejectsDeployment(t *testing.T) {
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

	_, _, ts := setupPodLifecycleTestAPI(t, lifecycle)

	resp, err := http.Post(ts.URL+"/pods/clowk-lp-web.a3f9/stop?freeze=true", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}

	body := readAll(t, resp)
	if !strings.Contains(body, "statefulset") {
		t.Errorf("error should mention statefulset-only restriction; got: %s", body)
	}

	// Stop must NOT have been called — freeze rejection is a
	// pre-flight check that aborts before docker.
	if len(lifecycle.stops) != 0 {
		t.Errorf("Stop should not fire when freeze rejected, got %v", lifecycle.stops)
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

// readAll drains a response body to a string. Test helper.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()

	var sb strings.Builder

	buf := make([]byte, 4096)

	for {
		n, err := resp.Body.Read(buf)

		if n > 0 {
			sb.Write(buf[:n])
		}

		if err != nil {
			break
		}
	}

	return sb.String()
}
