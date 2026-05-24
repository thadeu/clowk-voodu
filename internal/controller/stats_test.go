// Tests for StatsCollector — the controller-side join of pods +
// docker stats + manifest limits. Every test pins one behaviour:
//
//   - The filter narrows correctly (kind/scope/name + orphans)
//   - The join is robust to missing data (pod in list but no stats,
//     stats but no manifest, etc.)
//   - Limit lookup handles every kind that owns a resources block
//     (deployment, statefulset, job, cronjob's nested job)
//
// We don't shell out to docker — fakeStatsClient returns canned
// runtime data, the package-level fakePodsLister/memStore fakes
// provide identity + manifest source.

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/docker"
)

// fakeStatsClient implements docker.StatsClient by returning a
// pre-baked map of name → ContainerStats. Records the names it was
// asked for so tests can assert filter-pushdown happened.
type fakeStatsClient struct {
	byName     map[string]docker.ContainerStats
	calledWith []string
}

func (f *fakeStatsClient) ContainerStats(names []string) ([]docker.ContainerStats, error) {
	f.calledWith = append([]string(nil), names...)

	out := make([]docker.ContainerStats, 0, len(names))
	for _, n := range names {
		if s, ok := f.byName[n]; ok {
			out = append(out, s)
		}
	}

	return out, nil
}

// makeStats is a small helper to keep test fixtures readable.
// NET I/O + BLOCK I/O default to plausible cumulative byte counts
// — non-zero so tests asserting "got attached" pass a strict
// equality. Tests that don't care about them just ignore those
// fields.
func makeStats(name string, cpu float64, mem uint64) docker.ContainerStats {
	return docker.ContainerStats{
		Name:            name,
		CPUPercent:      cpu,
		MemUsageBytes:   mem,
		MemLimitBytes:   1024 * 1024 * 1024,
		MemPercent:      float64(mem) / float64(1024*1024*1024) * 100,
		PIDs:            5,
		NetRxBytes:      338_000,
		NetTxBytes:      41_700,
		BlockReadBytes:  2_040_000_000,
		BlockWriteBytes: 1_210_000,
	}
}

// makePod is a small helper mirroring makeStats.
func makePod(kind, scope, name, replicaID, container string, running bool) Pod {
	return Pod{
		Name:         container,
		Kind:         kind,
		Scope:        scope,
		ResourceName: name,
		ReplicaID:    replicaID,
		Running:      running,
	}
}

// putDeploymentWithLimits seeds the package-level memStore with a
// deployment manifest carrying optional resources.limits. Returns
// the store so callers chain the wiring.
func putDeploymentWithLimits(t *testing.T, store *memStore, scope, name, cpu, memory string) {
	t.Helper()

	spec := map[string]any{"image": "x"}

	if cpu != "" || memory != "" {
		spec["resources"] = map[string]any{
			"limits": map[string]any{
				"cpu":    cpu,
				"memory": memory,
			},
		}
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

// putCronjobWithLimits seeds a cronjob manifest with the nested
// `job.resources` shape — exercises the extra-decode path in
// extractLimits.
func putCronjobWithLimits(t *testing.T, store *memStore, scope, name, cpu, memory string) {
	t.Helper()

	spec := map[string]any{
		"schedule": "* * * * *",
		"job": map[string]any{
			"image": "x",
			"resources": map[string]any{
				"limits": map[string]any{
					"cpu":    cpu,
					"memory": memory,
				},
			},
		},
	}

	raw, _ := json.Marshal(spec)

	if _, err := store.Put(context.Background(), &Manifest{
		Kind:  KindCronJob,
		Scope: scope,
		Name:  name,
		Spec:  raw,
	}); err != nil {
		t.Fatalf("seed cronjob: %v", err)
	}
}

// TestStatsCollector_HappyPath joins identity + runtime + limits
// for one running deployment. The base case — if this breaks,
// nothing else matters.
func TestStatsCollector_HappyPath(t *testing.T) {
	pods := &fakePodsLister{pods: []Pod{
		makePod("deployment", "clowk-lp", "web", "a3f9", "voodu-clowk-lp-web.a3f9", true),
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-clowk-lp-web.a3f9": makeStats("voodu-clowk-lp-web.a3f9", 47.5, 120*1024*1024),
	}}

	store := newMemStore()
	putDeploymentWithLimits(t, store, "clowk-lp", "web", "0.4", "254Mi")

	collector := &StatsCollector{Pods: pods, Stats: stats, Store: store}

	got, err := collector.Collect(context.Background(), StatsFilter{})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(got), got)
	}

	r := got[0]
	if r.Identity.Kind != "deployment" || r.Identity.Scope != "clowk-lp" || r.Identity.Name != "web" {
		t.Errorf("identity wrong: %+v", r.Identity)
	}

	if r.Usage.CPUPercent != 47.5 {
		t.Errorf("cpu: %v", r.Usage.CPUPercent)
	}

	if r.Usage.MemoryUsageBytes != 120*1024*1024 {
		t.Errorf("mem usage: %d", r.Usage.MemoryUsageBytes)
	}

	if r.Limits.CPU != "0.4" || r.Limits.Memory != "254Mi" {
		t.Errorf("limits raw: %+v", r.Limits)
	}

	if r.Limits.MemoryBytes != 254*1024*1024 {
		t.Errorf("limits memory bytes: %d", r.Limits.MemoryBytes)
	}

	if r.Orphan {
		t.Error("matched pod must not be marked orphan")
	}
}

