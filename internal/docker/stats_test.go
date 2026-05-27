// Tests for the docker stats SDK conversion layer. We don't talk to a
// real docker daemon in unit tests (that's an integration concern) —
// each case feeds a typed container.StatsResponse fixture into
// convertStats and asserts the resulting ContainerStats fields.
//
// The earlier CLI-text parsing tests (parseStatsLine, parseDualBytes,
// etc.) were retired alongside the CLI shim: the SDK path operates on
// typed structs end-to-end, so there's no string parsing left to pin.

package docker

import (
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

// TestConvertStats_HappyPath pins the canonical Linux-cgroup-v2 sample
// shape — every field the rest of the codebase reads should land in
// the expected ContainerStats slot.
func TestConvertStats_HappyPath(t *testing.T) {
	raw := container.StatsResponse{
		ID:   "abc123def456ghi789", // long ID — should be truncated to 12
		Name: "/fsw-rabbitmq.0",    // leading slash — should be stripped
		Read: time.Now(),

		// CPU%: cpuDelta=1e8, systemDelta=1e9, onlineCPUs=4
		//   = (1e8 / 1e9) × 4 × 100 = 40.0%
		CPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 2_000_000_000},
			SystemUsage: 10_000_000_000,
			OnlineCPUs:  4,
		},
		PreCPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 1_900_000_000},
			SystemUsage: 9_000_000_000,
		},

		MemoryStats: container.MemoryStats{
			Usage: 200 * 1024 * 1024,  // 200 MiB
			Limit: 1024 * 1024 * 1024, // 1 GiB
			Stats: map[string]uint64{
				"cache": 50 * 1024 * 1024, // 50 MiB page cache to subtract
			},
		},

		Networks: map[string]container.NetworkStats{
			"eth0": {RxBytes: 1_000_000, TxBytes: 500_000},
			"eth1": {RxBytes: 200_000, TxBytes: 100_000},
		},

		BlkioStats: container.BlkioStats{
			IoServiceBytesRecursive: []container.BlkioStatEntry{
				{Op: "Read", Value: 8_000_000},
				{Op: "Write", Value: 4_000_000},
				{Op: "Read", Value: 1_000_000}, // second read entry — summed
			},
		},

		PidsStats: container.PidsStats{Current: 19},
	}

	got := convertStats(raw, "fallback-name")

	if got.Name != "fsw-rabbitmq.0" {
		t.Errorf("Name = %q, want fsw-rabbitmq.0 (leading slash stripped)", got.Name)
	}

	if got.ID != "abc123def456" {
		t.Errorf("ID = %q, want abc123def456 (truncated to 12 chars)", got.ID)
	}

	if got.CPUPercent != 40.0 {
		t.Errorf("CPUPercent = %v, want 40.0", got.CPUPercent)
	}

	// MemUsage: 200 MiB raw - 50 MiB cache = 150 MiB
	wantMem := uint64(150 * 1024 * 1024)
	if got.MemUsageBytes != wantMem {
		t.Errorf("MemUsageBytes = %d, want %d (raw minus page cache)", got.MemUsageBytes, wantMem)
	}

	if got.MemLimitBytes != 1024*1024*1024 {
		t.Errorf("MemLimitBytes = %d, want 1GiB", got.MemLimitBytes)
	}

	// 150 MiB / 1 GiB = ~14.6%
	if got.MemPercent < 14.0 || got.MemPercent > 15.0 {
		t.Errorf("MemPercent = %v, want ~14.65", got.MemPercent)
	}

	if got.NetRxBytes != 1_200_000 {
		t.Errorf("NetRxBytes = %d, want 1200000 (summed across ifaces)", got.NetRxBytes)
	}

	if got.NetTxBytes != 600_000 {
		t.Errorf("NetTxBytes = %d, want 600000 (summed)", got.NetTxBytes)
	}

	if got.BlockReadBytes != 9_000_000 {
		t.Errorf("BlockReadBytes = %d, want 9000000 (summed Reads)", got.BlockReadBytes)
	}

	if got.BlockWriteBytes != 4_000_000 {
		t.Errorf("BlockWriteBytes = %d, want 4000000", got.BlockWriteBytes)
	}

	if got.PIDs != 19 {
		t.Errorf("PIDs = %d, want 19", got.PIDs)
	}
}

// TestConvertStats_NameFallback covers the case where the daemon's
// response omits the Name field — sampleOne always passes the caller-
// supplied name as fallback so the result never ends up unnamed.
func TestConvertStats_NameFallback(t *testing.T) {
	raw := container.StatsResponse{} // empty — no Name, no nothing
	got := convertStats(raw, "caller-name")

	if got.Name != "caller-name" {
		t.Errorf("Name = %q, want caller-name (fallback when Name empty)", got.Name)
	}
}

// TestConvertStats_ZeroSystemDelta — when the pre-sample is missing
// (IncludePreviousSample=false), CPU% should be 0 instead of NaN or
// panic on division-by-zero.
func TestConvertStats_ZeroSystemDelta(t *testing.T) {
	raw := container.StatsResponse{
		CPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 1_000_000_000},
			SystemUsage: 0, // no previous sample collected
			OnlineCPUs:  4,
		},
		// PreCPUStats zero
	}
	got := convertStats(raw, "test")

	if got.CPUPercent != 0 {
		t.Errorf("CPUPercent = %v, want 0 when systemDelta is 0", got.CPUPercent)
	}
}

// TestConvertStats_Cgroupv2PageCache — cgroup v2 uses "file" instead
// of "cache" in MemoryStats.Stats. convertStats should subtract either.
func TestConvertStats_Cgroupv2PageCache(t *testing.T) {
	raw := container.StatsResponse{
		MemoryStats: container.MemoryStats{
			Usage: 300 * 1024 * 1024, // 300 MiB
			Limit: 1024 * 1024 * 1024,
			Stats: map[string]uint64{
				"file": 100 * 1024 * 1024, // cgroup v2 page cache key
			},
		},
	}
	got := convertStats(raw, "test")

	want := uint64(200 * 1024 * 1024)
	if got.MemUsageBytes != want {
		t.Errorf("MemUsageBytes = %d, want %d (cgroup v2 'file' as cache)", got.MemUsageBytes, want)
	}
}

// TestSubSat — saturating subtraction never underflows. Page cache
// can in theory exceed Usage in daemon races; we want 0, not 18EiB.
func TestSubSat(t *testing.T) {
	if got := subSat(100, 50); got != 50 {
		t.Errorf("subSat(100, 50) = %d, want 50", got)
	}

	if got := subSat(50, 100); got != 0 {
		t.Errorf("subSat(50, 100) = %d, want 0 (saturating, no underflow)", got)
	}

	if got := subSat(0, 100); got != 0 {
		t.Errorf("subSat(0, 100) = %d, want 0", got)
	}
}
