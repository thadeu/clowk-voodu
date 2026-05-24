// Tests for the /pods?detail=true stats join. Pins:
//
//   - When a StatsCollector is wired and matches the pod by container
//     name, each PodDetail.Stats carries the real Usage + Limits
//     (the WebUI then drops mock_pod_cpu / mock_pod_mem).
//   - When the collector is absent, the field is omitted from JSON
//     (omitempty); the response wire-shape is backward compatible
//     with pre-W6 consumers.
//   - When the collector returns no row for a pod (orphan filter out,
//     race with delete), that pod's slot has Stats=nil while the
//     rest of the list still ships the join.

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.voodu.clowk.in/internal/docker"
)

// TestPodsDetail_AttachesPodStatsWhenCollectorWired is the happy path:
// the WebUI calls /pods?detail=true and gets per-pod CPU/Mem usage +
// declared limits without spending a second /stats round-trip.
func TestPodsDetail_AttachesPodStatsWhenCollectorWired(t *testing.T) {
	api, store := newTestAPI(t)

	pods := &fakePodsLister{
		pods: []Pod{
			makePod("deployment", "clowk-lp", "web", "a3f9", "voodu-clowk-lp-web.a3f9", true),
		},
		details: map[string]*PodDetail{
			"voodu-clowk-lp-web.a3f9": {
				Pod: Pod{Name: "voodu-clowk-lp-web.a3f9", Image: "web:1"},
				ID:  "abc",
			},
		},
	}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-clowk-lp-web.a3f9": makeStats("voodu-clowk-lp-web.a3f9", 12.5, 100*1024*1024),
	}}

	putDeploymentWithLimits(t, store, "clowk-lp", "web", "0.4", "254Mi")

	api.Pods = pods
	api.Stats = &StatsCollector{Pods: pods, Stats: stats, Store: store}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods?detail=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	var env struct {
		Data struct {
			Pods []struct {
				Name  string            `json:"name"`
				Stats *PodStatsSnapshot `json:"stats"`
			} `json:"pods"`
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(env.Data.Pods) != 1 {
		t.Fatalf("pods=%d want 1", len(env.Data.Pods))
	}

	got := env.Data.Pods[0]
	if got.Stats == nil {
		t.Fatalf("Stats nil — expected the join to attach a snapshot")
	}

	if got.Stats.Usage.CPUPercent != 12.5 {
		t.Errorf("CPUPercent=%v want 12.5", got.Stats.Usage.CPUPercent)
	}

	if got.Stats.Usage.MemoryUsageBytes != 100*1024*1024 {
		t.Errorf("MemoryUsageBytes=%d want %d", got.Stats.Usage.MemoryUsageBytes, 100*1024*1024)
	}

	if got.Stats.Limits.CPU != "0.4" {
		t.Errorf("Limits.CPU=%q want 0.4", got.Stats.Limits.CPU)
	}

	if got.Stats.Limits.Memory != "254Mi" {
		t.Errorf("Limits.Memory=%q want 254Mi", got.Stats.Limits.Memory)
	}

	// W7: NET/BLOCK I/O cumulative bytes ride alongside CPU/Mem.
	// Same makeStats fixture sets these — assert they survive the
	// docker.ContainerStats → UsageStats → PodStatsSnapshot copy
	// path end-to-end.
	if got.Stats.Usage.NetRxBytes != 338_000 {
		t.Errorf("NetRxBytes=%d want 338000", got.Stats.Usage.NetRxBytes)
	}

	if got.Stats.Usage.NetTxBytes != 41_700 {
		t.Errorf("NetTxBytes=%d want 41700", got.Stats.Usage.NetTxBytes)
	}

	if got.Stats.Usage.BlockReadBytes != 2_040_000_000 {
		t.Errorf("BlockReadBytes=%d want 2_040_000_000", got.Stats.Usage.BlockReadBytes)
	}

	if got.Stats.Usage.BlockWriteBytes != 1_210_000 {
		t.Errorf("BlockWriteBytes=%d want 1_210_000", got.Stats.Usage.BlockWriteBytes)
	}
}

// TestPodsDetail_OmitsStatsWhenCollectorMissing pins backward
// compat — a controller without a StatsCollector keeps emitting
// the pre-W6 wire shape (no `stats` key in the JSON). Consumers
// that haven't been updated to read the new field still work.
func TestPodsDetail_OmitsStatsWhenCollectorMissing(t *testing.T) {
	api, _ := newTestAPI(t)

	api.Pods = &fakePodsLister{
		pods: []Pod{
			makePod("deployment", "x", "web", "a", "voodu-x-web.a", true),
		},
		details: map[string]*PodDetail{
			"voodu-x-web.a": {Pod: Pod{Name: "voodu-x-web.a"}, ID: "id"},
		},
	}
	// Note: api.Stats deliberately nil.

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods?detail=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := decodeBodyMap(resp)
	pods := bodyDataPods(body)
	if len(pods) != 1 {
		t.Fatalf("pods=%d want 1", len(pods))
	}

	if _, present := pods[0]["stats"]; present {
		t.Errorf("expected `stats` key absent when collector nil; got %v", pods[0])
	}
}

// TestPodsDetail_PartialMatchLeavesUnmatchedStatsNil — when the
// collector returns rows for SOME pods (race with container delete,
// orphan filter, etc.), the matched slots get .Stats populated and
// the unmatched slots keep .Stats nil. The list as a whole still
// returns.
func TestPodsDetail_PartialMatchLeavesUnmatchedStatsNil(t *testing.T) {
	api, store := newTestAPI(t)

	pods := &fakePodsLister{
		pods: []Pod{
			makePod("deployment", "x", "web", "a", "voodu-x-web.a", true),
			makePod("deployment", "x", "api", "b", "voodu-x-api.b", true),
		},
		details: map[string]*PodDetail{
			"voodu-x-web.a": {Pod: Pod{Name: "voodu-x-web.a"}, ID: "1"},
			"voodu-x-api.b": {Pod: Pod{Name: "voodu-x-api.b"}, ID: "2"},
		},
	}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		// Only the web pod is in the stats sample — `api` vanished
		// between list and stats. The api row should still ship with
		// Stats=nil.
		"voodu-x-web.a": makeStats("voodu-x-web.a", 5.0, 50*1024*1024),
	}}

	api.Pods = pods
	api.Stats = &StatsCollector{Pods: pods, Stats: stats, Store: store}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods?detail=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := decodeBodyMap(resp)
	got := bodyDataPods(body)

	var web, api1 map[string]any
	for _, p := range got {
		if p["name"] == "voodu-x-web.a" {
			web = p
		}
		if p["name"] == "voodu-x-api.b" {
			api1 = p
		}
	}

	if web == nil || api1 == nil {
		t.Fatalf("missing pods in response: %v", got)
	}

	if web["stats"] == nil {
		t.Errorf("web pod should have stats: %v", web)
	}

	if api1["stats"] != nil {
		t.Errorf("api pod should have stats absent: %v", api1)
	}
}

