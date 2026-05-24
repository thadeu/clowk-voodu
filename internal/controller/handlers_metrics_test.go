// Integration tests for GET /metrics. Wires a real API with a
// temp MetricsDir, seeds NDJSON fixtures, asserts the HTTP layer
// passes the right options through to metrics.Query and serialises
// the response in the WebUI's expected shape.
//
// Reader-side correctness (bucket math, partial-line tolerance,
// gzip support) is covered by internal/metrics tests; here we
// pin the HTTP boundary specifically.

package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedMetricsFixture drops a one-line ndjson file in dir. Tests
// build small fixtures inline and assert what comes back through
// the handler — keeps the wire shape pinned at the boundary.
func seedMetricsFixture(t *testing.T, dir, filename, content string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMetrics_503WhenDirMissing(t *testing.T) {
	api, _ := newTestAPI(t)
	api.MetricsDir = "" // explicit — no metrics store wired

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics?source=system&metric=cpu_percent&range=1h")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503 when MetricsDir empty", resp.StatusCode)
	}
}

func TestMetrics_400OnUnknownMetric(t *testing.T) {
	api, _ := newTestAPI(t)
	api.MetricsDir = t.TempDir()

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics?source=system&metric=_secret&range=1h")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown metric should 400, got %d", resp.StatusCode)
	}
}

func TestMetrics_400OnMissingSource(t *testing.T) {
	api, _ := newTestAPI(t)
	api.MetricsDir = t.TempDir()

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics?metric=cpu_percent&range=1h")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing source should 400, got %d", resp.StatusCode)
	}
}

// TestMetrics_HappyPathEnvelope seeds a one-sample fixture and
// asserts the response envelope shape matches what the WebUI
// reads (`data.metric`, `data.interval_seconds`, `data.series[]`).
// A drift in any of these paths slips past a typed decode but
// breaks the chart — so we assert the keys exist via a map decode.
func TestMetrics_HappyPathEnvelope(t *testing.T) {
	api, _ := newTestAPI(t)
	dir := t.TempDir()
	api.MetricsDir = dir

	// Use today's UTC date so the file overlaps a `range=1h` query.
	today := todayUTCDate(t)
	seedMetricsFixture(t, dir, "metrics-"+today+".ndjson", `{"ts":"`+nowUTCRFC3339(t)+`","source":"system","cpu_percent":12.4}
`)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics?source=system&metric=cpu_percent&range=1h")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	var env struct {
		Status string         `json:"status"`
		Data   map[string]any `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	if env.Status != "ok" {
		t.Fatalf("status: %q", env.Status)
	}

	// WebUI reads these exact paths. Catching renames here keeps
	// the chart from breaking after a Go-side refactor.
	for _, key := range []string{"metric", "interval_seconds", "series"} {
		if _, ok := env.Data[key]; !ok {
			t.Errorf("missing data.%s in %v", key, env.Data)
		}
	}

	if env.Data["metric"] != "cpu_percent" {
		t.Errorf("data.metric=%v want cpu_percent", env.Data["metric"])
	}

	series, _ := env.Data["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("series len=%d want 1: %v", len(series), series)
	}

	point := series[0].(map[string]any)
	if point["value"] != 12.4 {
		t.Errorf("value=%v want 12.4", point["value"])
	}
}

// TestMetrics_PodScopeNameFilter pins the (scope, name) filter
// works through the handler. Two pods in the fixture; query for
// scope=x must return only one in the bucket.
func TestMetrics_PodScopeNameFilter(t *testing.T) {
	api, _ := newTestAPI(t)
	dir := t.TempDir()
	api.MetricsDir = dir

	today := todayUTCDate(t)
	now := nowUTCRFC3339(t)

	seedMetricsFixture(t, dir, "metrics-"+today+".ndjson",
		`{"ts":"`+now+`","source":"pod","container":"voodu-x-web.a","kind":"deployment","scope":"x","name":"web","cpu_percent":10}
{"ts":"`+now+`","source":"pod","container":"voodu-y-web.b","kind":"deployment","scope":"y","name":"web","cpu_percent":99}
`)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics?source=pod&metric=cpu_percent&scope=x&name=web&range=1h")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var env struct {
		Data struct {
			Series []struct {
				Value float64 `json:"value"`
			} `json:"series"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)

	if len(env.Data.Series) != 1 {
		t.Fatalf("series=%d want 1", len(env.Data.Series))
	}

	if env.Data.Series[0].Value != 10 {
		t.Errorf("scope=x value=%v want 10 (NOT 99 — that's scope=y)", env.Data.Series[0].Value)
	}
}

// TestMetrics_AutoIntervalCapsToMaxBuckets — a 7d query with
// auto interval should land at a step ≥ 7d/300 (= ~33 min).
// We just assert interval_seconds is at least 1800 (30m), the
// nearest sensible step above the naive division.
func TestMetrics_AutoIntervalCapsToMaxBuckets(t *testing.T) {
	api, _ := newTestAPI(t)
	api.MetricsDir = t.TempDir()

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics?source=system&metric=cpu_percent&range=7d&interval=auto")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var env struct {
		Data struct {
			IntervalSeconds int `json:"interval_seconds"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)

	// 7d / 300 = 2016s. The picker rounds UP to the next sensible
	// step (the 1h slot in autoInterval).
	if env.Data.IntervalSeconds < 1800 {
		t.Errorf("interval_seconds=%d, want ≥ 1800 (30m) for 7d/auto", env.Data.IntervalSeconds)
	}
}

// ── helpers ───────────────────────────────────────────────────────

// todayUTCDate / nowUTCRFC3339 keep fixtures from going stale —
// using a hardcoded date would fall outside the query's `range`
// window once the calendar moves past it.
func todayUTCDate(t *testing.T) string {
	t.Helper()
	return nowUTCRFC3339(t)[:10]
}

func nowUTCRFC3339(t *testing.T) string {
	t.Helper()
	// Use time.Now() — these are server-side fixtures consumed by
	// the handler in the same wall-clock instant the test runs in.
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
