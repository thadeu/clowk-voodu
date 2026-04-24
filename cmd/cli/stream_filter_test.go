package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// ansiRe matches every ANSI escape sequence we emit: CSI (\x1b[...m,
// \x1b[2K, \x1b[NX). Used by tests to assert on the human-readable
// substrate without tripping on color/clear-line bytes interleaved
// between visible glyphs.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// forceFilter constructs a progressFilter with the TTY gate flipped on
// so tests can exercise the state machine even though the test binary
// writes to a pipe. Without this, every test would fall through the
// `!f.tty` escape hatch and behave as passthrough — masking real
// parser bugs.
func forceFilter(out *bytes.Buffer, verbose bool) *progressFilter {
	f := newProgressFilter(out, verbose)
	f.tty = true

	return f
}

// TestProgressFilterVerboseIsPassthrough locks in the escape hatch:
// when the user asked for --verbose, every byte we receive lands on
// stdout unchanged, in order, no spinner, no buildx filtering. This
// is how we stay debuggable when a build misbehaves.
func TestProgressFilterVerboseIsPassthrough(t *testing.T) {
	var out bytes.Buffer

	f := forceFilter(&out, true)

	payload := "-----> Building release...\n#1 transferring\n#2 DONE\n-----> Build completed\n"

	if _, err := f.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}

	_ = f.Close()

	if out.String() != payload {
		t.Errorf("verbose mode must passthrough:\n got:  %q\n want: %q", out.String(), payload)
	}
}

// TestProgressFilterNonTTYIsPassthrough mirrors the verbose case for
// the other escape hatch: when stdout is not a TTY (piped to file,
// CI log, `> out.txt`), the filter disables itself. We explicitly
// build the filter with newProgressFilter so tty is detected as
// false (bytes.Buffer isn't an *os.File).
func TestProgressFilterNonTTYIsPassthrough(t *testing.T) {
	var out bytes.Buffer

	f := newProgressFilter(&out, false)

	payload := "-----> Building release...\n#1 transferring\n-----> Build completed\n"

	if _, err := f.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}

	_ = f.Close()

	if out.String() != payload {
		t.Errorf("non-tty mode must passthrough:\n got:  %q\n want: %q", out.String(), payload)
	}
}

// TestProgressFilterCollapsesBuildBlock is the happy path: between a
// start and end marker, docker buildx chatter disappears, and the
// block is bookended by the marker line + a ✓ summary. What lands on
// stdout should be short and readable.
func TestProgressFilterCollapsesBuildBlock(t *testing.T) {
	var out bytes.Buffer

	f := forceFilter(&out, false)

	rememberShippedTag("web")

	// Three phases: pre-build narrative, noisy build, post-build.
	payload := strings.Join([]string{
		"-----> Shipping web (scope: softphone, context: .)",
		"-----> Creating release fb4b418b872f",
		"-----> Building release...",
		"-----> Using explicit lang strategy: \"nodejs\" (from manifest)",
		"#0 building with \"default\" instance using docker driver",
		"#1 [internal] load build definition from Dockerfile",
		"#2 DONE 0.1s",
		"-----> Node.js build complete!",
		"-----> Build completed",
		"Deploy completed successfully for 'softphone-web'",
		"-----> Pruned 1 old release(s)",
		"",
	}, "\n")

	if _, err := f.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}

	_ = f.Close()

	got := stripANSI(out.String())

	// Must keep: each step banner becomes a committed `✓ <step> (Ns)`
	// line when it closes. Pruned is a passthrough banner that emits
	// its own ✓ inline. Build completed → overall `✓ Built <tag> in
	// Ns` summary.
	for _, want := range []string{
		"✓ Shipping web",
		"✓ Creating release fb4b418b872f",
		"✓ Building release...",
		"✓ Built web in",
		"✓ Pruned 1 old release(s)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}

	// Must drop: the raw `----->` prefixed banners (replaced by their
	// ✓ commits), docker buildx `#N` chatter, plugin sub-banners, AND
	// the redundant "Deploy completed successfully …" line (the ✓ Built
	// summary above already carries the same success signal).
	for _, banned := range []string{
		"-----> Shipping",
		"-----> Creating release",
		"-----> Building release",
		"-----> Pruned",
		"-----> Build completed",
		"Deploy completed successfully",
		"#0 building",
		"#1 [internal]",
		"#2 DONE",
		"-----> Using explicit lang strategy",
		"-----> Node.js build complete",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("unexpected %q leaked into filtered output:\n%s", banned, got)
		}
	}
}

// TestIsBuildxNoise locks in the classifier the spinner uses to decide
// what to swallow. It must be narrow: matches `#<digits> [space] ...`
// exactly, so any plugin or error output that happens to start with
// `#` (e.g. a shell comment echoed for debug) stays visible.
func TestIsBuildxNoise(t *testing.T) {
	noise := []string{
		"#0 building with default",
		"#1 [internal] load build definition",
		"#12 CACHED",
		"#5 resolve docker.io/library/...",
	}

	for _, s := range noise {
		if !isBuildxNoise(s) {
			t.Errorf("expected noise: %q", s)
		}
	}

	signal := []string{
		"",
		"#",
		"# comment with space",
		"#abc not digits",
		"-----> Building release...",
		"ERROR: failed to solve",
		"Deploy completed successfully",
	}

	for _, s := range signal {
		if isBuildxNoise(s) {
			t.Errorf("expected signal (not noise): %q", s)
		}
	}
}

// TestProgressFilterCloseFlushesPartialLine guards a subtle corner:
// the last line of the SSH stream might lack a trailing newline
// (process exited, or stdout buffer snapshot). Close() flushes the
// leftover buffer so nothing goes missing.
func TestProgressFilterCloseFlushesPartialLine(t *testing.T) {
	var out bytes.Buffer

	f := forceFilter(&out, false)

	// No trailing newline — processLineLocked never fires.
	if _, err := f.Write([]byte("final line without newline")); err != nil {
		t.Fatal(err)
	}

	_ = f.Close()

	if !strings.Contains(out.String(), "final line without newline") {
		t.Errorf("Close() must flush leftover, got: %q", out.String())
	}
}

// TestProgressFilterCloseClearsDanglingSpinner handles the crash
// scenario: we entered a build block, docker exploded, SSH died
// before emitting `-----> Build completed`. Close() must stop the
// ticker goroutine and clear the spinner line so the user isn't
// greeted by a frozen `⠼ Building web... (42s)` forever.
func TestProgressFilterCloseClearsDanglingSpinner(t *testing.T) {
	var out bytes.Buffer

	f := forceFilter(&out, false)

	rememberShippedTag("api")

	payload := strings.Join([]string{
		"-----> Building release...",
		"#1 building",
		"",
	}, "\n")

	if _, err := f.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}

	// No build completed marker — simulates abrupt termination.
	if err := f.Close(); err != nil {
		t.Fatalf("Close err = %v", err)
	}

	// Close must not hang. If we got here, the spinner goroutine
	// stopped cleanly. We don't assert on specific ANSI bytes — the
	// line is cleared with \r\x1b[2K which is hard to pin down
	// without overfitting — but the test would deadlock if
	// stopSpinnerLocked did.
	if f.active {
		t.Error("filter still in active state after Close")
	}
}
