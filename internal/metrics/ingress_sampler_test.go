package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End-to-end: write fake Caddy lines to a tempfile, run a sampler
// against it, verify NDJSON appears in the writer's output directory
// with the expected aggregated shape.
func TestIngressSampler_AggregatesAndEmits(t *testing.T) {
	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "access.log")

	if err := os.WriteFile(logFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	metricsDir := t.TempDir()
	writer, err := NewWriter(metricsDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	// Static resolver — pretend `api.example.com` belongs to deployment
	// (scope=clowk-vd, name=api). Other hosts return ok=false → skipped.
	resolver := &StaticHostResolver{Bindings: map[string]IngressBinding{
		"api.example.com": {Host: "api.example.com", Scope: "clowk-vd", Name: "api"},
	}}

	sampler := &IngressSampler{
		LogPath: logFile,
		Tick:    50 * time.Millisecond,
		Now:     func() time.Time { return time.Date(2026, 5, 25, 13, 0, 0, 0, time.UTC) },
		Hosts:   resolver,
		Writer:  writer,
	}

	// Pre-seed sampler so first-open seeks to EOF (we add lines AFTER
	// it's been initialized, simulating fresh requests). We call
	// evaluate() directly instead of Run(ctx) so no ctx is needed.
	sampler.firstOpen = true
	sampler.agg = NewIngressAggregator()

	// Open file once to set up tail state at EOF.
	if err := sampler.ingestNewLines(); err != nil {
		t.Fatal(err)
	}

	// Now write Caddy-style access log lines.
	lines := []string{
		// 3 requests to api, two 2xx + one 5xx with diverse latencies
		caddyLine("api.example.com", 0.010, 200, 100),
		caddyLine("api.example.com", 0.020, 200, 200),
		caddyLine("api.example.com", 0.500, 500, 50),
		// 1 request to an unmanaged host — should be skipped
		caddyLine("unmanaged.example.com", 0.005, 200, 1),
		// 1 line with malformed JSON — should be skipped
		`{not valid json`,
		// 1 line missing host — should be skipped
		`{"request":{},"duration":0.001,"status":200,"size":10}`,
	}

	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range lines {
		fmt.Fprintln(f, ln)
	}
	f.Close()

	// Run one evaluation explicitly (instead of waiting for the ticker).
	sampler.evaluate()

	// Read the NDJSON file the writer just produced.
	dayFile := filepath.Join(metricsDir, "metrics-2026-05-25.ndjson")
	raw, err := os.ReadFile(dayFile)
	if err != nil {
		t.Fatal(err)
	}

	rows := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 emitted row, got %d:\n%s", len(rows), raw)
	}

	var sample IngressSample
	if err := json.Unmarshal([]byte(rows[0]), &sample); err != nil {
		t.Fatalf("unmarshal: %v\nrow: %s", err, rows[0])
	}

	if sample.Source != SourceIngress {
		t.Errorf("source: want ingress, got %s", sample.Source)
	}
	if sample.Host != "api.example.com" {
		t.Errorf("host: want api.example.com, got %s", sample.Host)
	}
	if sample.Scope != "clowk-vd" || sample.Name != "api" {
		t.Errorf("identity: want clowk-vd/api, got %s/%s", sample.Scope, sample.Name)
	}
	if sample.ReqCount != 3 {
		t.Errorf("req_count: want 3, got %d", sample.ReqCount)
	}
	if sample.Req2xx != 2 {
		t.Errorf("req_2xx: want 2, got %d", sample.Req2xx)
	}
	if sample.Req5xx != 1 {
		t.Errorf("req_5xx: want 1, got %d", sample.Req5xx)
	}
	if sample.BytesOut != 350 {
		t.Errorf("bytes_out: want 350, got %d", sample.BytesOut)
	}

	// Latency percentiles populated (3 samples → all five present).
	// With nearest-rank on [10, 20, 500]: p99 index = int(2 * 0.99) = 1
	// → sorted[1] = 20ms. Max correctly = 500ms.
	if sample.LatencyP99Ms == nil || *sample.LatencyP99Ms != 20.0 {
		t.Errorf("p99: want 20ms (nearest-rank on 3 samples), got %v", deref(sample.LatencyP99Ms))
	}
	if sample.LatencyMaxMs == nil || *sample.LatencyMaxMs != 500.0 {
		t.Errorf("max: want 500ms, got %v", deref(sample.LatencyMaxMs))
	}
}

