// metrics_adapter.go bridges the controller's `StatsCollector`
// (which lives in this package and consumes docker stats + manifests)
// to the `internal/metrics` package's `PodSource` seam.
//
// The bridge exists because:
//
//   - `internal/metrics` MUST NOT import `controller` (cycle: the
//     controller's server.go wires the sampler).
//   - The sampler needs per-pod runtime numbers in a stable typed
//     shape (`metrics.PodRuntime`) that doesn't pull controller
//     types into its API surface.
//
// This adapter is the only place that field-by-field copies from
// `PodStats` (`stats.go`) into `metrics.PodRuntime`. If new metric
// fields land on UsageStats, add them here AND in the wire shape
// in `internal/metrics/metrics.go`.

package controller

import (
	"context"

	"go.voodu.clowk.in/internal/metrics"
)

// statsCollectorAdapter wraps *StatsCollector so it satisfies
// metrics.PodSource. Orphans are EXCLUDED from the metric stream
// (filter.Orphans = false) — we only persist data for containers
// the controller is managing; leaked / pre-M0 containers don't
// belong on the chart.
type statsCollectorAdapter struct {
	c *StatsCollector
}

// NewMetricsPodSource is the constructor the server's Start() uses.
// Nil-tolerant: when c is nil (test setups), returns nil so the
// sampler skips pod sampling without erroring.
func NewMetricsPodSource(c *StatsCollector) metrics.PodSource {
	if c == nil {
		return nil
	}

	return &statsCollectorAdapter{c: c}
}

// Collect copies every running pod's runtime numbers into the
// flat `metrics.PodRuntime` shape. We call RefreshSnapshot rather
// than Collect directly: that does the docker stats roundtrip AND
// stores the orphans-included result in the collector's atomic
// snapshot pointer in one shot. The side effect — keeping the
// snapshot fresh so /stats, /pods?detail=true and the autoscaler
// can read from cache — is the whole point of this refactor.
//
// We then filter orphans out of the returned slice so the
// time-series store only carries data for containers the controller
// is managing; leaked / pre-M0 containers don't belong on the
// chart. (Orphans stay in the snapshot for /stats?orphans=true.)
func (a *statsCollectorAdapter) Collect(ctx context.Context) ([]metrics.PodRuntime, error) {
	rows, err := a.c.RefreshSnapshot(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]metrics.PodRuntime, 0, len(rows))

	for _, r := range rows {
		if r.Orphan {
			continue
		}

		out = append(out, metrics.PodRuntime{
			Container:       r.ContainerName,
			Kind:            r.Identity.Kind,
			Scope:           r.Identity.Scope,
			Name:            r.Identity.Name,
			ReplicaID:       r.Identity.ReplicaID,
			CPUPercent:      r.Usage.CPUPercent,
			MemUsageBytes:   r.Usage.MemoryUsageBytes,
			MemLimitBytes:   r.Usage.MemoryLimitBytes,
			NetRxBytes:      r.Usage.NetRxBytes,
			NetTxBytes:      r.Usage.NetTxBytes,
			BlockReadBytes:  r.Usage.BlockReadBytes,
			BlockWriteBytes: r.Usage.BlockWriteBytes,
		})
	}

	return out, nil
}
