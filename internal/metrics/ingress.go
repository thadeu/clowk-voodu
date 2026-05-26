package metrics

import "time"

// SourceIngress identifies NDJSON rows produced by the ingress
// sampler (one row per host per tick, aggregated from Caddy's
// access log). Distinct from SourcePod / SourceSystem so the reader
// can filter cleanly without parsing the rest of the line.
const SourceIngress Source = "ingress"

// DefaultCaddyAccessLog is where the voodu-caddy plugin writes its
// JSON access log on the host. The plugin's install script bind-
// mounts $VOODU_CADDY_STATE_DIR/logs → /var/log/caddy inside the
// container; on the host the file lives at this path (default state
// dir is /opt/voodu/caddy). See voodu-caddy/internal/ingress/config.go
// AccessLogPath for the in-container side of the contract.
const DefaultCaddyAccessLog = "/opt/voodu/caddy/logs/access.log"

// MaxRequestsPerWindow caps how many duration samples we retain per
// (host, scope, name) bucket within a single Tick window. Beyond
// this, the aggregator drops further samples — we lose percentile
// accuracy at the tail but bound peak memory. A pathological case
// (1000 req/s × 15s = 15K) easily fits under this cap; only floods
// (looping bot, misconfigured client) blow past it, and those will
// trip 5xx-count alerts on the metrics that DO survive.
const MaxRequestsPerWindow = 50_000

// IngressSample is one row per (host, scope, name) per Tick. Pointer
// fields for the percentiles + max so the encoder omits them via
// omitempty when the window had zero requests (no durations to
// summarise) — readers/WebUI use the missing-field signal the same
// way they handle "no delta on first sample" elsewhere in this
// package.
type IngressSample struct {
	Ts     time.Time `json:"ts"`
	Source Source    `json:"source"`
	Host   string    `json:"host"`
	Scope  string    `json:"scope,omitempty"`
	Name   string    `json:"name"`

	ReqCount uint64 `json:"req_count"`
	Req2xx   uint64 `json:"req_2xx,omitempty"`
	Req3xx   uint64 `json:"req_3xx,omitempty"`
	Req4xx   uint64 `json:"req_4xx,omitempty"`
	Req5xx   uint64 `json:"req_5xx,omitempty"`

	// Latencies in milliseconds. Pointer + omitempty so a 0-request
	// window doesn't ship five zero percentile fields (would dilute
	// MAX aggregation on the WebUI side into pretending the deployment
	// had a real "0 ms p99" at that bucket).
	LatencyP50Ms *float64 `json:"latency_p50_ms,omitempty"`
	LatencyP90Ms *float64 `json:"latency_p90_ms,omitempty"`
	LatencyP95Ms *float64 `json:"latency_p95_ms,omitempty"`
	LatencyP99Ms *float64 `json:"latency_p99_ms,omitempty"`
	LatencyMaxMs *float64 `json:"latency_max_ms,omitempty"`

	// Cumulative bytes Caddy SENT to clients within the window. Useful
	// for "throughput" charts. We don't track bytes_in (request body
	// size) because Caddy's `size` field is response size only and a
	// real bytes-in metric would require parsing `bytes_read`; deferred
	// until an operator asks.
	BytesOut uint64 `json:"bytes_out,omitempty"`
}

// IngressRequest is one parsed Caddy access log line, narrowed to
// the fields the aggregator cares about. Public so tests can build
// fixtures without going through JSON parsing.
//
// URI is the request path (Caddy `request.uri`, e.g. "/api/users"
// or "/_next/static/chunks/abc.js"). Used by IsAssetRequest to
// classify the row before incrementing the page/API counter — see
// ingress_aggregator.go's Push for the skip logic.
type IngressRequest struct {
	Host       string
	URI        string
	DurationMs float64
	Status     int
	SizeBytes  uint64
}