// deref formats a *float64 for assertion messages — prints "<nil>"
// instead of the pointer address when the latency wasn't populated.
func deref(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v", *p)
}

// Hosts not declared as ingress get skipped — sampler never emits
// a row for them. Important so traffic to a stranger host doesn't
// fabricate a "deployment" in the warehouse.
func TestIngressSampler_SkipsUnmanagedHosts(t *testing.T) {
	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "access.log")
	os.WriteFile(logFile, nil, 0o644)

	metricsDir := t.TempDir()
	writer, _ := NewWriter(metricsDir, nil)

	sampler := &IngressSampler{
		LogPath: logFile,
		Now:     func() time.Time { return time.Date(2026, 5, 25, 13, 0, 0, 0, time.UTC) },
		Hosts:   &StaticHostResolver{}, // empty bindings → every host unmanaged
		Writer:  writer,
	}
	sampler.firstOpen = true
	sampler.agg = NewIngressAggregator()
	sampler.ingestNewLines()

	// Append a request to a host nobody knows about.
	f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintln(f, caddyLine("unknown.example.com", 0.01, 200, 100))
	f.Close()

	sampler.evaluate()

	dayFile := filepath.Join(metricsDir, "metrics-2026-05-25.ndjson")
	raw, err := os.ReadFile(dayFile)

	// Either the file doesn't exist (writer never opened it) or it's empty —
	// either is acceptable for "skipped everything".
	if err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		t.Errorf("expected zero emitted rows, got:\n%s", raw)
	}
}

// First-open MUST seek to EOF so historical lines aren't backfilled
// as "now" samples. Without this, a controller restart against a 50MB
// access log would emit a huge sample claiming "now" traffic was
// everything the host ever served.
func TestIngressSampler_FirstOpenSeeksToEOF(t *testing.T) {
	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "access.log")

	// Pre-populate with old lines — sampler should NOT see these.
	os.WriteFile(logFile, []byte(
		caddyLine("api.example.com", 0.01, 200, 100)+"\n"+
			caddyLine("api.example.com", 0.02, 200, 200)+"\n",
	), 0o644)

	metricsDir := t.TempDir()
	writer, _ := NewWriter(metricsDir, nil)

	sampler := &IngressSampler{
		LogPath: logFile,
		Now:     func() time.Time { return time.Date(2026, 5, 25, 13, 0, 0, 0, time.UTC) },
		Hosts: &StaticHostResolver{Bindings: map[string]IngressBinding{
			"api.example.com": {Host: "api.example.com", Scope: "s", Name: "n"},
		}},
		Writer: writer,
	}
	sampler.firstOpen = true
	sampler.agg = NewIngressAggregator()

	sampler.evaluate()

	// Heartbeat-zero emits 1 row per known binding even with no
	// fresh traffic (post-2026-05-26 behaviour — keeps HTTP charts
	// in lockstep with resource charts). The historical lines'
	// counts MUST NOT show up: if EOF-seek failed and we ingested
	// 100 + 200 bytes from the pre-existing log, the row would
	// carry req_count=2 and bytes_out=300. A heartbeat-only run
	// carries req_count=0 and no bytes_out (omitempty).
	dayFile := filepath.Join(metricsDir, "metrics-2026-05-25.ndjson")
	raw, err := os.ReadFile(dayFile)
	if err != nil {
		t.Fatalf("expected heartbeat row, got read err: %v", err)
	}
	if strings.Contains(string(raw), `"req_count":2`) || strings.Contains(string(raw), `"bytes_out":300`) {
		t.Errorf("EOF-seek failed; row carries historical traffic:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"req_count":0`) {
		t.Errorf("expected heartbeat row with req_count:0, got:\n%s", raw)
	}
}

// caddyLine builds a minimal Caddy v2 JSON access log line for tests.
// Mirrors the shape we verified on live infra (request.host, duration,
// status, size) plus the noise fields Caddy actually emits (logger,
// resp_headers) to be honest about what the parser tolerates.
func caddyLine(host string, durationSec float64, status, size int) string {
	payload := map[string]any{
		"level":  "info",
		"ts":     1779724557.0,
		"logger": "http.log.access",
		"msg":    "handled request",
		"request": map[string]any{
			"remote_ip": "1.2.3.4",
			"method":    "GET",
			"host":      host,
			"uri":       "/",
		},
		"duration": durationSec,
		"status":   status,
		"size":     size,
		"resp_headers": map[string]any{
			"Server": []string{"Caddy"},
		},
	}

	b, _ := json.Marshal(payload)

	return string(b)
}
