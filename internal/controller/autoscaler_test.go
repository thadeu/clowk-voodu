// Tests for the M7 CPU-driven Autoscaler. Each test pins one
// behaviour of the decision loop:
//
//   - Up/down/hold based on CPU vs target with hysteresis
//   - Min / Max bounds are respected (no scale past the floor or
//     ceiling)
//   - Cooldown windows gate consecutive scale events
//   - Empty pod list (deployment applied, nothing running yet)
//     is a silent no-op, not an error
//   - Deployments WITHOUT an autoscale block are skipped entirely
//
// Wiring: fakeAutoscaleApplier records the SetReplicas calls; the
// existing fakePodsLister + fakeStatsClient (from stats_test.go)
// fake the runtime view; memStore holds manifests.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/docker"
)

// fakeAutoscaleApplier records every SetReplicas call so tests can
// assert "the scaler decided to scale to N" without needing a real
// reconciler. Errors can be injected via the err field.
type fakeAutoscaleApplier struct {
	mu    sync.Mutex
	calls []autoscaleCall
	err   error
}

type autoscaleCall struct {
	Scope    string
	Name     string
	Replicas int
}

func (f *fakeAutoscaleApplier) SetReplicas(_ context.Context, scope, name string, replicas int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, autoscaleCall{Scope: scope, Name: name, Replicas: replicas})

	return f.err
}

func (f *fakeAutoscaleApplier) Calls() []autoscaleCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]autoscaleCall, len(f.calls))
	copy(out, f.calls)

	return out
}

// putDeploymentWithAutoscale seeds a deployment manifest carrying
// an autoscale block. Tests pass the bounds and cpu_target; cooldowns
// stay empty (controller defaults apply unless the test overrides).
func putDeploymentWithAutoscale(t *testing.T, store *memStore, scope, name string, min, max, target int) {
	t.Helper()

	spec := map[string]any{
		"image":    "x",
		"replicas": min,
		"autoscale": map[string]any{
			"min":        min,
			"max":        max,
			"cpu_target": target,
		},
	}

	raw, _ := json.Marshal(spec)

	if _, err := store.Put(context.Background(), &Manifest{
		Kind:  KindDeployment,
		Scope: scope,
		Name:  name,
		Spec:  raw,
	}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
}

// makeReplicaPods generates `count` running pods for a deployment.
// Each pod gets a distinct replica id so the pods list reflects the
// fleet size — what the scaler treats as "current replica count".
func makeReplicaPods(scope, name string, count int) []Pod {
	out := make([]Pod, 0, count)

	for i := 0; i < count; i++ {
		replicaID := fmt.Sprintf("r%d", i)
		container := fmt.Sprintf("voodu-%s-%s.%s", scope, name, replicaID)
		out = append(out, makePod("deployment", scope, name, replicaID, container, true))
	}

	return out
}

// makeUniformStats builds a stats map where every pod reports the
// same CPU% — the common test fixture for "fleet running at X%".
func makeUniformStats(pods []Pod, cpu float64) map[string]docker.ContainerStats {
	out := make(map[string]docker.ContainerStats, len(pods))

	for _, p := range pods {
		out[p.Name] = makeStats(p.Name, cpu, 100*1024*1024)
	}

	return out
}

// newTestAutoscaler wires the fakes into an Autoscaler ready for
// evaluate calls. Tick is short so a separate test that wants to
// exercise the Run loop can timeout quickly; per-deployment tests
// invoke evaluate directly.
func newTestAutoscaler(store *memStore, pods []Pod, statsMap map[string]docker.ContainerStats) (*Autoscaler, *fakeAutoscaleApplier) {
	collector := &StatsCollector{
		Pods:  &fakePodsLister{pods: pods},
		Stats: &fakeStatsClient{byName: statsMap},
		Store: store,
	}

	applier := &fakeAutoscaleApplier{}

	return &Autoscaler{
		Store: store,
		Stats: collector,
		Apply: applier,
		Tick:  10 * time.Millisecond,
	}, applier
}

func TestAutoscaler_ScalesUpOnHighCPU(t *testing.T) {
	// Fleet at 2 replicas, both running at 90% CPU, target 70%.
	// 90 > 70 * 1.1 = 77 → scale up to 3.
	store := newMemStore()
	putDeploymentWithAutoscale(t, store, "prod", "api", 2, 10, 70)

	pods := makeReplicaPods("prod", "api", 2)
	stats := makeUniformStats(pods, 90)

	as, applier := newTestAutoscaler(store, pods, stats)

	as.evaluate(context.Background())

	calls := applier.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 scale call, got %d: %+v", len(calls), calls)
	}

	if calls[0].Replicas != 3 {
		t.Errorf("expected scale to 3, got %d", calls[0].Replicas)
	}
}

