// Tests for the docker stats parsing layer. We don't shell out to
// `docker stats` in unit tests — that's an integration concern and
// would make the suite non-hermetic. Instead, every parser case
// uses raw byte fixtures captured from real docker output, so a
// future format change shows up as a test failure rather than a
// silent runtime panic.

package docker

import (
	"strings"
	"testing"
)

// TestParseBytes_RoundTripsDockerUnits pins the unit vocabulary
// docker emits: binary (MiB, GiB) for memory, decimal (kB, MB) for
// network/block I/O. Each entry uses the exact-precision case so
// we can assert == on uint64 without float fuzziness.
func TestParseBytes_RoundTripsDockerUnits(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"0B", 0},
		{"1B", 1},
		{"1024B", 1024},
		{"1KiB", 1024},
		{"1MiB", 1024 * 1024},
		{"1GiB", 1024 * 1024 * 1024},
		{"1kB", 1000},
		{"1MB", 1000 * 1000},
		{"1GB", 1000 * 1000 * 1000},
		{"100MiB", 100 * 1024 * 1024},
		{"", 0}, // empty → 0 (consistent with "--" handling upstream)
	}

	for _, c := range cases {
		got, err := parseBytes(c.in)
		if err != nil {
			t.Errorf("parseBytes(%q): unexpected error %v", c.in, err)
			continue
		}

		if got != c.want {
			t.Errorf("parseBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestParseBytes_FractionalSizes pins that decimal-point sizes
// (the common case in real output: "112.1MiB") parse without
// losing precision beyond what float64 inherently allows.
func TestParseBytes_FractionalSizes(t *testing.T) {
	got, err := parseBytes("112.1MiB")
	if err != nil {
		t.Fatal(err)
	}

	// 112.1 * 1024 * 1024 = 117545369.6 → truncated to 117545369.
	// Compute via float64 var to dodge Go's compile-time rejection
	// of fractional-float-to-uint constant conversions.
	v := 112.1 * 1024 * 1024
	want := uint64(v)

	if got != want {
		t.Errorf("parseBytes(112.1MiB) = %d, want %d", got, want)
	}
}

// TestParseBytes_UnknownUnitErrors guarantees an unknown unit
// surfaces loudly instead of silently mapping to 0. A future
// docker version emitting "PiB" should fail tests, prompting an
// update to the memUnit map — much better than every consumer
// seeing 0 bytes and guessing why.
func TestParseBytes_UnknownUnitErrors(t *testing.T) {
	_, err := parseBytes("5PiB")
	if err == nil {
		t.Fatal("expected error on unknown unit PiB")
	}

	if !strings.Contains(err.Error(), "unknown unit") {
		t.Errorf("expected 'unknown unit' in error, got: %v", err)
	}
}

// TestParseBytes_NegativeIsError covers a defensive case: docker
// shouldn't emit negatives, but if a future format introduces
// signed deltas we want a hard error rather than a silent uint
// wraparound.
func TestParseBytes_NegativeIsError(t *testing.T) {
	_, err := parseBytes("-1MiB")
	if err == nil {
		t.Fatal("expected error on negative size")
	}
}

// TestParsePercent_StripsSuffix covers the common case + the
// "--" sentinel docker uses for stopped containers (which we map
// to 0 rather than error so the parser stays robust against a
// race where stats are queried mid-stop).
func TestParsePercent_StripsSuffix(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"0.14%", 0.14},
		{"5.70%", 5.70},
		{"100%", 100},
		{"0%", 0},
		{"--", 0},
		{"", 0},
	}

	for _, c := range cases {
		got, err := parsePercent(c.in)
		if err != nil {
			t.Errorf("parsePercent(%q): %v", c.in, err)
			continue
		}

		if got != c.want {
			t.Errorf("parsePercent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestParseDualBytes_SplitsUsedLimit pins the canonical
// "112.1MiB / 1.921GiB" docker format. Both halves go through
// parseBytes, so unit coverage rides on parseBytes tests; here we
// only check the split + plumbing.
func TestParseDualBytes_SplitsUsedLimit(t *testing.T) {
	used, limit, err := parseDualBytes("112.1MiB / 1.921GiB")
	if err != nil {
		t.Fatal(err)
	}

	// Variables (not constants) so Go's fractional-float→uint
	// constant rejection doesn't fire — we want runtime truncation.
	usedF := 112.1 * 1024 * 1024
	wantUsed := uint64(usedF)

	if used != wantUsed {
		t.Errorf("used: got %d, want %d", used, wantUsed)
	}

	limitF := 1.921 * 1024 * 1024 * 1024
	wantLimit := uint64(limitF)

	if limit != wantLimit {
		t.Errorf("limit: got %d, want %d", limit, wantLimit)
	}
}

// TestParseDualBytes_MalformedErrors — anything without exactly one
// slash separator is unrecognisable. The parser shouldn't try to
// guess; emit an error and let the caller surface it.
func TestParseDualBytes_MalformedErrors(t *testing.T) {
	_, _, err := parseDualBytes("112.1MiB")
	if err == nil {
		t.Fatal("expected error on missing slash")
	}
}

// TestParseStatsLine_HappyPath pins the end-to-end JSON shape
// docker emits with `--format json`. The fixture is captured
// verbatim from a real `docker stats --no-stream --format
// '{{json .}}' redis-stack` invocation.
func TestParseStatsLine_HappyPath(t *testing.T) {
	line := `{"BlockIO":"2.04GB / 1.21MB","CPUPerc":"0.14%","Container":"355f39079962933acf3143762a25b520783193d97e3a21c3ae23fb514dc63ce5","ID":"355f39079962","MemPerc":"5.70%","MemUsage":"112.1MiB / 1.921GiB","Name":"redis-stack","NetIO":"338kB / 41.7kB","PIDs":"19"}`

	stats, err := parseStatsLine(line)
	if err != nil {
		t.Fatal(err)
	}

	if stats.Name != "redis-stack" {
		t.Errorf("name: %q", stats.Name)
	}

	if stats.ID != "355f39079962" {
		t.Errorf("id: %q", stats.ID)
	}

	if stats.CPUPercent != 0.14 {
		t.Errorf("cpu: %v", stats.CPUPercent)
	}

	if stats.MemPercent != 5.70 {
		t.Errorf("mem_percent: %v", stats.MemPercent)
	}

	if stats.PIDs != 19 {
		t.Errorf("pids: %d", stats.PIDs)
	}

	memF := 112.1 * 1024 * 1024
	wantMem := uint64(memF)
	if stats.MemUsageBytes != wantMem {
		t.Errorf("mem_usage: got %d, want %d", stats.MemUsageBytes, wantMem)
	}

	// NetIO "338kB / 41.7kB" → 338000 rx, 41700 tx (docker uses
	// decimal kB for network — 1kB = 1000B).
	if stats.NetRxBytes != 338_000 {
		t.Errorf("net_rx_bytes: got %d, want 338000", stats.NetRxBytes)
	}

	if stats.NetTxBytes != 41_700 {
		t.Errorf("net_tx_bytes: got %d, want 41700", stats.NetTxBytes)
	}

	// BlockIO "2.04GB / 1.21MB" → 2.04e9 read, 1.21e6 write.
	wantBlkRead := uint64(2.04 * 1000 * 1000 * 1000)
	if stats.BlockReadBytes != wantBlkRead {
		t.Errorf("block_read_bytes: got %d, want %d", stats.BlockReadBytes, wantBlkRead)
	}

	wantBlkWrite := uint64(1.21 * 1000 * 1000)
	if stats.BlockWriteBytes != wantBlkWrite {
		t.Errorf("block_write_bytes: got %d, want %d", stats.BlockWriteBytes, wantBlkWrite)
	}
}

// TestParseStatsOutput_MultipleLines pins the typical multi-
// container output (line-delimited JSON, one container per line).
// Tests that blank lines (trailing newline) are skipped without
// erroring — `docker stats` always trails a newline.
func TestParseStatsOutput_MultipleLines(t *testing.T) {
	raw := []byte(`{"Name":"a","ID":"aaa","CPUPerc":"1%","MemUsage":"10MiB / 100MiB","MemPerc":"10%","PIDs":"5"}
{"Name":"b","ID":"bbb","CPUPerc":"2%","MemUsage":"20MiB / 100MiB","MemPerc":"20%","PIDs":"7"}

`)

	out, err := parseStatsOutput(raw)
	if err != nil {
		t.Fatal(err)
	}

	if len(out) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(out), out)
	}

	if out[0].Name != "a" || out[1].Name != "b" {
		t.Errorf("order/values wrong: %+v", out)
	}
}

// TestParseStatsOutput_MalformedLineSurfacesError makes sure a
// future docker change that breaks the format produces a test
// failure rather than silently dropping a row. The line number is
// part of the error so the operator (or test author) can grep the
// daemon output for the offender.
func TestParseStatsOutput_MalformedLineSurfacesError(t *testing.T) {
	raw := []byte("{not json}\n")

	_, err := parseStatsOutput(raw)
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}

	if !strings.Contains(err.Error(), "line 1") {
		t.Errorf("error should pinpoint the offending line: %v", err)
	}
}

// TestParseStatsOutput_Empty covers the no-running-containers
// case — docker emits an empty stream (or just a newline). The
// parser should return an empty slice without error.
func TestParseStatsOutput_Empty(t *testing.T) {
	for _, input := range []string{"", "\n", "   \n  "} {
		out, err := parseStatsOutput([]byte(input))
		if err != nil {
			t.Errorf("input %q: unexpected error %v", input, err)
		}

		if len(out) != 0 {
			t.Errorf("input %q: expected empty, got %+v", input, out)
		}
	}
}
