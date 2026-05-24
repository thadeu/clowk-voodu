// handlers_metrics.go owns `GET /metrics` — the time-series query
// endpoint backing WebUI charts (and a future `vd metrics` CLI).
// Reads from the NDJSON files the background Sampler appends to;
// the request path is read-only and stateless from the controller's
// perspective.

package controller

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.voodu.clowk.in/internal/metrics"
)

// rangeAliases turns a friendly `range=1h|6h|24h|7d` query param
// into a Go duration. Operator can also pass a literal duration
// (e.g. `range=90m`) and the fallback parses it directly.
//
// Why a closed set of aliases at all (not just `time.ParseDuration`)?
//
//   - WebUI URLs read cleanly: `?range=24h` is more memorable than
//     `?range=86400s`.
//   - Caps the most-common ranges to known buckets the cleanup
//     window can serve (7d max — anything longer truncates).
//   - Lets us add server-side cache later keyed on the alias.
var rangeAliases = map[string]time.Duration{
	"15m": 15 * time.Minute,
	"30m": 30 * time.Minute,
	"1h":  time.Hour,
	"3h":  3 * time.Hour,
	"6h":  6 * time.Hour,
	"12h": 12 * time.Hour,
	"24h": 24 * time.Hour,
	"3d":  3 * 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

// intervalAliases maps friendly bucket sizes. `auto` is a sentinel
// the handler resolves to `range / MaxBuckets` rounded to a sensible
// step — kept out of this table so the resolution logic is in one
// place (autoInterval below).
var intervalAliases = map[string]time.Duration{
	"15s": 15 * time.Second,
	"30s": 30 * time.Second,
	"1m":  time.Minute,
	"5m":  5 * time.Minute,
	"15m": 15 * time.Minute,
	"1h":  time.Hour,
}

// handleMetrics returns a bucketed time-series for one metric.
//
//	GET /metrics?source=system|pod
//	             &metric=cpu_percent|...
//	             &scope=<scope>           (pod only)
//	             &name=<name>             (pod only)
//	             &range=1h|6h|24h|7d|<go-duration>
//	             &interval=auto|15s|1m|5m|<go-duration>
//
// 503 when MetricsDir isn't wired (test setups). 400 on any param
// problem — be loud about what's wrong so the WebUI dev sees it
// in the browser console. 200 + chart-ready envelope on the happy
// path; series may be empty if no data overlaps the range.
//
// Streaming-friendly response: we write the envelope via
// `json.Encoder` so the WebUI client sees bytes immediately even
// when a 7d query takes 1–3s to scan disk. The series itself is
// short enough (≤ MaxBuckets = 300 points) to fit in a single
// encode; the stream-write benefit is the TTFB headers + the open
// brace, not chunked series rendering.
func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if a.MetricsDir == "" {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("metrics store not configured"))
		return
	}

	q := r.URL.Query()

	source, err := parseSource(q.Get("source"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	metric := strings.TrimSpace(q.Get("metric"))
	if metric == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("metric is required"))
		return
	}

	if !metrics.MetricAllowed(metric) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("unknown metric %q", metric))
		return
	}

	rangeDur, err := parseMetricsRange(q.Get("range"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	now := time.Now().UTC()
	start := now.Add(-rangeDur)

	interval, err := parseInterval(q.Get("interval"), rangeDur)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	res, err := metrics.Query(metrics.QueryOpts{
		Dir:      a.MetricsDir,
		Source:   source,
		Metric:   metric,
		Scope:    strings.TrimSpace(q.Get("scope")),
		Name:     strings.TrimSpace(q.Get("name")),
		Pod:      strings.TrimSpace(q.Get("pod")),
		Start:    start,
		End:      now,
		Interval: interval,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   res,
	})
}

// parseSource validates the source param. Kept tiny so the handler
// reads as a chain of parse → call → write.
func parseSource(raw string) (metrics.Source, error) {
	switch strings.TrimSpace(raw) {
	case string(metrics.SourceSystem):
		return metrics.SourceSystem, nil
	case string(metrics.SourcePod):
		return metrics.SourcePod, nil
	default:
		return "", fmt.Errorf("source must be %q or %q", metrics.SourceSystem, metrics.SourcePod)
	}
}

// parseMetricsRange resolves the `range` query param. Empty → 1h default
// (the most common chart). Alias keys (15m, 1h, 7d, …) take
// precedence over a literal Go duration string so operators get
// the curated set on the typical paths.
func parseMetricsRange(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Hour, nil
	}

	if d, ok := rangeAliases[raw]; ok {
		return d, nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("range: %w", err)
	}

	if d <= 0 {
		return 0, fmt.Errorf("range must be positive")
	}

	// Cap so a typo like `range=70d` doesn't try to walk a year of
	// files. Caller can cap their own UI but we defend here too.
	if d > 30*24*time.Hour {
		return 0, fmt.Errorf("range max 30d")
	}

	return d, nil
}

// parseInterval resolves the `interval` query param. `auto` →
// `range / MaxBuckets` rounded to a sensible step so we always
// land at one of {15s, 30s, 1m, 5m, 15m, 1h}. Alias keys mirror
// intervalAliases; literal Go durations are accepted as the
// escape hatch.
func parseInterval(raw string, rangeDur time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "auto" {
		return autoInterval(rangeDur), nil
	}

	if d, ok := intervalAliases[raw]; ok {
		return d, nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("interval: %w", err)
	}

	if d <= 0 {
		return 0, fmt.Errorf("interval must be positive")
	}

	return d, nil
}

// autoInterval picks the step that keeps the chart under
// MaxBuckets points. Rounded UP to the next sensible step so the
// series is always a clean unit operators recognise — never
// `1m13s` from naive division. Future: this is also the seam
// where adaptive resolution (per-range memo) would land.
func autoInterval(rangeDur time.Duration) time.Duration {
	naive := rangeDur / metrics.MaxBuckets
	if naive <= 0 {
		return 15 * time.Second
	}

	for _, step := range []time.Duration{
		15 * time.Second,
		30 * time.Second,
		time.Minute,
		5 * time.Minute,
		15 * time.Minute,
		time.Hour,
	} {
		if step >= naive {
			return step
		}
	}

	return time.Hour
}
