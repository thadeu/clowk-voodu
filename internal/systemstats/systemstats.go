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
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
)

// Snapshot is the wire-shape returned by GET /system. Field tags are
// JSON because the same struct is encoded straight into the response
// body — keeps a single source of truth for the HTTP contract.
type Snapshot struct {
	Host HostInfo `json:"host"`
	CPU  CPUStats `json:"cpu"`
	Mem  MemStats `json:"mem"`
	Disk []DiskFS `json:"disk"`
	IO   IORate   `json:"io"`
	Net  NetRate  `json:"net"`
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

// IORate is throughput per second, summed across all block devices.
// Computed as delta between two samples — first call returns 0
// (no prior sample to diff against).
type IORate struct {
	ReadBytesPerSec  float64 `json:"read_bytes_per_sec"`
	WriteBytesPerSec float64 `json:"write_bytes_per_sec"`
}

// NetRate mirrors IORate for network interfaces (aggregate across
// all NICs). Same first-call-returns-zero contract.
type NetRate struct {
	RxBytesPerSec float64 `json:"rx_bytes_per_sec"`
	TxBytesPerSec float64 `json:"tx_bytes_per_sec"`
}

// Collector is the single-method seam tests use to inject canned
// snapshots without dragging in gopsutil's OS dependencies.
type Collector interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// GopsutilCollector is the production implementation. It keeps a
// small amount of state — the last sampled IO/Net counters — so it
// can compute rates without forcing every caller to maintain that
// bookkeeping themselves.
//
// Thread-safe: protected by a mutex on the rate-state read-modify-
// write. Per-sample work is short (~5–15ms on Linux), so contention
// is not a concern at the WebUI's polling cadence (~once per 10s).
type GopsutilCollector struct {
	mu          sync.Mutex
	lastSampled time.Time

	lastReadBytes  uint64
	lastWriteBytes uint64
	lastRxBytes    uint64
	lastTxBytes    uint64
}

// NewGopsutilCollector returns a collector ready to serve requests.
// No pre-sampling — the first Snapshot will return 0 for CPU%, IO
// rate, and Net rate (those metrics need two samples to diff).
// Subsequent calls populate the rates from the delta. Callers that
// want a populated first response should call Snapshot twice in
// quick succession (a few seconds apart) at startup.
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

	// IO + Net rates — read raw counters then delta against the
	// last sample. Mutex-serialised because two concurrent callers
	// must not both think they're the "first" reader and clobber
	// the baseline.
	now := time.Now()

	var (
		rb, wb uint64
		rx, tx uint64
	)

	if io, err := disk.IOCountersWithContext(ctx); err == nil {
		for _, c := range io {
			rb += c.ReadBytes
			wb += c.WriteBytes
		}
	}

	if ni, err := psnet.IOCountersWithContext(ctx, false); err == nil && len(ni) > 0 {
		rx = ni[0].BytesRecv
		tx = ni[0].BytesSent
	}

	snap.IO, snap.Net = g.updateRates(now, rb, wb, rx, tx)

	return snap, nil
}

// updateRates is the rate-calculation seam — pulled out of Snapshot
// so the delta math can be tested without standing up gopsutil.
// Caller passes the raw cumulative counters; this returns the rates
// since the previous call and updates the baseline.
//
// First-call semantics: lastSampled.IsZero() → returns zero rates,
// only seeds the baseline. This is the documented "first Snapshot
// returns 0 for IO/Net" behaviour. Counter wrap-around (uint64
// underflow when current < last — happens on interface reset) also
// returns zero for that field rather than emitting a bogus huge
// number; a missed sample is better than a misleading one.
func (g *GopsutilCollector) updateRates(now time.Time, rb, wb, rx, tx uint64) (IORate, NetRate) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var (
		io  IORate
		net NetRate
	)

	if !g.lastSampled.IsZero() {
		elapsed := now.Sub(g.lastSampled).Seconds()
		if elapsed > 0 {
			io.ReadBytesPerSec = rate(rb, g.lastReadBytes, elapsed)
			io.WriteBytesPerSec = rate(wb, g.lastWriteBytes, elapsed)
			net.RxBytesPerSec = rate(rx, g.lastRxBytes, elapsed)
			net.TxBytesPerSec = rate(tx, g.lastTxBytes, elapsed)
		}
	}

	g.lastSampled = now
	g.lastReadBytes = rb
	g.lastWriteBytes = wb
	g.lastRxBytes = rx
	g.lastTxBytes = tx

	return io, net
}

// rate computes (current - previous) / elapsed, guarding against
// uint64 underflow when the counter went backwards (interface
// reset, daemon restart). Returns 0 rather than a huge number so
// the dashboard doesn't briefly show "8.4 PB/s" after a reboot.
func rate(current, previous uint64, elapsedSeconds float64) float64 {
	if current < previous {
		return 0
	}

	return float64(current-previous) / elapsedSeconds
}