func TestAutoscaler_ScalesDownOnLowCPU(t *testing.T) {
	// Fleet at 4 replicas, all running at 20% CPU, target 70%.
	// 20 < 70 * 0.7 = 49 → scale down to 3.
	store := newMemStore()
	putDeploymentWithAutoscale(t, store, "prod", "api", 1, 10, 70)

	pods := makeReplicaPods("prod", "api", 4)
	stats := makeUniformStats(pods, 20)

	as, applier := newTestAutoscaler(store, pods, stats)

	as.evaluate(context.Background())

	calls := applier.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 scale call, got %d", len(calls))
	}

	if calls[0].Replicas != 3 {
		t.Errorf("expected scale to 3, got %d", calls[0].Replicas)
	}
}

func TestAutoscaler_RespectsMin(t *testing.T) {
	// Fleet at 2 (= min), CPU dropped to 5% — well below scale-down
	// threshold. The scaler must NOT call SetReplicas at all (no
	// floor underflow).
	store := newMemStore()
	putDeploymentWithAutoscale(t, store, "prod", "api", 2, 10, 70)

	pods := makeReplicaPods("prod", "api", 2)
	stats := makeUniformStats(pods, 5)

	as, applier := newTestAutoscaler(store, pods, stats)

	as.evaluate(context.Background())

	if calls := applier.Calls(); len(calls) != 0 {
		t.Errorf("expected no scale calls at min, got %+v", calls)
	}
}

func TestAutoscaler_RespectsMax(t *testing.T) {
	// Fleet at 10 (= max), CPU pegged at 99% — well above scale-up
	// threshold. The scaler must NOT call SetReplicas (no ceiling
	// overflow).
	store := newMemStore()
	putDeploymentWithAutoscale(t, store, "prod", "api", 2, 10, 70)

	pods := makeReplicaPods("prod", "api", 10)
	stats := makeUniformStats(pods, 99)

	as, applier := newTestAutoscaler(store, pods, stats)

	as.evaluate(context.Background())

	if calls := applier.Calls(); len(calls) != 0 {
		t.Errorf("expected no scale calls at max, got %+v", calls)
	}
}

func TestAutoscaler_RespectsCooldownUp(t *testing.T) {
	// Two consecutive evaluations with the same high-CPU input.
	// First triggers a scale-up; the second must hold because the
	// cooldown window (default 30s) has not elapsed.
	store := newMemStore()
	putDeploymentWithAutoscale(t, store, "prod", "api", 2, 10, 70)

	pods := makeReplicaPods("prod", "api", 2)
	stats := makeUniformStats(pods, 90)

	as, applier := newTestAutoscaler(store, pods, stats)

	as.evaluate(context.Background())
	as.evaluate(context.Background())

	calls := applier.Calls()
	if len(calls) != 1 {
		t.Errorf("expected exactly 1 scale call (cooldown blocks 2nd), got %d: %+v", len(calls), calls)
	}
}

func TestAutoscaler_RespectsCooldownDown(t *testing.T) {
	// Symmetric to the up case: two low-CPU ticks back-to-back, the
	// scale-down cooldown (default 5m) blocks the second event.
	store := newMemStore()
	putDeploymentWithAutoscale(t, store, "prod", "api", 1, 10, 70)

	pods := makeReplicaPods("prod", "api", 5)
	stats := makeUniformStats(pods, 10)

	as, applier := newTestAutoscaler(store, pods, stats)

	as.evaluate(context.Background())
	as.evaluate(context.Background())

	calls := applier.Calls()
	if len(calls) != 1 {
		t.Errorf("expected exactly 1 scale call (cooldown blocks 2nd), got %d: %+v", len(calls), calls)
	}
}

func TestAutoscaler_NoPodsSkips(t *testing.T) {
	// Manifest exists with an autoscale block, but no running pods
	// (bootstrap state — apply just landed, reconciler hasn't spawned
	// yet). The scaler must hold silently, not error.
	store := newMemStore()
	putDeploymentWithAutoscale(t, store, "prod", "api", 2, 10, 70)

	as, applier := newTestAutoscaler(store, nil, nil)

	as.evaluate(context.Background())

	if calls := applier.Calls(); len(calls) != 0 {
		t.Errorf("expected no scale calls (bootstrap), got %+v", calls)
	}
}

func TestAutoscaler_HoldsInDeadZone(t *testing.T) {
	// CPU at 70% (exactly target), with target = 70%. 70 is neither
	// > 77 (up trigger) nor < 49 (down trigger) → hold.
	store := newMemStore()
	putDeploymentWithAutoscale(t, store, "prod", "api", 1, 10, 70)

	pods := makeReplicaPods("prod", "api", 3)
	stats := makeUniformStats(pods, 70)

	as, applier := newTestAutoscaler(store, pods, stats)

	as.evaluate(context.Background())

	if calls := applier.Calls(); len(calls) != 0 {
		t.Errorf("expected no scale calls in dead zone, got %+v", calls)
	}
}

