package systemstats

import (
	"context"
	"encoding/json"
	"testing"
)

// TestSnapshotJSONShape pins the wire contract — field names + nesting
// — so a careless rename in a struct tag would break the test before
// it broke the WebUI. The Rails client reads these exact paths.
//
// W7 narrowed the shape: `io` and `net` left the system endpoint
// because at the host level they're either misleading (NIC summing
// double-counts container traffic) or hard to render without per-NIC
// breakdown. Per-pod NET/BLOCK I/O moved to UsageStats; this test
// asserts neither key reappears in /system by accident.
func TestSnapshotJSONShape(t *testing.T) {
	snap := Snapshot{
		Host: HostInfo{Hostname: "h", Kernel: "k", UptimeSeconds: 42},
		CPU:  CPUStats{Percent: 12.5, Cores: 8, Load1: 0.5},
		Mem:  MemStats{UsedBytes: 1, TotalBytes: 2, AvailableBytes: 3},
		Disk: []DiskFS{{Mount: "/", UsedBytes: 10, TotalBytes: 100}},
	}

	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Top-level keys the WebUI consumes.
	for _, key := range []string{"host", "cpu", "mem", "disk"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing top-level key %q in %s", key, b)
		}
	}

	// W7 regression guard: io and net MUST stay out of /system.
	// Reintroducing them at the host level should be a deliberate
	// design decision (with a per-NIC breakdown), not a slip from
	// somebody adding a field "for completeness."
	for _, key := range []string{"io", "net"} {
		if _, present := got[key]; present {
			t.Errorf("key %q reappeared in /system — host-level network/io was removed in W7 in favour of per-pod stats", key)
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
