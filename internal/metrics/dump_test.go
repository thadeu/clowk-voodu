package metrics

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDump_FiltersBySince covers the core invariant: lines whose
// ts ≤ since are NOT emitted; lines with ts > since are emitted
// verbatim. Operator pain if this regresses: WebUI either re-pulls
// the same rows every tick (warehouse bloats with dupes) or misses
// rows on the boundary (chart has holes).
func TestDump_FiltersBySince(t *testing.T) {
	dir := t.TempDir()

	// Three rows spaced 1s apart. Boundary row sits exactly at since;
	// dump uses strict > so it must NOT appear in the output.
	lines := []string{
		`{"ts":"2026-05-25T10:00:00Z","source":"system","cpu_percent":10.0}`,
		`{"ts":"2026-05-25T10:00:01Z","source":"system","cpu_percent":11.0}`, // == since, excluded
		`{"ts":"2026-05-25T10:00:02Z","source":"system","cpu_percent":12.0}`,
	}

	// Filename date MUST match the rows' date: listFiles picks files by
	// the [Since, Now] day window, so a file dated outside it is skipped
	// entirely. Using a literal date (not time.Now()) keeps the test
	// deterministic regardless of the wall clock.
	writeNDJSON(t, filepath.Join(dir, "metrics-2026-05-25.ndjson"), lines)

	since, _ := time.Parse(time.RFC3339, "2026-05-25T10:00:01Z")
	now, _ := time.Parse(time.RFC3339, "2026-05-25T10:00:03Z")

	var buf bytes.Buffer
	if err := Dump(&buf, DumpOpts{
		Dir:   dir,
		Since: since,
		Now:   func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	out := buf.String()

	if strings.Contains(out, `"cpu_percent":10`) {
		t.Errorf("row before since must not be emitted; got: %s", out)
	}
	if strings.Contains(out, `"cpu_percent":11`) {
		t.Errorf("row AT since must not be emitted (strict >); got: %s", out)
	}
	if !strings.Contains(out, `"cpu_percent":12`) {
		t.Errorf("row after since must be emitted; got: %s", out)
	}
}

// TestDump_PassesGzipRotated confirms we transparently decompress
// rolled-over daily files. The cleanup job gzips yesterday's file
// at midnight UTC; the warehouse sync needs to keep working across
// that boundary.
func TestDump_PassesGzipRotated(t *testing.T) {
	dir := t.TempDir()

	// "Yesterday" gzipped + "today" plain.
	writeGzipNDJSON(t, filepath.Join(dir, "metrics-2026-05-24.ndjson.gz"), []string{
		`{"ts":"2026-05-24T23:59:50Z","source":"system","cpu_percent":7.0}`,
	})
	writeNDJSON(t, filepath.Join(dir, "metrics-2026-05-25.ndjson"), []string{
		`{"ts":"2026-05-25T00:00:10Z","source":"system","cpu_percent":8.0}`,
	})

	now, _ := time.Parse(time.RFC3339, "2026-05-25T00:01:00Z")

	var buf bytes.Buffer
	if err := Dump(&buf, DumpOpts{
		Dir:   dir,
		Since: now.Add(-time.Hour),
		Now:   func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, `"cpu_percent":7`) {
		t.Errorf("gzipped file row missing; got: %s", out)
	}
	if !strings.Contains(out, `"cpu_percent":8`) {
		t.Errorf("plain file row missing; got: %s", out)
	}
}

// TestDump_TolerantOfCorruptLine — a partial last line from a
// crash-mid-write must NOT poison the whole stream. Confirmed
// counterpart behaviour exists in reader.go (streamFile); dump must
// match because it shares the same on-disk source of truth.
func TestDump_TolerantOfCorruptLine(t *testing.T) {
	dir := t.TempDir()
	// Literal date matching the rows below — see TestDump_FiltersBySince
	// for why a time.Now()-derived filename drifts out of the day window.
	path := filepath.Join(dir, "metrics-2026-05-25.ndjson")

	body := strings.Join([]string{
		`{"ts":"2026-05-25T10:00:00Z","source":"system","cpu_percent":10.0}`,
		`{"ts":"2026-05-25T10:00:01Z","source":"system","cpu_pe`, // truncated
		`{"ts":"2026-05-25T10:00:02Z","source":"system","cpu_percent":12.0}`,
		"",
	}, "\n")

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	now, _ := time.Parse(time.RFC3339, "2026-05-25T10:01:00Z")

	var buf bytes.Buffer
	if err := Dump(&buf, DumpOpts{
		Dir:   dir,
		Since: time.Time{}, // backfill
		Now:   func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, `"cpu_percent":10`) {
		t.Errorf("pre-corrupt row missing; got: %s", out)
	}
	if !strings.Contains(out, `"cpu_percent":12`) {
		t.Errorf("post-corrupt row missing; got: %s", out)
	}
	// 3 lines: only the 2 valid + the corrupt one is dropped (no
	// emission). Newline terminators = 2 newlines if dump appends.
	if strings.Count(out, "\n") != 2 {
		t.Errorf("expected exactly 2 emitted lines; got %d in %q",
			strings.Count(out, "\n"), out)
	}
}

// TestDump_EmptyOnNoMatch — when the warehouse is fully caught up
// (since equals MAX(ts) in store), the response must be EMPTY, not
// error. WebUI's recurring tick will fire many of these; an error
// path here means false-alarm logs every 30s.
func TestDump_EmptyOnNoMatch(t *testing.T) {
	dir := t.TempDir()

	// Literal date matching the row — with a time.Now()-derived name the
	// file falls outside the day window and the dump comes back empty
	// for the WRONG reason (file skipped, not since-filtered). Pinning
	// the date makes this genuinely exercise the "caught up → empty" path.
	writeNDJSON(t, filepath.Join(dir, "metrics-2026-05-25.ndjson"), []string{
		`{"ts":"2026-05-25T10:00:00Z","source":"system","cpu_percent":10.0}`,
	})

	now, _ := time.Parse(time.RFC3339, "2026-05-25T10:01:00Z")
	since, _ := time.Parse(time.RFC3339, "2026-05-25T10:00:30Z") // after the only row

	var buf bytes.Buffer
	if err := Dump(&buf, DumpOpts{
		Dir:   dir,
		Since: since,
		Now:   func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	if buf.Len() != 0 {
		t.Errorf("expected empty dump when caught up; got %d bytes: %q",
			buf.Len(), buf.String())
	}
}

// TestDump_ChronologicalOrder — lines must come out in ts order
// across files. WebUI's bulk_insert relies on MAX(ts_epoch) being
// monotonic over the response so a job crash mid-batch leaves
// the watermark consistent.
func TestDump_ChronologicalOrder(t *testing.T) {
	dir := t.TempDir()

	writeGzipNDJSON(t, filepath.Join(dir, "metrics-2026-05-24.ndjson.gz"), []string{
		`{"ts":"2026-05-24T23:00:00Z","source":"system","cpu_percent":1.0}`,
	})
	writeNDJSON(t, filepath.Join(dir, "metrics-2026-05-25.ndjson"), []string{
		`{"ts":"2026-05-25T00:00:00Z","source":"system","cpu_percent":2.0}`,
		`{"ts":"2026-05-25T01:00:00Z","source":"system","cpu_percent":3.0}`,
	})

	now, _ := time.Parse(time.RFC3339, "2026-05-25T02:00:00Z")

	var buf bytes.Buffer
	if err := Dump(&buf, DumpOpts{
		Dir: dir,
		Now: func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}

	// Crude order check — cpu_percent values are 1, 2, 3 in
	// chronological order; if they appear in that sequence the
	// listFiles + dumpFile combo respected the day ordering.
	wantOrder := []string{`"cpu_percent":1`, `"cpu_percent":2`, `"cpu_percent":3`}
	for i, want := range wantOrder {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d: want substring %q, got %q", i, want, lines[i])
		}
	}
}

// ── helpers ─────────────────────────────────────────────────────

func writeNDJSON(t *testing.T, path string, lines []string) {
	t.Helper()
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeGzipNDJSON(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	body := strings.Join(lines, "\n") + "\n"
	if _, err := fmt.Fprint(gz, body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
}
