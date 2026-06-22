package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/progress"
)

func TestSanitizeTailLine(t *testing.T) {
	cases := map[string]string{
		"#5 [4/5] RUN bun install":      "#5 [4/5] RUN bun install",
		"\x1b[1m\x1b[32m#5 done\x1b[0m": "#5 done",
		"col\tone\ttwo":                 "col one two",
		"trailing   ":                   "trailing",
		"with\rcarriage":                "withcarriage",
	}

	for in, want := range cases {
		if got := sanitizeTailLine(in); got != want {
			t.Errorf("sanitizeTailLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateVisible(t *testing.T) {
	if got := truncateVisible("short", 20); got != "short" {
		t.Errorf("no-truncate: got %q", got)
	}

	if got := truncateVisible("abcdefgh", 5); got != "abcd…" {
		t.Errorf("truncate: got %q, want %q", got, "abcd…")
	}

	if got := truncateVisible("anything", 1); got != "" {
		t.Errorf("max<=1 should be empty, got %q", got)
	}
}

// Raw build sub-output (non-JSON frames) during an active step feeds the
// tail ring, capped at buildTailN — oldest lines fall off.
func TestEventRendererBuildTailRing(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(t, &buf, false)

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
		{Type: progress.EventStepStart, ID: "build", Label: "building release"},
	})

	total := buildTailN + 2

	for i := 1; i <= total; i++ {
		_, _ = fmt.Fprintf(r, "line %d\n", i)
	}

	r.mu.Lock()
	tail := append([]string{}, r.tail...)
	r.mu.Unlock()

	if len(tail) != buildTailN {
		t.Fatalf("tail len = %d, want %d (%v)", len(tail), buildTailN, tail)
	}

	// Oldest two fell off: the window is the last buildTailN lines.
	wantFirst := fmt.Sprintf("line %d", total-buildTailN+1)
	wantLast := fmt.Sprintf("line %d", total)

	if tail[0] != wantFirst || tail[buildTailN-1] != wantLast {
		t.Errorf("ring kept the wrong window: %v (want %s … %s)", tail, wantFirst, wantLast)
	}
}

// On step failure the build tail is preserved as committed rows beneath
// the ✗, above the reconciler error — the operator's chosen behavior.
func TestEventRendererFailureKeepsBuildTail(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(t, &buf, false)

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
		{Type: progress.EventStepStart, ID: "build", Label: "building release"},
	})

	for _, ln := range []string{"#2 step two", "#3 step three", "#4 step four", "#5 step five"} {
		_, _ = r.Write([]byte(ln + "\n"))
	}

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventStepEnd, ID: "build", Status: progress.StatusFail, Error: "build release: exit status 1"},
	})

	_ = r.Close()

	got := stripANSI(buf.String())

	for _, want := range []string{"#5 step five", "✗ building release", "build release: exit status 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("failure output missing %q:\n%s", want, got)
		}
	}
}

// A blank line in the build output during an active step must be
// swallowed — a bare newline would push the cursor below the live block
// and the next redraw would paint the spinner one row lower (the stacked-
// spinner ghosting). Asserted directly: processing "\n" writes nothing.
func TestEventRendererBlankLineSwallowedDuringStep(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(t, &buf, false)

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
		{Type: progress.EventStepStart, ID: "build", Label: "building release"},
	})

	before := buf.Len()

	_, _ = r.Write([]byte("\n"))

	if buf.Len() != before {
		t.Errorf("blank line during active step must write nothing; buf grew by %d bytes: %q",
			buf.Len()-before, buf.String()[before:])
	}
}

// Outside an active step a blank line is a real visual separator and
// passes through.
func TestEventRendererBlankLinePassthroughWhenIdle(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(t, &buf, false)

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
	})

	before := buf.Len()

	_, _ = r.Write([]byte("\n"))

	if buf.Len() == before {
		t.Error("blank line when idle should pass through as a separator")
	}
}

// On step success the committed output is just the ✓ line — the build
// tail block collapses (no committed sub-output rows). Asserted on the
// post-Close buffer: success must NOT print the tail as committed rows
// the way failure does.
func TestEventRendererSuccessShowsOnlyCheckLine(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(t, &buf, false)

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
		{Type: progress.EventStepStart, ID: "build", Label: "building release"},
		{Type: progress.EventStepEnd, ID: "build", Status: progress.StatusOK},
	})

	_ = r.Close()

	got := stripANSI(buf.String())

	if !strings.Contains(got, "✓ building release") {
		t.Errorf("expected committed ✓ line:\n%s", got)
	}
}
