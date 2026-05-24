// Package metrics is the controller-side time-series store.
//
// Architecture in one paragraph: a single background sampler ticks
// every N seconds (15s default), pulls the host snapshot from the
// systemstats Collector and per-container snapshots from the
// StatsCollector, and appends one NDJSON line per source to a daily
// file under `<VOODU_ROOT>/cache/metrics/metrics-YYYY-MM-DD.ndjson`.
// A reader streams those files on demand, parses only the requested
// metric field, buckets into a fixed-size series (≤300 points), and
// returns chart-ready data. A cleanup pass on each tick gzips
// yesterday's file and unlinks anything older than the retention
// window (7 days default).
//
// Why NDJSON on disk instead of etcd / a TSDB / SQL?
//
//   - etcd is for low-cardinality metadata (manifests, PATs); a
//     ~1 write/sec stream of metrics would bloat the etcd log and
//     compaction surface for no query benefit.
//   - A real TSDB (Prometheus, VictoriaMetrics, InfluxDB) is a
//     separate daemon — operational weight we don't want for a
//     single-VM controller.
//   - SQL adds schema migrations + a query layer we don't need.
//   - NDJSON is the simplest format that survives ad-hoc inspection
//     (operators can `cat`, `grep`, `jq` the files) and a future
//     DuckDB query layer can read it natively without reformat.
//
// Identity / chart axis:
//
//   Pod entries carry BOTH (kind, scope, name) AND (container,
//   replica_id). Chart aggregation queries group on (scope, name)
//   so a chart for "the web deployment in scope X" survives
//   container restarts and replica scale-up (replica_id is
//   regenerated per spawn — see internal/containers/labels.go).
//   Drill-down to a specific replica filters on replica_id.
//
// Counter semantics:
//
//   net_*_bytes and block_*_bytes are CUMULATIVE since container
//   start (matching `docker stats`). The sampler ALSO writes
//   *_delta_bytes (the difference vs the prior sample for the same
//   container) so charts can render either rate or total without
//   client-side baseline tracking. Delta is OMITTED on the first
//   sample after process start (no baseline) and on the first
//   sample after a detected counter reset (current < last —
//   container restarted).
package metrics

import "time"

// Source distinguishes the two kinds of rows. The reader filters
// by this in O(1) before parsing the rest of the line.
type Source string

const (
	SourceSystem Source = "system"
	SourcePod    Source = "pod"
)

// SystemSample is the host-level row written once per tick. Fields
// mirror systemstats.Snapshot's flat scalars (disk[] becomes the
// `/` mount's bytes — multi-mount support is a future addition).
type SystemSample struct {
	Ts          time.Time `json:"ts"`
	Source      Source    `json:"source"`
	CPUPercent  float64   `json:"cpu_percent"`
	MemUsedBytes  uint64  `json:"mem_used_bytes"`
	MemTotalBytes uint64  `json:"mem_total_bytes"`
	DiskUsedBytes  uint64 `json:"disk_used_bytes,omitempty"`
	DiskTotalBytes uint64 `json:"disk_total_bytes,omitempty"`
}

// PodSample is one row per running pod per tick. Identity fields
// (Kind/Scope/Name/ReplicaID/Container) are denormalised inline so
// the reader doesn't need a separate manifest lookup at query time.
//
// *_delta_bytes are pointer types because they're absent on the
// first sample after start AND the first sample after a counter
// reset; encoding/json's `omitempty` treats nil as "skip the key"
// for pointers, which is the wire signal the reader uses to know
// "no delta to chart for this point."
type PodSample struct {
	Ts        time.Time `json:"ts"`
	Source    Source    `json:"source"`
	Container string    `json:"container"`
	Kind      string    `json:"kind"`
	Scope     string    `json:"scope,omitempty"`
	Name      string    `json:"name"`
	ReplicaID string    `json:"replica_id,omitempty"`

	CPUPercent       float64 `json:"cpu_percent"`
	MemUsageBytes    uint64  `json:"mem_usage_bytes"`
	MemLimitBytes    uint64  `json:"mem_limit_bytes,omitempty"`

	NetRxBytes uint64 `json:"net_rx_bytes,omitempty"`
	NetTxBytes uint64 `json:"net_tx_bytes,omitempty"`
	// Pointer so omitempty drops the field on first-sample / post-reset.
	NetRxDeltaBytes *uint64 `json:"net_rx_delta_bytes,omitempty"`
	NetTxDeltaBytes *uint64 `json:"net_tx_delta_bytes,omitempty"`

	BlockReadBytes  uint64 `json:"block_read_bytes,omitempty"`
	BlockWriteBytes uint64 `json:"block_write_bytes,omitempty"`
	// Same pointer-omitempty pattern as Net.
	BlockReadDeltaBytes  *uint64 `json:"block_read_delta_bytes,omitempty"`
	BlockWriteDeltaBytes *uint64 `json:"block_write_delta_bytes,omitempty"`
}

// DefaultInterval is the sampler's tick cadence when the operator
// hasn't overridden via --metrics-interval / VOODU_METRICS_INTERVAL.
// Chosen as a balance between chart fidelity (15s shows spikes
// shorter than a minute) and disk footprint (~95 MB per 7d × 10 pods).
const DefaultInterval = 15 * time.Second

// DefaultRetention is the cleanup window when the operator hasn't
// overridden via --metrics-retention / VOODU_METRICS_RETENTION.
// 7 days because:
//   - covers a typical week of post-mortem queries
//   - bounded disk: ~15 MB after yesterday's file is gzipped
//   - longer history is the job of an external TSDB scrape
const DefaultRetention = 7 * 24 * time.Hour

// MaxPodsPerSample caps the number of pods we'll write per tick.
// A misbehaving manifest loop creating thousands of pods could
// otherwise fill the disk. Beyond the cap, the sampler logs a
// warning and skips the rest — better to have partial visibility
// than to crash the controller's disk.
const MaxPodsPerSample = 100
