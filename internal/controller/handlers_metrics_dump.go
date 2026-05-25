// handlers_metrics_dump.go owns `GET /metrics/dump` — incremental
// NDJSON stream for the WebUI's local metrics warehouse. Pairs with
// `internal/metrics/dump.go` for the file-streaming logic.
//
// This is intentionally a separate handler from `/metrics` because
// the two have different contracts:
//
//   - `/metrics` returns a bucketed, range-query, single-metric
//     envelope (JSON object) — the chart-ready path.
//   - `/metrics/dump` streams ALL metric lines, raw, since a
//     timestamp — the warehouse-sync path.
//
// Same on-disk store, two read paths, no shared coupling.

package controller

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.voodu.clowk.in/internal/metrics"
)

// handleMetricsDump streams raw NDJSON lines newer than `since` from
// the on-disk metrics store.
//
//	GET /metrics/dump?since=<unix_ts>
//
// Response Content-Type is `application/x-ndjson` — one JSON object
// per line, newline-delimited, NOT a JSON array. Chunked transfer
// so the WebUI client can parse as bytes arrive instead of buffering
// the whole response.
//
// `since` is unix seconds. Empty / missing / 0 → dump everything
// in the retention window (7d default). The WebUI's
// MetricsSyncIslandJob passes MAX(ts_epoch) from its local warehouse;
// the first-ever sync per island sends 0 which triggers the backfill
// path.
//
// 503 when MetricsDir isn't wired (test setups). 400 on a malformed
// since param (negative, non-numeric — we'd rather the WebUI dev see
// the bug than silently re-pull 7d every tick). 200 + streaming
// response on the happy path; an empty dump is 200 with zero bytes
// after headers (not 204 — keeps streaming HTTP clients happy).
func (a *API) handleMetricsDump(w http.ResponseWriter, r *http.Request) {
	if a.MetricsDir == "" {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("metrics store not configured"))
		return
	}

	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// Set headers BEFORE the first byte. application/x-ndjson lets
	// the WebUI pick a streaming parser without sniffing the body.
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	// no-store because the warehouse-sync sender (the WebUI's
	// MetricsSyncIslandJob) already memo's via the local SQLite —
	// caching at the HTTP layer would just dilute the picture.
	w.Header().Set("Cache-Control", "no-store")
	// No Content-Length — Go switches to Transfer-Encoding: chunked
	// when we don't set it, which is what we want for streaming.

	w.WriteHeader(http.StatusOK)

	if err := metrics.Dump(w, metrics.DumpOpts{
		Dir:   a.MetricsDir,
		Since: since,
	}); err != nil {
		// Headers + status already flushed; can't change them. The
		// WebUI sees a truncated stream and retries on the next
		// tick (with the same Since, so nothing's lost).
		return
	}
}

// parseSince accepts `since` as unix seconds. Empty → zero Time
// (caller treats as backfill / dump-everything-in-retention).
// Negative or non-numeric → 400. We're strict here on purpose: a
// silently-zero default would mask the bug of "WebUI sent something
// it computed wrong" and we'd pay it as 7d full pulls every tick.
func parseSince(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}

	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("since: %w", err)
	}

	if n < 0 {
		return time.Time{}, fmt.Errorf("since must be non-negative unix seconds")
	}

	if n == 0 {
		return time.Time{}, nil
	}

	return time.Unix(n, 0).UTC(), nil
}