// TestStatsCollector_PushesNamesToDocker pins the filter pushdown:
// docker stats is called only with the names that survived the
// kind/scope/name filter, not with every running container on the
// host. On a busy host (50+ apps) this saves daemon CPU.
func TestStatsCollector_PushesNamesToDocker(t *testing.T) {
	pods := &fakePodsLister{pods: []Pod{
		makePod("deployment", "scope-a", "web", "1", "voodu-scope-a-web.1", true),
		makePod("deployment", "scope-b", "api", "1", "voodu-scope-b-api.1", true),
		makePod("statefulset", "a", "pg", "0", "voodu-data-pg.0", true),
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-scope-a-web.1": makeStats("voodu-scope-a-web.1", 1, 1),
	}}

	collector := &StatsCollector{Pods: pods, Stats: stats}

	_, err := collector.Collect(context.Background(), StatsFilter{
		Scope:   "scope-a",
		Orphans: true, // store is nil → all results would be orphans
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(stats.calledWith) != 1 || stats.calledWith[0] != "voodu-scope-a-web.1" {
		t.Errorf("filter pushdown failed; docker called with %v", stats.calledWith)
	}
}

// TestStatsCollector_DropsStoppedPods locks in "running only". Stopped
// pods have no cgroup to sample; including them as zeros would be
// confusing — operators expect docker stats parity.
func TestStatsCollector_DropsStoppedPods(t *testing.T) {
	pods := &fakePodsLister{pods: []Pod{
		makePod("deployment", "x", "running", "1", "voodu-x-running.1", true),
		makePod("deployment", "x", "stopped", "1", "voodu-x-stopped.1", false),
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-x-running.1": makeStats("voodu-x-running.1", 1, 1),
	}}

	store := newMemStore()
	putDeploymentWithLimits(t, store, "x", "running", "", "")
	putDeploymentWithLimits(t, store, "x", "stopped", "", "")

	collector := &StatsCollector{Pods: pods, Stats: stats, Store: store}

	got, err := collector.Collect(context.Background(), StatsFilter{})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 (running), got %d", len(got))
	}

	if got[0].Identity.Name != "running" {
		t.Errorf("returned the wrong pod: %+v", got[0].Identity)
	}
}

// TestStatsCollector_OrphanWithoutManifest covers the leak case:
// container exists with full voodu labels, but the manifest was
// deleted. Default (Orphans=false) should hide it; --orphans
// surfaces it.
func TestStatsCollector_OrphanWithoutManifest(t *testing.T) {
	pods := &fakePodsLister{pods: []Pod{
		makePod("deployment", "ghost", "leaked", "1", "voodu-ghost-leaked.1", true),
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-ghost-leaked.1": makeStats("voodu-ghost-leaked.1", 1, 1),
	}}

	// Empty store — the manifest doesn't exist.
	store := newMemStore()

	collector := &StatsCollector{Pods: pods, Stats: stats, Store: store}

	// Default: orphan hidden.
	got, err := collector.Collect(context.Background(), StatsFilter{})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 0 {
		t.Errorf("default should hide orphans, got %+v", got)
	}

	// --orphans: surfaced with Orphan=true.
	got, err = collector.Collect(context.Background(), StatsFilter{Orphans: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || !got[0].Orphan {
		t.Errorf("orphans flag should surface leaked pod: %+v", got)
	}

	if got[0].Limits.CPU != "" || got[0].Limits.MemoryBytes != 0 {
		t.Errorf("orphan must have empty limits: %+v", got[0].Limits)
	}
}

// TestStatsCollector_LegacyContainer covers pre-M0 containers —
// have the umbrella createdby=voodu label but no voodu.kind.
// Treated as orphans because nothing identifies them; --orphans
// surfaces them so operators can see what still needs a re-apply.
func TestStatsCollector_LegacyContainer(t *testing.T) {
	pods := &fakePodsLister{pods: []Pod{
		// Kind="" → legacy
		{Name: "voodu-legacy-app", Running: true},
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-legacy-app": makeStats("voodu-legacy-app", 1, 1),
	}}

	collector := &StatsCollector{Pods: pods, Stats: stats}

	got, _ := collector.Collect(context.Background(), StatsFilter{})
	if len(got) != 0 {
		t.Errorf("legacy hidden by default, got %+v", got)
	}

	got, _ = collector.Collect(context.Background(), StatsFilter{Orphans: true})
	if len(got) != 1 || !got[0].Orphan {
		t.Errorf("legacy should surface under --orphans: %+v", got)
	}
}

// TestStatsCollector_NoLimitsBlockIsNotOrphan ensures a deployment
// that exists in the store but has NO `resources {}` block isn't
// mistaken for an orphan. Empty limits is a valid state — the
// operator chose not to cap.
func TestStatsCollector_NoLimitsBlockIsNotOrphan(t *testing.T) {
	pods := &fakePodsLister{pods: []Pod{
		makePod("deployment", "x", "uncapped", "1", "voodu-x-uncapped.1", true),
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-x-uncapped.1": makeStats("voodu-x-uncapped.1", 1, 1),
	}}

	store := newMemStore()
	putDeploymentWithLimits(t, store, "x", "uncapped", "", "")

	collector := &StatsCollector{Pods: pods, Stats: stats, Store: store}

	got, err := collector.Collect(context.Background(), StatsFilter{})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}

	if got[0].Orphan {
		t.Error("declared but uncapped pod must not be orphan")
	}

	if got[0].Limits.CPU != "" || got[0].Limits.Memory != "" {
		t.Errorf("limits should be empty: %+v", got[0].Limits)
	}
}

// TestStatsCollector_CronjobNestedResources pins the cronjob shape
// — the resources block lives under `job` on the wire, not at root.
// Without the nested-decode path in extractLimits, cronjob stats
// would always show empty limits despite the manifest having them.
func TestStatsCollector_CronjobNestedResources(t *testing.T) {
	pods := &fakePodsLister{pods: []Pod{
		makePod("cronjob", "ops", "backup", "abcd", "voodu-ops-backup.abcd", true),
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-ops-backup.abcd": makeStats("voodu-ops-backup.abcd", 1, 1),
	}}

	store := newMemStore()
	putCronjobWithLimits(t, store, "ops", "backup", "0.5", "128Mi")

	collector := &StatsCollector{Pods: pods, Stats: stats, Store: store}

	got, err := collector.Collect(context.Background(), StatsFilter{})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatal("expected one row")
	}

	if got[0].Limits.CPU != "0.5" || got[0].Limits.Memory != "128Mi" {
		t.Errorf("nested resources lookup failed: %+v", got[0].Limits)
	}

	if got[0].Limits.MemoryBytes != 128*1024*1024 {
		t.Errorf("memory bytes: %d", got[0].Limits.MemoryBytes)
	}
}

// TestStatsCollector_StatsRaceSkipsRow pins the race where a pod
// appears in ListPods but disappears from docker stats — happens
// mid-restart. The collector must skip that row (next refresh
// catches it) rather than emit a zeroed entry.
func TestStatsCollector_StatsRaceSkipsRow(t *testing.T) {
	pods := &fakePodsLister{pods: []Pod{
		makePod("deployment", "x", "racy", "1", "voodu-x-racy.1", true),
	}}

	// Empty stats response — simulates the race.
	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{}}

	collector := &StatsCollector{Pods: pods, Stats: stats}

	got, err := collector.Collect(context.Background(), StatsFilter{Orphans: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 0 {
		t.Errorf("racy pod should be skipped, got %+v", got)
	}
}

// TestFilterPods_FieldVocabulary pins the filter semantics: each
// non-empty filter field narrows; empty = wildcard; combinations
// AND together. Direct unit test (separate from Collect) so a
// future refactor that touches filter logic shows up here first.
func TestFilterPods_FieldVocabulary(t *testing.T) {
	pods := []Pod{
		makePod("deployment", "a", "web", "1", "a-web.1", true),
		makePod("deployment", "a", "api", "1", "a-api.1", true),
		makePod("statefulset", "a", "pg", "0", "a-pg.0", true),
		makePod("deployment", "b", "web", "1", "b-web.1", true),
	}

	cases := []struct {
		name   string
		filter StatsFilter
		want   int
	}{
		{"no filter → all", StatsFilter{}, 4},
		{"kind only", StatsFilter{Kind: "deployment"}, 3},
		{"scope only", StatsFilter{Scope: "a"}, 3},
		{"name only", StatsFilter{Name: "web"}, 2},
		{"kind+scope", StatsFilter{Kind: "deployment", Scope: "a"}, 2},
		{"scope+name", StatsFilter{Scope: "a", Name: "web"}, 1},
		{"all three", StatsFilter{Kind: "deployment", Scope: "a", Name: "web"}, 1},
		{"no matches", StatsFilter{Kind: "deployment", Scope: "z"}, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Copy because filterPods uses pods[:0] under the hood.
			input := append([]Pod(nil), pods...)
			got := filterPods(input, c.filter)

			if len(got) != c.want {
				t.Errorf("got %d, want %d: %+v", len(got), c.want, got)
			}
		})
	}
}

// TestStatsCollector_SnapshotEmptyBeforeRefresh pins the first-boot
// case: SnapshotPods returns ok=false when RefreshSnapshot has never
// run. Callers (handleStats, enrichPods, autoscaler) rely on this to
// know they need to fall back to a live Collect.
func TestStatsCollector_SnapshotEmptyBeforeRefresh(t *testing.T) {
	collector := &StatsCollector{
		Pods:  &fakePodsLister{},
		Stats: &fakeStatsClient{},
	}

	_, _, ok := collector.SnapshotPods(StatsFilter{})
	if ok {
		t.Error("snapshot should report ok=false before first RefreshSnapshot")
	}
}

// TestStatsCollector_RefreshAndSnapshotRoundtrip exercises the
// happy path: RefreshSnapshot pulls live data, stores it, and the
// next SnapshotPods returns the same rows plus a sample timestamp
// no older than the call.
func TestStatsCollector_RefreshAndSnapshotRoundtrip(t *testing.T) {
	pods := &fakePodsLister{pods: []Pod{
		makePod("deployment", "clowk-lp", "web", "a3f9", "voodu-clowk-lp-web.a3f9", true),
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-clowk-lp-web.a3f9": makeStats("voodu-clowk-lp-web.a3f9", 47.5, 120*1024*1024),
	}}

	store := newMemStore()
	putDeploymentWithLimits(t, store, "clowk-lp", "web", "0.4", "254Mi")

	collector := &StatsCollector{Pods: pods, Stats: stats, Store: store}

	before := time.Now().Add(-time.Second)

	rows, err := collector.RefreshSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 {
		t.Fatalf("RefreshSnapshot returned %d rows, want 1", len(rows))
	}

	got, ts, ok := collector.SnapshotPods(StatsFilter{})
	if !ok {
		t.Fatal("snapshot should be populated after RefreshSnapshot")
	}

	if len(got) != 1 {
		t.Fatalf("SnapshotPods returned %d rows, want 1", len(got))
	}

	if got[0].Identity.Name != "web" || got[0].Usage.CPUPercent != 47.5 {
		t.Errorf("snapshot row content drifted: %+v", got[0])
	}

	if ts.Before(before) {
		t.Errorf("snapshot timestamp %v should be after %v", ts, before)
	}
}

// TestStatsCollector_SnapshotFilterMath pins the in-memory filter
// applied at read time. The snapshot always stores Orphans=true
// (every pod the host has) so SnapshotPods can narrow without
// re-collecting.
func TestStatsCollector_SnapshotFilterMath(t *testing.T) {
	pods := &fakePodsLister{pods: []Pod{
		makePod("deployment", "scope-a", "web", "1", "scope-a-web.1", true),
		makePod("deployment", "scope-b", "web", "1", "scope-b-web.1", true),
		makePod("statefulset", "scope-a", "pg", "0", "scope-a-pg.0", true),
		// Legacy / orphan — only surfaces under Orphans=true.
		{Name: "legacy", Running: true},
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"scope-a-web.1": makeStats("scope-a-web.1", 1, 1),
		"scope-b-web.1": makeStats("scope-b-web.1", 1, 1),
		"scope-a-pg.0":  makeStats("scope-a-pg.0", 1, 1),
		"legacy":        makeStats("legacy", 1, 1),
	}}

	store := newMemStore()
	putDeploymentWithLimits(t, store, "scope-a", "web", "", "")
	putDeploymentWithLimits(t, store, "scope-b", "web", "", "")
	// statefulset path: use deployment store helper isn't right; just
	// leave pg as orphan (no manifest) so it ALSO requires Orphans=true.

	collector := &StatsCollector{Pods: pods, Stats: stats, Store: store}

	if _, err := collector.RefreshSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		filter StatsFilter
		want   int
	}{
		{"orphans included → all 4", StatsFilter{Orphans: true}, 4},
		{"no orphans → only managed", StatsFilter{}, 2},
		{"by scope (managed)", StatsFilter{Scope: "scope-a"}, 1},
		{"by scope + orphans", StatsFilter{Scope: "scope-a", Orphans: true}, 2},
		{"by kind", StatsFilter{Kind: "deployment"}, 2},
		{"by name", StatsFilter{Name: "web"}, 2},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, ok := collector.SnapshotPods(c.filter)
			if !ok {
				t.Fatal("snapshot not populated")
			}

			if len(got) != c.want {
				t.Errorf("got %d pods, want %d: %+v", len(got), c.want, got)
			}
		})
	}
}

