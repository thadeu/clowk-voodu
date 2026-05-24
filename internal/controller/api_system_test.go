// Tests for GET /system — pins the HTTP boundary specifically:
// 503 when collector is missing, happy-path envelope shape, and
// that the wire field names line up with what the Rails WebUI
// (Voodu::Client#system) reads. The systemstats package itself
// owns the unit tests for the rate-delta math.

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.voodu.clowk.in/internal/systemstats"
)

// fakeSystemCollector returns a canned snapshot without touching
// the OS — keeps the test fast and reproducible across CI hosts.
type fakeSystemCollector struct {
	snap systemstats.Snapshot
	err  error
}

func (f *fakeSystemCollector) Snapshot(_ context.Context) (systemstats.Snapshot, error) {
	return f.snap, f.err
}

func TestSystem_503WhenCollectorMissing(t *testing.T) {
	api, _ := newTestAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/system")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

func TestSystem_HappyPathEnvelope(t *testing.T) {
	api, _ := newTestAPI(t)

	api.System = &fakeSystemCollector{
		snap: systemstats.Snapshot{
			Host: systemstats.HostInfo{
				Hostname:      "debian-prod-01",
				Kernel:        "debian 6.1.0",
				UptimeSeconds: 3548412,
			},
			CPU:  systemstats.CPUStats{Percent: 12.4, Cores: 8, Load1: 0.41, Load5: 0.86, Load15: 1.21},
			Mem:  systemstats.MemStats{UsedBytes: 8_123_456_789, TotalBytes: 16_777_216_000, AvailableBytes: 7_234_567_890},
			Disk: []systemstats.DiskFS{{Mount: "/", UsedBytes: 184_000_000_000, TotalBytes: 500_000_000_000}},
		},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/system")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	// Decode permissively into a map so we can assert the WebUI's
	// exact dotted paths exist — a struct rename in systemstats
	// would slip past a typed decode but break this assertion.
	var env struct {
		Status string         `json:"status"`
		Data   map[string]any `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	if env.Status != "ok" {
		t.Fatalf("status: got %q want ok", env.Status)
	}

	// The Rails Voodu::Client#system reads each of these exact
	// paths. Asserting them here means a wire-shape regression
	// fails Go tests before it ships to the WebUI.
	mustHave(t, env.Data, "host", "uptime_seconds")
	mustHave(t, env.Data, "cpu", "percent")
	mustHave(t, env.Data, "cpu", "cores")
	mustHave(t, env.Data, "mem", "used_bytes")
	mustHave(t, env.Data, "mem", "total_bytes")

	// W7 regression guard — host-level io/net moved to per-pod
	// UsageStats. Don't let them sneak back in.
	for _, key := range []string{"io", "net"} {
		if _, present := env.Data[key]; present {
			t.Errorf("key %q reappeared in /system response — host-level net/io was deliberately removed in W7", key)
		}
	}

	disk, _ := env.Data["disk"].([]any)
	if len(disk) == 0 {
		t.Fatalf("disk slice empty: %v", env.Data["disk"])
	}
}

// mustHave walks a decoded JSON object down `path` and fails the
// test if any segment is missing. Encoded errors print the parent
// map so a missing key is easy to spot.
func mustHave(t *testing.T, root map[string]any, path ...string) {
	t.Helper()

	cur := any(root)

	for i, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: segment %d not a map (got %T = %v)", path, i, cur, cur)
		}

		next, ok := m[key]
		if !ok {
			t.Fatalf("path %v: key %q missing under %v", path, key, m)
		}

		cur = next
	}
}
