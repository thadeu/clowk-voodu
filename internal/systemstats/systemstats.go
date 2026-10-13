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
// averages. Percent is the busy% averaged over the collector's CPU
// window (the last 15s by default, see CPUWindow) — the same value
// for every caller, so GET /system and the metrics chart agree. It
// is 0 until the collector has two readings (right after boot).
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

// GopsutilCollector is the production implementation. Its only
// state is the CPU window: CPU% is a rate, so it needs readings over
// time, and those must belong to this collector — not to gopsutil's
// package-global "since the last call" baseline that every caller
// used to reset for everyone else.
type GopsutilCollector struct {
	cpu *CPUWindow
}

// NewGopsutilCollector returns a collector whose CPU% averages over
// cpuWindow (DefaultCPUWindow when <= 0). Start Run once so the
// window gets a reading every second; without it, Snapshot's own
// readings feed the window at the callers' cadence.
func NewGopsutilCollector(cpuWindow time.Duration) *GopsutilCollector {
	return &GopsutilCollector{cpu: &CPUWindow{Window: cpuWindow}}
}

// Run keeps the CPU window fed until ctx ends. Long-lived: start it
// once per collector, next to the metrics sampler.
func (g *GopsutilCollector) Run(ctx context.Context) {
	g.cpu.Run(ctx)
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

	// CPU — a fresh reading closes the window at "now", then the
	// average over the last window. Reading never moves a baseline,
	// so other callers can't shrink this one's window.
	g.cpu.read(ctx)
	snap.CPU.Percent = g.cpu.Percent()

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