// TestStatsCollector_SnapshotIsolation pins that mutating the
// returned slice doesn't poison the cached entry — important
// because handlers may sort or filter after the call.
func TestStatsCollector_SnapshotIsolation(t *testing.T) {
	pods := &fakePodsLister{pods: []Pod{
		makePod("deployment", "x", "a", "1", "x-a.1", true),
		makePod("deployment", "x", "b", "1", "x-b.1", true),
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"x-a.1": makeStats("x-a.1", 1, 1),
		"x-b.1": makeStats("x-b.1", 1, 1),
	}}

	store := newMemStore()
	putDeploymentWithLimits(t, store, "x", "a", "", "")
	putDeploymentWithLimits(t, store, "x", "b", "", "")

	collector := &StatsCollector{Pods: pods, Stats: stats, Store: store}

	if _, err := collector.RefreshSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}

	first, _, _ := collector.SnapshotPods(StatsFilter{})
	if len(first) != 2 {
		t.Fatalf("got %d, want 2", len(first))
	}

	// Mutate the returned slice.
	first[0].Usage.CPUPercent = 999

	second, _, _ := collector.SnapshotPods(StatsFilter{})
	if second[0].Usage.CPUPercent == 999 {
		t.Error("snapshot leaked: mutating returned slice corrupted cache")
	}
}
