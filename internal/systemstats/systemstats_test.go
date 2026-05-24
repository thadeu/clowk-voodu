package systemstats

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

// TestUpdateRatesFirstCallReturnsZero pins the documented first-call
// contract: with no baseline, IO/Net rates MUST be zero rather than
// a garbage value derived from "subtracting from zero". This is what
// lets callers safely call Snapshot once at startup without seeing
// fake-looking spikes.
func TestUpdateRatesFirstCallReturnsZero(t *testing.T) {
	c := NewGopsutilCollector()

	io, net := c.updateRates(time.Now(), 1_000_000, 500_000, 2_000_000, 1_500_000)

	if io.ReadBytesPerSec != 0 || io.WriteBytesPerSec != 0 {
		t.Errorf("first IO call should be 0, got %+v", io)
	}

	if net.RxBytesPerSec != 0 || net.TxBytesPerSec != 0 {
		t.Errorf("first Net call should be 0, got %+v", net)
	}
}

// TestUpdateRatesSecondCallComputesDelta covers the normal case:
// counters advanced, time advanced, rate = delta / elapsed.
func TestUpdateRatesSecondCallComputesDelta(t *testing.T) {
	c := NewGopsutilCollector()

	t0 := time.Now()
	c.updateRates(t0, 1_000_000, 500_000, 2_000_000, 1_500_000)

	// 2 seconds later, each counter advanced.
	t1 := t0.Add(2 * time.Second)
	io, net := c.updateRates(t1, 1_002_000, 504_000, 2_006_000, 1_502_000)

	// 2000 read bytes over 2 seconds = 1000/sec
	if !approxEq(io.ReadBytesPerSec, 1000) {
		t.Errorf("read: got %v want 1000", io.ReadBytesPerSec)
	}

	if !approxEq(io.WriteBytesPerSec, 2000) {
		t.Errorf("write: got %v want 2000", io.WriteBytesPerSec)
	}

	if !approxEq(net.RxBytesPerSec, 3000) {
		t.Errorf("rx: got %v want 3000", net.RxBytesPerSec)
	}

	if !approxEq(net.TxBytesPerSec, 1000) {
		t.Errorf("tx: got %v want 1000", net.TxBytesPerSec)
	}
}

// TestUpdateRatesCounterWrapReturnsZero pins the underflow guard:
// when current < previous (NIC reset, daemon restart), the rate
// MUST be 0 rather than a huge garbage number from uint64
// underflow. The alternative would briefly show "8.4 PB/s" on the
// dashboard after every restart — worse UX than a missed sample.
func TestUpdateRatesCounterWrapReturnsZero(t *testing.T) {
	c := NewGopsutilCollector()

	t0 := time.Now()
	c.updateRates(t0, 1_000_000, 500_000, 2_000_000, 1_500_000)

	// Counter went backwards (simulated reset).
	t1 := t0.Add(2 * time.Second)
	io, net := c.updateRates(t1, 0, 0, 0, 0)

	if io.ReadBytesPerSec != 0 || io.WriteBytesPerSec != 0 {
		t.Errorf("wrap should yield 0 io, got %+v", io)
	}

	if net.RxBytesPerSec != 0 || net.TxBytesPerSec != 0 {
		t.Errorf("wrap should yield 0 net, got %+v", net)
	}
}

// TestUpdateRatesZeroElapsedReturnsZero — two samples at the same
// instant. Avoids div-by-zero, returns 0.
func TestUpdateRatesZeroElapsedReturnsZero(t *testing.T) {
	c := NewGopsutilCollector()

	t0 := time.Now()
	c.updateRates(t0, 1_000_000, 500_000, 2_000_000, 1_500_000)

	io, net := c.updateRates(t0, 1_002_000, 504_000, 2_006_000, 1_502_000)

	if io.ReadBytesPerSec != 0 || net.RxBytesPerSec != 0 {
		t.Errorf("zero elapsed must yield 0 rates, got io=%+v net=%+v", io, net)
	}
}

// TestSnapshotJSONShape pins the wire contract — field names + nesting
// — so a careless rename in a struct tag would break the test before
// it broke the WebUI. The Rails client reads these exact paths.
func TestSnapshotJSONShape(t *testing.T) {
	snap := Snapshot{
		Host: HostInfo{Hostname: "h", Kernel: "k", UptimeSeconds: 42},
		CPU:  CPUStats{Percent: 12.5, Cores: 8, Load1: 0.5},
		Mem:  MemStats{UsedBytes: 1, TotalBytes: 2, AvailableBytes: 3},
		Disk: []DiskFS{{Mount: "/", UsedBytes: 10, TotalBytes: 100}},
		IO:   IORate{ReadBytesPerSec: 1.5, WriteBytesPerSec: 2.5},
		Net:  NetRate{RxBytesPerSec: 3.5, TxBytesPerSec: 4.5},
	}

	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Top-level keys.
	for _, key := range []string{"host", "cpu", "mem", "disk", "io", "net"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing top-level key %q in %s", key, b)
		}
	}

	// Spot-check the nested keys the WebUI reads.
	host := got["host"].(map[string]any)
	if _, ok := host["uptime_seconds"]; !ok {
		t.Errorf("host.uptime_seconds missing: %v", host)
	}

	cpu := got["cpu"].(map[string]any)
	if _, ok := cpu["percent"]; !ok {
		t.Errorf("cpu.percent missing: %v", cpu)
	}

	mem := got["mem"].(map[string]any)
	if _, ok := mem["used_bytes"]; !ok {
		t.Errorf("mem.used_bytes missing: %v", mem)
	}

	io := got["io"].(map[string]any)
	if _, ok := io["read_bytes_per_sec"]; !ok {
		t.Errorf("io.read_bytes_per_sec missing: %v", io)
	}
}

// TestSnapshotRealSmokeTest is a sanity check that gopsutil works on
// this host — not pinning specific numbers (those vary), just that
// Snapshot returns without error and surfaces plausible values
// (cores > 0, total memory > 0). If this fails on a dev machine we
// know the dependency wiring is broken.
func TestSnapshotRealSmokeTest(t *testing.T) {
	c := NewGopsutilCollector()

	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if snap.CPU.Cores <= 0 {
		t.Errorf("expected cores > 0, got %d", snap.CPU.Cores)
	}

	if snap.Mem.TotalBytes == 0 {
		t.Errorf("expected mem total > 0")
	}

	if snap.Host.UptimeSeconds == 0 {
		t.Errorf("expected uptime > 0")
	}
}

func approxEq(a, b float64) bool {
	return math.Abs(a-b) < 0.01
}
