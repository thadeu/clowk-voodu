// Tests for GET /stats — verifies the HTTP envelope shape, query-
// param decoding, 503 when StatsCollector is missing, and that the
// underlying StatsCollector receives the parsed filter verbatim.
//
// Stats logic is exercised in stats_test.go; this file pins the
// HTTP boundary specifically.

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.voodu.clowk.in/internal/docker"
)

func TestStats_503WhenCollectorMissing(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

func TestStats_HappyPathReturnsJoinedShape(t *testing.T) {
	api, store := newTestAPI(t)

	pods := &fakePodsLister{pods: []Pod{
		makePod("deployment", "clowk-lp", "web", "a3f9", "voodu-clowk-lp-web.a3f9", true),
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-clowk-lp-web.a3f9": makeStats("voodu-clowk-lp-web.a3f9", 12.5, 100*1024*1024),
	}}

	putDeploymentWithLimits(t, store, "clowk-lp", "web", "0.4", "254Mi")

	api.Stats = &StatsCollector{Pods: pods, Stats: stats, Store: store}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	var env struct {
		Status string
		Data   struct {
			Pods []PodStats
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	if env.Status != "ok" || len(env.Data.Pods) != 1 {
		t.Fatalf("unexpected envelope: %+v", env)
	}

	p := env.Data.Pods[0]
	if p.Identity.Kind != "deployment" || p.Identity.Name != "web" {
		t.Errorf("identity wrong: %+v", p.Identity)
	}

	if p.Usage.CPUPercent != 12.5 {
		t.Errorf("cpu: %v", p.Usage.CPUPercent)
	}

	if p.Limits.Memory != "254Mi" {
		t.Errorf("limits: %+v", p.Limits)
	}
}

// TestStats_FiltersFromQuery exercises the URL → StatsFilter
// decode. Each parameter is parsed; orphans accepts both "true"
// and "1" for ergonomics.
func TestStats_FiltersFromQuery(t *testing.T) {
	api, _ := newTestAPI(t)

	pods := &fakePodsLister{pods: []Pod{
		makePod("deployment", "scope-a", "web", "1", "scope-a-web.1", true),
		makePod("deployment", "scope-b", "web", "1", "scope-b-web.1", true),
		makePod("statefulset", "scope-a", "pg", "0", "scope-a-pg.0", true),
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"scope-a-web.1": makeStats("scope-a-web.1", 1, 1),
		"scope-b-web.1": makeStats("scope-b-web.1", 1, 1),
		"scope-a-pg.0":  makeStats("scope-a-pg.0", 1, 1),
	}}

	api.Stats = &StatsCollector{Pods: pods, Stats: stats}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	cases := []struct {
		name       string
		query      string
		wantPods   int
		wantScopes []string
	}{
		{"no filter, orphans implicit-off → 0", "?orphans=true", 3, []string{"scope-a", "scope-a", "scope-b"}},
		{"by scope", "?scope=scope-a&orphans=true", 2, []string{"scope-a", "scope-a"}},
		{"by kind", "?kind=deployment&orphans=true", 2, nil},
		{"orphans=1 alias", "?orphans=1&kind=deployment", 2, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/stats" + c.query)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}

			var env struct {
				Data struct {
					Pods []PodStats
				}
			}

			_ = json.NewDecoder(resp.Body).Decode(&env)

			if len(env.Data.Pods) != c.wantPods {
				t.Errorf("got %d pods, want %d (%+v)", len(env.Data.Pods), c.wantPods, env.Data.Pods)
			}
		})
	}
}

// TestStats_OrphansDefaultExcludes pins that the default response
// (no ?orphans param) HIDES pods missing voodu identity, matching
// the design decision (option c with flag opt-in).
func TestStats_OrphansDefaultExcludes(t *testing.T) {
	api, _ := newTestAPI(t)

	pods := &fakePodsLister{pods: []Pod{
		// Legacy: Kind="" → orphan
		{Name: "voodu-legacy", Running: true},
	}}

	stats := &fakeStatsClient{byName: map[string]docker.ContainerStats{
		"voodu-legacy": makeStats("voodu-legacy", 1, 1),
	}}

	api.Stats = &StatsCollector{Pods: pods, Stats: stats}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	// Default: legacy hidden.
	resp, err := http.Get(ts.URL + "/stats")

	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}

	defer resp.Body.Close()

	var env struct {
		Data struct {
			Pods []PodStats
		}
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if len(env.Data.Pods) != 0 {
		t.Errorf("default should hide orphans; got %+v", env.Data.Pods)
	}

	// With ?orphans=true: surfaces.
	resp2, err := http.Get(ts.URL + "/stats?orphans=true")

	if err != nil {
		t.Fatalf("GET /stats?orphans=true: %v", err)
	}

	defer resp2.Body.Close()

	_ = json.NewDecoder(resp2.Body).Decode(&env)

	if len(env.Data.Pods) != 1 || !env.Data.Pods[0].Orphan {
		t.Errorf("orphans=true should surface legacy: %+v", env.Data.Pods)
	}
}
