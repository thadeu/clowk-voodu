// Tests for internal/metrics. Each test covers one of the
// architectural risks called out in the M2 plan:
//
//   - bucket aggregation correctness across ranges/intervals
//   - counter reset detection drops deltas instead of clamping
//   - partial-line corruption tolerance
//   - day-boundary file selection by sample.Ts
//   - retention cleanup leaves the right files + gzips yesterday
//   - streaming reader memory stays bounded
//   - first-boot omits deltas (no baseline)
//   - identity wire shape stable across the writer/reader round trip
package metrics

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/systemstats"
)

// helper — build a writer rooted in t.TempDir() so each test is
// fully isolated and parallel-safe.
func newTestWriter(t *testing.T) (*Writer, string) {
	t.Helper()

	dir := t.TempDir()

	w, err := NewWriter(dir, nil)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}

	t.Cleanup(func() { _ = w.Close() })

	return w, dir
}

// TestWriter_RoundTripSystemSample pins the wire shape end-to-end:
// write a system row, read it back via Query, get the same value.
// Catches any drift in JSON field names or the source filter.
func TestWriter_RoundTripSystemSample(t *testing.T) {
	w, dir := newTestWriter(t)

	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	if err := w.WriteSystem(SystemSample{
		Ts:            now,
		CPUPercent:    12.4,
		MemUsedBytes:  8_000_000_000,
		MemTotalBytes: 16_000_000_000,
	}); err != nil {
		t.Fatal(err)
	}

	_ = w.Close()

	res, err := Query(QueryOpts{
		Dir:      dir,
		Source:   SourceSystem,
		Metric:   "cpu_percent",
		Start:    now.Add(-time.Minute),
		End:      now.Add(time.Minute),
		Interval: 30 * time.Second,
		Now:      func() time.Time { return now.Add(time.Minute) },
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	if len(res.Series) != 1 {
		t.Fatalf("series len=%d want 1 (got %+v)", len(res.Series), res.Series)
	}

	if res.Series[0].Value != 12.4 {
		t.Errorf("value=%v want 12.4", res.Series[0].Value)
	}
}

// TestWriter_RoundTripPodSampleWithFilter pins the (scope, name)
// filter — the chart axis identity. Two pods on different scopes
// emit lines; query for scope=x must return only one.
func TestWriter_RoundTripPodSampleWithFilter(t *testing.T) {
	w, dir := newTestWriter(t)

	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	for _, p := range []PodSample{
		{Ts: now, Container: "voodu-x-web.a", Kind: "deployment", Scope: "x", Name: "web", CPUPercent: 10.0},
		{Ts: now, Container: "voodu-y-web.b", Kind: "deployment", Scope: "y", Name: "web", CPUPercent: 99.9},
	} {
		if err := w.WritePod(p); err != nil {
			t.Fatal(err)
		}
	}

	_ = w.Close()

	res, err := Query(QueryOpts{
		Dir:      dir,
		Source:   SourcePod,
		Metric:   "cpu_percent",
		Scope:    "x",
		Name:     "web",
		Start:    now.Add(-time.Minute),
		End:      now.Add(time.Minute),
		Interval: 30 * time.Second,
		Now:      func() time.Time { return now.Add(time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Series) != 1 {
		t.Fatalf("series len=%d want 1", len(res.Series))
	}

	if res.Series[0].Value != 10.0 {
		t.Errorf("scope=x value=%v want 10.0 (NOT 99.9 — that's scope=y)", res.Series[0].Value)
	}
}

// TestReader_BucketAggregationAveragesAcrossSamples confirms the
// bucket = avg(sum/count) math. Three samples in one bucket → avg.
func TestReader_BucketAggregationAveragesAcrossSamples(t *testing.T) {
	w, dir := newTestWriter(t)

	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	for i, cpu := range []float64{10, 20, 30} {
		// 3 samples spaced 5s apart — all fall in one 60s bucket.
		if err := w.WriteSystem(SystemSample{
			Ts:         now.Add(time.Duration(i) * 5 * time.Second),
			CPUPercent: cpu,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_ = w.Close()

	res, err := Query(QueryOpts{
		Dir:      dir,
		Source:   SourceSystem,
		Metric:   "cpu_percent",
		Start:    now,
		End:      now.Add(time.Minute),
		Interval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Series) != 1 {
		t.Fatalf("series=%d want 1", len(res.Series))
	}

	if res.Series[0].Value != 20.0 {
		t.Errorf("avg=%v want 20.0 (mean of 10,20,30)", res.Series[0].Value)
	}
}

// TestReader_PartialLineToleratedSkipsAndContinues pins the
// "crash mid-write" corruption tolerance. Append a complete line,
// a partial line, then another complete line. Reader must skip
// the bad one and emit the two good ones.
func TestReader_PartialLineToleratedSkipsAndContinues(t *testing.T) {
	w, dir := newTestWriter(t)

	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	if err := w.WriteSystem(SystemSample{Ts: now, CPUPercent: 1.0}); err != nil {
		t.Fatal(err)
	}

	_ = w.Close()

	// Append a partial line directly to today's file (simulates
	// a writer crash mid-line).
	path := filepath.Join(dir, "metrics-"+now.Format("2006-01-02")+".ndjson")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.WriteString(`{"ts":"2026-05-24T12:01:00Z","source":"system","cpu_per`); err != nil {
		t.Fatal(err)
	}

	if _, err := f.WriteString("\n"); err != nil {
		t.Fatal(err)
	}

	_ = f.Close()

	// Reopen writer and emit one more good line.
	w2, err := NewWriter(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := w2.WriteSystem(SystemSample{Ts: now.Add(2 * time.Minute), CPUPercent: 3.0}); err != nil {
		t.Fatal(err)
	}

	_ = w2.Close()

	res, err := Query(QueryOpts{
		Dir:      dir,
		Source:   SourceSystem,
		Metric:   "cpu_percent",
		Start:    now,
		End:      now.Add(5 * time.Minute),
		Interval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Series) != 2 {
		t.Fatalf("series=%d want 2 (good before + good after, partial skipped); got %+v", len(res.Series), res.Series)
	}

	if res.Series[0].Value != 1.0 || res.Series[1].Value != 3.0 {
		t.Errorf("values=%v want [1.0, 3.0]", res.Series)
	}
}

// TestWriter_DayRolloverPicksFileBySampleTs is the day-boundary
// race guard. Two writes from a SINGLE writer instance with one
// before midnight and one after — they MUST land in different
// files (yesterday + today), regardless of which one opened the
// handle first.
func TestWriter_DayRolloverPicksFileBySampleTs(t *testing.T) {
	w, dir := newTestWriter(t)

	yesterday := time.Date(2026, 5, 23, 23, 59, 59, 0, time.UTC)
	today := time.Date(2026, 5, 24, 0, 0, 1, 0, time.UTC)

	if err := w.WriteSystem(SystemSample{Ts: yesterday, CPUPercent: 7}); err != nil {
		t.Fatal(err)
	}

	if err := w.WriteSystem(SystemSample{Ts: today, CPUPercent: 8}); err != nil {
		t.Fatal(err)
	}

	_ = w.Close()

	for _, want := range []string{"metrics-2026-05-23.ndjson", "metrics-2026-05-24.ndjson"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("expected file %s; not present: %v", want, err)
		}
	}
}

// TestSampler_OmitsDeltasOnFirstSampleAndAfterReset is the heart
// of the post-W6 counter semantics. First sighting → no deltas
// (no baseline). Second sighting → deltas appear. Counter reset
// (current < previous) → no deltas for the reset sample AND no
// deltas for the sample after that (need two clean baselines
// before resuming delta math).
func TestSampler_OmitsDeltasOnFirstSampleAndAfterReset(t *testing.T) {
	w, dir := newTestWriter(t)

	pods := &fakePodSource{}

	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	tickCount := 0

	sampler := &Sampler{
		Tick:   time.Second,
		Writer: w,
		Pods:   pods,
		Now: func() time.Time {
			defer func() { tickCount++ }()
			return now.Add(time.Duration(tickCount) * time.Second)
		},
	}

	// Tick 0: first sighting, NetRx=100. No deltas.
	pods.next = []PodRuntime{{Container: "c", Name: "n", NetRxBytes: 100}}
	sampler.evaluate(context.Background(), 0)

	// Tick 1: NetRx=150 → delta 50.
	pods.next = []PodRuntime{{Container: "c", Name: "n", NetRxBytes: 150}}
	sampler.evaluate(context.Background(), 1)

	// Tick 2: NetRx=50 (RESET — container restarted). No delta;
	// also marks post-reset so next tick has no delta either.
	pods.next = []PodRuntime{{Container: "c", Name: "n", NetRxBytes: 50}}
	sampler.evaluate(context.Background(), 2)

	// Tick 3: NetRx=80 — post-reset cleanup, baseline only, no delta.
	pods.next = []PodRuntime{{Container: "c", Name: "n", NetRxBytes: 80}}
	sampler.evaluate(context.Background(), 3)

	// Tick 4: NetRx=120 → delta 40 (normal).
	pods.next = []PodRuntime{{Container: "c", Name: "n", NetRxBytes: 120}}
	sampler.evaluate(context.Background(), 4)

	_ = w.Close()

	lines := readAllLines(t, dir)
	if len(lines) != 5 {
		t.Fatalf("expected 5 pod lines, got %d: %v", len(lines), lines)
	}

	// Decode each line and check whether net_rx_delta_bytes is
	// present.
	want := []struct {
		hasDelta bool
		delta    uint64
	}{
		{hasDelta: false},          // tick 0: first sighting
		{hasDelta: true, delta: 50}, // tick 1: normal delta
		{hasDelta: false},          // tick 2: reset detected
		{hasDelta: false},          // tick 3: post-reset cleanup
		{hasDelta: true, delta: 40}, // tick 4: back to normal
	}

	for i, line := range lines {
		var got map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d unmarshal: %v\n%s", i, err, line)
		}

		raw, present := got["net_rx_delta_bytes"]

		if want[i].hasDelta {
			if !present {
				t.Errorf("tick %d: expected net_rx_delta_bytes present, line=%s", i, line)
				continue
			}

			var d uint64
			_ = json.Unmarshal(raw, &d)

			if d != want[i].delta {
				t.Errorf("tick %d: delta=%d want %d", i, d, want[i].delta)
			}
		} else if present {
			t.Errorf("tick %d: expected net_rx_delta_bytes ABSENT, but got %s", i, raw)
		}
	}
}

// TestSampler_PrunesBaselineForVanishedContainer pins memory bound
// on the baselines map — a container that disappears for >5 ticks
// loses its baseline entry.
func TestSampler_PrunesBaselineForVanishedContainer(t *testing.T) {
	w, _ := newTestWriter(t)

	pods := &fakePodSource{}

	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	tickCount := 0

	sampler := &Sampler{
		Tick:   time.Second,
		Writer: w,
		Pods:   pods,
		Now: func() time.Time {
			defer func() { tickCount++ }()
			return now.Add(time.Duration(tickCount) * time.Second)
		},
	}

	// Tick 0: see container "c".
	pods.next = []PodRuntime{{Container: "c", Name: "n", NetRxBytes: 100}}
	sampler.evaluate(context.Background(), 0)

	// Ticks 1..6: container missing. After baselineStaleAfter (5)
	// it should be evicted.
	for i := uint64(1); i <= 7; i++ {
		pods.next = nil
		sampler.evaluate(context.Background(), i)
	}

	sampler.mu.Lock()
	defer sampler.mu.Unlock()

	if _, present := sampler.baselines["c"]; present {
		t.Errorf("baseline for vanished container should be evicted; map=%v", sampler.baselines)
	}
}

// TestCleanup_DropsOldKeepsRecentGzipsYesterday is the retention
// test. Set up files for today / yesterday / 8 days ago. Run
// cleanup with 7d retention. Expect:
//   - today: untouched (.ndjson)
//   - yesterday: gzipped (.ndjson.gz, original .ndjson removed)
//   - 8 days ago: deleted entirely
func TestCleanup_DropsOldKeepsRecentGzipsYesterday(t *testing.T) {
	dir := t.TempDir()

	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	mustWriteFile(t, dir, "metrics-2026-05-24.ndjson", `{"ts":"2026-05-24T11:00:00Z","source":"system","cpu_percent":1}`)
	mustWriteFile(t, dir, "metrics-2026-05-23.ndjson", `{"ts":"2026-05-23T11:00:00Z","source":"system","cpu_percent":2}`)
	mustWriteFile(t, dir, "metrics-2026-05-16.ndjson", `{"ts":"2026-05-16T11:00:00Z","source":"system","cpu_percent":3}`)

	if err := Cleanup(dir, now, 7*24*time.Hour, nil); err != nil {
		t.Fatal(err)
	}

	// Today: still raw.
	if _, err := os.Stat(filepath.Join(dir, "metrics-2026-05-24.ndjson")); err != nil {
		t.Errorf("today's raw file should remain: %v", err)
	}

	// Yesterday: gzipped, original gone.
	if _, err := os.Stat(filepath.Join(dir, "metrics-2026-05-23.ndjson.gz")); err != nil {
		t.Errorf("yesterday's file should be gzipped: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "metrics-2026-05-23.ndjson")); !os.IsNotExist(err) {
		t.Errorf("yesterday's raw should be removed; stat err=%v", err)
	}

	// 8 days ago: gone entirely.
	if _, err := os.Stat(filepath.Join(dir, "metrics-2026-05-16.ndjson")); !os.IsNotExist(err) {
		t.Errorf("8d-old file should be deleted; stat err=%v", err)
	}
}

// TestReader_ReadsGzippedHistoricalFiles confirms the reader
// transparently decompresses .ndjson.gz. Pin together with the
// Cleanup test so the rollover → read round trip works.
func TestReader_ReadsGzippedHistoricalFiles(t *testing.T) {
	dir := t.TempDir()

	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)

	// Write a gzipped fixture directly.
	gzPath := filepath.Join(dir, "metrics-2026-05-23.ndjson.gz")

	f, err := os.Create(gzPath)
	if err != nil {
		t.Fatal(err)
	}

	gz := gzip.NewWriter(f)

	line := []byte(`{"ts":"2026-05-23T12:00:00Z","source":"system","cpu_percent":42}` + "\n")
	if _, err := gz.Write(line); err != nil {
		t.Fatal(err)
	}

	_ = gz.Close()
	_ = f.Close()

	res, err := Query(QueryOpts{
		Dir:      dir,
		Source:   SourceSystem,
		Metric:   "cpu_percent",
		Start:    yesterday.Add(-time.Hour),
		End:      now,
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Series) != 1 {
		t.Fatalf("expected 1 series point from gzipped file, got %d", len(res.Series))
	}

	if res.Series[0].Value != 42 {
		t.Errorf("value=%v want 42", res.Series[0].Value)
	}
}

// TestReader_TruncatedFlagSet pins the "you asked for more than we
// have" UX. Disk has yesterday only; query asks for the last 30d.
// Response gets truncated=true + available_from=yesterday.
func TestReader_TruncatedFlagSet(t *testing.T) {
	dir := t.TempDir()

	mustWriteFile(t, dir, "metrics-2026-05-23.ndjson", `{"ts":"2026-05-23T12:00:00Z","source":"system","cpu_percent":1}`)

	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	res, err := Query(QueryOpts{
		Dir:      dir,
		Source:   SourceSystem,
		Metric:   "cpu_percent",
		Start:    now.Add(-30 * 24 * time.Hour),
		End:      now,
		Interval: time.Hour,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	if !res.Truncated {
		t.Errorf("expected Truncated=true")
	}

	wantAvail := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	if !res.AvailableFrom.Equal(wantAvail) {
		t.Errorf("AvailableFrom=%v want %v", res.AvailableFrom, wantAvail)
	}
}

// TestReader_MetricAllowList rejects arbitrary field names so
// queries can't fish into the wire shape via the URL.
func TestReader_MetricAllowList(t *testing.T) {
	dir := t.TempDir()

	_, err := Query(QueryOpts{
		Dir:      dir,
		Source:   SourceSystem,
		Metric:   "_secret_field",
		Start:    time.Now().Add(-time.Hour),
		End:      time.Now(),
		Interval: time.Minute,
	})

	if err == nil {
		t.Fatal("expected error for unknown metric")
	}

	if !strings.Contains(err.Error(), "unknown metric") {
		t.Errorf("error should mention unknown metric, got: %v", err)
	}
}

// TestReader_StreamingMemoryBoundedOnLargeFile confirms the
// streaming reader doesn't blow up memory on a sizeable input.
// Synth ~50k lines (~10 MB) and assert peak alloc stays modest.
func TestReader_StreamingMemoryBoundedOnLargeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "metrics-2026-05-24.ndjson")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	// ~50k lines. Each line ~200B = ~10 MB on disk.
	base := time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 50_000; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		line := []byte(`{"ts":"` + ts.Format(time.RFC3339) + `","source":"system","cpu_percent":12.4,"mem_used_bytes":1,"mem_total_bytes":2,"disk_used_bytes":3,"disk_total_bytes":4}` + "\n")
		_, _ = f.Write(line)
	}

	_ = f.Close()

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	_, err = Query(QueryOpts{
		Dir:      dir,
		Source:   SourceSystem,
		Metric:   "cpu_percent",
		Start:    base,
		End:      base.Add(50_000 * time.Second),
		Interval: time.Hour, // 14 buckets max
	})
	if err != nil {
		t.Fatal(err)
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// Generous ceiling — bucket+parse should be well under 20 MB
	// of heap churn. If this trips, someone re-introduced a "load
	// the whole file" path.
	delta := after.TotalAlloc - before.TotalAlloc

	if delta > 100*1024*1024 {
		t.Errorf("query allocated %.1f MB (>100 MB) — streaming likely broken", float64(delta)/(1024*1024))
	}
}

// ── helpers ───────────────────────────────────────────────────────

// fakePodSource feeds the sampler a canned slice per Collect.
// Tests set fake.next before each evaluate() call.
type fakePodSource struct {
	next []PodRuntime
}

func (f *fakePodSource) Collect(_ context.Context) ([]PodRuntime, error) {
	return f.next, nil
}

// fakeSystemSource for tests that exercise sampleSystem.
type fakeSystemSource struct {
	snap systemstats.Snapshot
}

func (f fakeSystemSource) Snapshot(_ context.Context) (systemstats.Snapshot, error) {
	return f.snap, nil
}

func readAllLines(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var out []string

	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "metrics-") {
			continue
		}

		path := filepath.Join(dir, e.Name())

		var r io.Reader

		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}

		defer f.Close()

		r = f

		if strings.HasSuffix(e.Name(), ".gz") {
			gz, err := gzip.NewReader(f)
			if err != nil {
				t.Fatal(err)
			}

			defer gz.Close()

			r = gz
		}

		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			t.Fatal(err)
		}

		for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}

			out = append(out, line)
		}
	}

	return out
}

func mustWriteFile(t *testing.T, dir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