// TestPodDescribe_AttachesPodStatsWhenCollectorWired pins the
// single-pod endpoint /pods/{name} — without this the table list
// (/pods?detail=true) would show real CPU/Mem while the detail
// view (/pods/{name}) ships `stats: null`, leaving the WebUI's
// pod show page rendering 0% / 0 MB and drifting from the table.
func TestPodDescribe_AttachesPodStatsWhenCollectorWired(t *testing.T) {
	api, store := newTestAPI(t)

	pods := &fakePodsLister{
		pods: []Pod{
			makePod("deployment", "clowk-lp", "web", "a3f9", "voodu-clowk-lp-web.a3f9", true),
		},
		details: map[string]*PodDetail{
			"voodu-clowk-lp-web.a3f9": {
				Pod: Pod{Name: "voodu-clowk-lp-web.a3f9", Image: "web:1"},
				ID:  "abc",
			},
		},
	}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-clowk-lp-web.a3f9": makeStats("voodu-clowk-lp-web.a3f9", 22.0, 200*1024*1024),
	}}

	putDeploymentWithLimits(t, store, "clowk-lp", "web", "1.0", "512Mi")

	api.Pods = pods
	api.Stats = &StatsCollector{Pods: pods, Stats: stats, Store: store}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/voodu-clowk-lp-web.a3f9")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	var env struct {
		Data struct {
			Pod struct {
				Name  string            `json:"name"`
				Stats *PodStatsSnapshot `json:"stats"`
			} `json:"pod"`
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if env.Data.Pod.Stats == nil {
		t.Fatalf("Stats nil — describe endpoint must attach the same snapshot as /pods?detail=true")
	}

	if env.Data.Pod.Stats.Usage.CPUPercent != 22.0 {
		t.Errorf("CPUPercent=%v want 22.0", env.Data.Pod.Stats.Usage.CPUPercent)
	}

	if env.Data.Pod.Stats.Usage.MemoryUsageBytes != 200*1024*1024 {
		t.Errorf("MemoryUsageBytes=%d", env.Data.Pod.Stats.Usage.MemoryUsageBytes)
	}

	if env.Data.Pod.Stats.Limits.CPU != "1.0" {
		t.Errorf("Limits.CPU=%q want 1.0", env.Data.Pod.Stats.Limits.CPU)
	}

	if env.Data.Pod.Stats.Limits.Memory != "512Mi" {
		t.Errorf("Limits.Memory=%q want 512Mi", env.Data.Pod.Stats.Limits.Memory)
	}
}

// TestPodDescribe_OmitsStatsWhenCollectorMissing — backward compat
// for the single-pod path. Without StatsCollector the response
// keeps its pre-W6 shape (no `stats` key).
func TestPodDescribe_OmitsStatsWhenCollectorMissing(t *testing.T) {
	api, _ := newTestAPI(t)

	api.Pods = &fakePodsLister{
		pods: []Pod{
			makePod("deployment", "x", "web", "a", "voodu-x-web.a", true),
		},
		details: map[string]*PodDetail{
			"voodu-x-web.a": {Pod: Pod{Name: "voodu-x-web.a"}, ID: "id"},
		},
	}
	// api.Stats deliberately nil.

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/voodu-x-web.a")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := decodeBodyMap(resp)
	data, _ := body["data"].(map[string]any)
	pod, _ := data["pod"].(map[string]any)

	if _, present := pod["stats"]; present {
		t.Errorf("expected `stats` key absent on /pods/{name} when collector nil; got %v", pod)
	}
}

// decodeBodyMap is a tiny helper — generic map decode lets the
// partial-match test assert KEY ABSENCE in the JSON, which a typed
// struct decode silently papers over.
func decodeBodyMap(resp *http.Response) (map[string]any, error) {
	var m map[string]any
	err := json.NewDecoder(resp.Body).Decode(&m)
	return m, err
}

func bodyDataPods(body map[string]any) []map[string]any {
	data, _ := body["data"].(map[string]any)
	raw, _ := data["pods"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}

	return out
}