func TestAutoscaler_IgnoresDeploymentsWithoutAutoscaleBlock(t *testing.T) {
	// Two deployments: one has an autoscale block, the other doesn't.
	// Even if the second is in a CPU range that WOULD trigger
	// scaling, the scaler must ignore it entirely (no autoscale
	// declaration = static replicas, hands off).
	store := newMemStore()
	putDeploymentWithAutoscale(t, store, "prod", "scaled", 2, 10, 70)

	// Plain deployment without autoscale.
	plainSpec, _ := json.Marshal(map[string]any{
		"image":    "x",
		"replicas": 2,
	})

	if _, err := store.Put(context.Background(), &Manifest{
		Kind:  KindDeployment,
		Scope: "prod",
		Name:  "plain",
		Spec:  plainSpec,
	}); err != nil {
		t.Fatalf("seed plain: %v", err)
	}

	// Pods + stats for both deployments at high CPU.
	scaledPods := makeReplicaPods("prod", "scaled", 2)
	plainPods := makeReplicaPods("prod", "plain", 2)

	allPods := append(scaledPods, plainPods...)

	allStats := makeUniformStats(allPods, 95)

	as, applier := newTestAutoscaler(store, allPods, allStats)

	as.evaluate(context.Background())

	calls := applier.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 scale call (only autoscaled deployment), got %d: %+v", len(calls), calls)
	}

	if calls[0].Name != "scaled" {
		t.Errorf("scaled the wrong deployment: %+v", calls[0])
	}
}

// TestAutoscaler_MeanIgnoresZeroPodsWhenMixed pins the stats-race
// guard: a freshly-spawned pod reports 0% CPU on its first sample;
// dragging the mean down by including it would either mask a real
// scale-up trigger or fire a phantom scale-down. The collector
// drops zeros only when at least one non-zero pod exists.
func TestAutoscaler_MeanIgnoresZeroPodsWhenMixed(t *testing.T) {
	store := newMemStore()
	putDeploymentWithAutoscale(t, store, "prod", "api", 1, 10, 70)

	pods := makeReplicaPods("prod", "api", 2)

	// One pod hot (90%), one pod zero (stats race). Mean across BOTH
	// = 45 (would hold). Mean across non-zero only = 90 → scale up.
	stats := map[string]docker.ContainerStats{
		pods[0].Name: makeStats(pods[0].Name, 90, 1),
		pods[1].Name: makeStats(pods[1].Name, 0, 1),
	}

	as, applier := newTestAutoscaler(store, pods, stats)

	as.evaluate(context.Background())

	calls := applier.Calls()
	if len(calls) != 1 || calls[0].Replicas != 3 {
		t.Errorf("expected scale up to 3 (zero-pod ignored), got %+v", calls)
	}
}

// TestStoreReplicasApplier_RoundTrips pins the production applier's
// behaviour: read manifest, mutate spec.replicas, put back. Other
// spec fields must survive — losing build_args / env / etc. on
// every scale event would be a quiet disaster.
func TestStoreReplicasApplier_RoundTrips(t *testing.T) {
	store := newMemStore()

	originalSpec := map[string]any{
		"image":    "ghcr.io/acme/api:1.0.0",
		"replicas": 2,
		"env": map[string]any{
			"DATABASE_URL": "postgres://...",
		},
		"build_args": map[string]any{
			"GOOS": "linux",
		},
	}

	raw, _ := json.Marshal(originalSpec)

	if _, err := store.Put(context.Background(), &Manifest{
		Kind:  KindDeployment,
		Scope: "prod",
		Name:  "api",
		Spec:  raw,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	applier := StoreReplicasApplier{Store: store}

	if err := applier.SetReplicas(context.Background(), "prod", "api", 5); err != nil {
		t.Fatalf("set replicas: %v", err)
	}

	got, err := store.Get(context.Background(), KindDeployment, "prod", "api")
	if err != nil {
		t.Fatal(err)
	}

	var updated map[string]any
	if err := json.Unmarshal(got.Spec, &updated); err != nil {
		t.Fatal(err)
	}

	if r, ok := updated["replicas"].(float64); !ok || int(r) != 5 {
		t.Errorf("replicas not updated: %v", updated["replicas"])
	}

	if img, _ := updated["image"].(string); img != "ghcr.io/acme/api:1.0.0" {
		t.Errorf("image lost: %v", updated["image"])
	}

	if env, ok := updated["env"].(map[string]any); !ok || env["DATABASE_URL"] != "postgres://..." {
		t.Errorf("env lost: %v", updated["env"])
	}

	if args, ok := updated["build_args"].(map[string]any); !ok || args["GOOS"] != "linux" {
		t.Errorf("build_args lost: %v", updated["build_args"])
	}
}

// TestStoreReplicasApplier_MissingManifestSilent pins the
// safety net: between the scaler's List and the Apply, a deployment
// might get deleted. Treating that as an error would log noise on
// every scaler tick; treating it as silent success is correct —
// there's nothing left to scale.
func TestStoreReplicasApplier_MissingManifestSilent(t *testing.T) {
	store := newMemStore()
	applier := StoreReplicasApplier{Store: store}

	if err := applier.SetReplicas(context.Background(), "prod", "ghost", 5); err != nil {
		t.Errorf("expected nil error on missing manifest, got %v", err)
	}
}
