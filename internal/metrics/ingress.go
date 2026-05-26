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

// IngressSample is one row per (host, scope, name) per Tick.
//
// Heartbeat-zero contract: EVERY numeric field carries a value on
// EVERY row, even on quiet windows where no traffic was observed.
// No `omitempty`, no pointer-nil tricks. A heartbeat row literally
// reads `..."req_count":0,"req_5xx":0,"latency_p95_ms":0,...` and
// the chart renderer interprets a continuous time series rather
// than freezing on the last burst.
//
// Trade-offs documented:
//
//   - MAX aggregation downstream is unaffected (0 stays below any
//     real latency).
//   - MIN/AVG aggregation OVER WINDOWS THAT MIX HEARTBEATS AND
//     REAL TRAFFIC will be dragged toward zero (e.g. one 100ms
//     request + nineteen 0ms heartbeats = avg 5ms, looks like
//     "really fast" when it actually means "almost no traffic").
//     Acceptable: operators reading p95 charts care about the
//     TAIL during loaded windows, not the average during quiet
//     ones. If a future view wants "p95 of windows with traffic
//     only," the warehouse can filter `req_count > 0` at query
//     time without changing the wire shape.
//   - Disk: each row ~10–15 extra bytes vs the previous omit-
//     based shape. Negligible against the 7d retention + per-
//     tick rate, and the win in chart continuity is worth it.
type IngressSample struct {
	Ts     time.Time `json:"ts"`
	Source Source    `json:"source"`
	Host   string    `json:"host"`
	Scope  string    `json:"scope,omitempty"`
	Name   string    `json:"name"`

	ReqCount uint64 `json:"req_count"`
	Req2xx   uint64 `json:"req_2xx"`
	Req3xx   uint64 `json:"req_3xx"`
	Req4xx   uint64 `json:"req_4xx"`
	Req5xx   uint64 `json:"req_5xx"`

	// Latencies in milliseconds. Plain float64 (not *float64) — see
	// the heartbeat-zero contract above for why 0 is a load-bearing
	// value here.
	LatencyP50Ms float64 `json:"latency_p50_ms"`
	LatencyP90Ms float64 `json:"latency_p90_ms"`
	LatencyP95Ms float64 `json:"latency_p95_ms"`
	LatencyP99Ms float64 `json:"latency_p99_ms"`
	LatencyMaxMs float64 `json:"latency_max_ms"`

	// Cumulative bytes Caddy SENT to clients within the window. We
	// don't track bytes_in (request body size) because Caddy's
	// `size` field is response size only and a real bytes-in metric
	// would require parsing `bytes_read`; deferred until an operator
	// asks.
	BytesOut uint64 `json:"bytes_out"`
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
