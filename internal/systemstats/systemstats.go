// Package systemstats is the controller's view of the HOST it runs on
// — CPU%, memory, disk usage, disk I/O rate, network rate, uptime,
// kernel — sourced from gopsutil so the WebUI (and a future
// `vd system` CLI command) can render real numbers instead of
// fabricated ones.
//
// Why a dedicated package, not glued into the existing /stats path?
//
//   - /stats answers "what is each pod consuming, vs its declared
//     limits?" — its identity is per-pod. Bolting host metrics onto
//     that endpoint via a `?type=` switch muddies the contract and
//     mixes caching characteristics (pod stats are O(N); host stats
//     are fixed cost).
//   - A separate package lets us pick a single dependency
//     (gopsutil/v4) and keep its blast radius small. Other packages
//     consume the typed Snapshot, never gopsutil directly.
//   - Future endpoints (`/metrics` for prometheus, `/audit`, etc.)
//     follow the same "one noun, one package" pattern.
//
// The Collector interface is the seam tests use. Production wires
// GopsutilCollector; tests inject a fake that returns canned
// snapshots without touching the OS.
package systemstats

import (
	"context"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

// Snapshot is the wire-shape returned by GET /system. Field tags are
// JSON because the same struct is encoded straight into the response
// body — keeps a single source of truth for the HTTP contract.
//
// Scope intentionally narrowed in W7: disk I/O rate and network
// throughput LEFT this struct because at the host level they're
// either misleading (sum over docker0/veth/eth0 double-counts the
// same byte flowing in/out) or hard to interpret without per-NIC
// breakdown. Per-pod NET I/O / BLOCK I/O moved into UsageStats
// (see internal/controller/stats.go) where docker already provides
// authoritative counts per-container.
//
// /system stays focused on "is this VM healthy at the OS level":
//   - boot identity + uptime
//   - CPU% + cores + load
//   - memory used/total
//   - disk SPACE used/total (per mount)
type Snapshot struct {
	Host HostInfo `json:"host"`
	CPU  CPUStats `json:"cpu"`
	Mem  MemStats `json:"mem"`
	Disk []DiskFS `json:"disk"`
}

// HostInfo is the static identity of the box — fields that don't
// change at runtime (modulo a reboot). Uptime is "seconds since
// boot" — cheaper to compute and locale-free vs a humanized string.
// Renderers (CLI, WebUI) format it for display.
type HostInfo struct {
	Hostname      string    `json:"hostname"`
	Kernel        string    `json:"kernel"`
	UptimeSeconds uint64    `json:"uptime_seconds"`
	BootTime      time.Time `json:"boot_time"`
}

// CPUStats carries aggregate CPU% across cores plus the unix load
// averages. Percent is a moving window — gopsutil's cpu.Percent
// returns the delta since the last call when interval=0, so the
// first Snapshot returns 0 for Percent (no baseline yet). Callers
// can either ignore the first sample or poll twice on startup.
type CPUStats struct {
	Percent float64 `json:"percent"`
	Cores   int     `json:"cores"`
	Load1   float64 `json:"load_1"`
	Load5   float64 `json:"load_5"`
	Load15  float64 `json:"load_15"`
}

// MemStats is the host's RAM picture. UsedBytes excludes buffers/
// cache (gopsutil semantics) — the value an operator expects to see
// in `htop` rather than the raw `free -m` "used" column.
type MemStats struct {
	UsedBytes      uint64 `json:"used_bytes"`
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// DiskFS describes one mounted filesystem. We surface `/` by default
// (the path that matters for "is the box about to run out of disk?")
// and skip pseudo filesystems (proc, tmpfs, devtmpfs). Returning a
// slice keeps the door open for surfacing additional mounts later
// without a wire-shape break.
type DiskFS struct {
	Mount      string `json:"mount"`
	UsedBytes  uint64 `json:"used_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

// Collector is the single-method seam tests use to inject canned
// snapshots without dragging in gopsutil's OS dependencies.
type Collector interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// GopsutilCollector is the production implementation. Stateless
// after W7 (host disk-I/O / network-rate sampling moved out — those
// metrics now live per-pod on UsageStats). Kept as a struct (not a
// free function) for interface satisfaction + future extensions
// that might need state again.
type GopsutilCollector struct{}

// NewGopsutilCollector returns a collector ready to serve requests.
// The first Snapshot returns 0 for CPU% (gopsutil's cpu.Percent
// needs two samples to compute the delta); subsequent calls give
// the real percentage. All other fields populate from the first
// call.
func NewGopsutilCollector() *GopsutilCollector {
	return &GopsutilCollector{}
}

// Snapshot reads every sub-metric and returns them in one bundle.
// Errors on optional metrics (e.g. disk I/O when /proc/diskstats is
// unreadable) degrade to zero values rather than failing the whole
// call — the WebUI can render "—" for missing fields, but losing
// the entire snapshot because the host doesn't expose disk counters
// would be a worse UX.
//
// The only failure mode that returns an error is when EVERY backend
// (CPU, mem, host info) fails — at that point we're not really
// running on a known OS and the caller should surface that.
func (g *GopsutilCollector) Snapshot(ctx context.Context) (Snapshot, error) {
	var snap Snapshot

	// Host info — cheap, never blocks. host.InfoWithContext respects
	// the ctx so callers can bound the call.
	if info, err := host.InfoWithContext(ctx); err == nil {
		snap.Host = HostInfo{
			Hostname:      info.Hostname,
			Kernel:        info.Platform + " " + info.KernelVersion,
			UptimeSeconds: info.Uptime,
			BootTime:      time.Unix(int64(info.BootTime), 0).UTC(),
		}
	}

	// CPU — gopsutil's Percent(0, false) returns the delta since the
	// last call (per cpu.go internals). First call therefore returns
	// 0 — documented in NewGopsutilCollector. Subsequent calls give
	// the real "% busy across all cores" number.
	if pcts, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(pcts) > 0 {
		snap.CPU.Percent = pcts[0]
	}

	if n, err := cpu.CountsWithContext(ctx, true); err == nil {
		snap.CPU.Cores = n
	}

	if avg, err := load.AvgWithContext(ctx); err == nil {
		snap.CPU.Load1 = avg.Load1
		snap.CPU.Load5 = avg.Load5
		snap.CPU.Load15 = avg.Load15
	}

	// Memory — VirtualMemory is the all-in-one. .Used is gopsutil's
	// "used minus buffers/cache" — the htop semantics, not free's.
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		snap.Mem = MemStats{
			UsedBytes:      vm.Used,
			TotalBytes:     vm.Total,
			AvailableBytes: vm.Available,
		}
	}

	// Disk filesystems — `/` is the universally-meaningful one (the
	// host root). Operators with separate volumes can ask for more
	// mounts later via query param. Skipping the discovery of every
	// mount keeps the response tight and predictable.
	if usage, err := disk.UsageWithContext(ctx, "/"); err == nil {
		snap.Disk = []DiskFS{{
			Mount:      "/",
			UsedBytes:  usage.Used,
			TotalBytes: usage.Total,
		}}
	}

	return snap, nil
}
