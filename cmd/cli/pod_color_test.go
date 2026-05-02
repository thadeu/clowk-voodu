package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestPodColorIndex_StableAcrossRuns pins the "same pod always
// gets the same color" invariant. Without this, the muscle-memory
// feature (operator learns `pod-a3f9 = cyan`) breaks: each `vd
// logs` invocation could pick different colors for the same name.
//
// FNV-32a is deterministic; the test guards against an accidental
// switch to a randomized hash or a palette reorder that would
// re-shuffle assignments.
func TestPodColorIndex_StableAcrossRuns(t *testing.T) {
	cases := []string{
		"clowk-lp-web.a3f9",
		"clowk-lp-web.bb01",
		"clowk-lp-redis.0",
		"prod-api.deadbeef",
	}

	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			first := podColorIndex(name)
			second := podColorIndex(name)
			third := podColorIndex(name)

			if first != second || second != third {
				t.Errorf("podColorIndex(%q) flapped: %d, %d, %d (must be stable)",
					name, first, second, third)
			}

			if first < 0 || first >= len(podColorPalette) {
				t.Errorf("podColorIndex(%q) = %d, out of palette range [0, %d)",
					name, first, len(podColorPalette))
			}
		})
	}
}

// TestPodColorIndex_DistributesAcrossPalette confirms the hash
// doesn't collapse onto a single slot. With 6 slots and a handful
// of typical pod names, we expect at least 3 distinct colors —
// enough that operators see distinct hues in a typical multi-pod
// stream.
//
// Deterministic input set so a future palette resize doesn't
// silently drop the test's distinguishing power.
func TestPodColorIndex_DistributesAcrossPalette(t *testing.T) {
	names := []string{
		"clowk-lp-web.a3f9",
		"clowk-lp-web.bb01",
		"clowk-lp-web.c2d8",
		"clowk-lp-web.d4e8",
		"clowk-lp-web.e5f9",
		"clowk-lp-web.f8d2",
	}

	seen := map[int]bool{}
	for _, n := range names {
		seen[podColorIndex(n)] = true
	}

	if len(seen) < 3 {
		t.Errorf("podColorIndex collapses %d names onto %d slots; expected ≥3 distinct (hash distribution broken or palette too small)",
			len(names), len(seen))
	}
}

// TestNewPodPalette_NoColorEnvStripsANSI is the no-color contract:
// when NO_COLOR is set (per no-color.org), the palette returns
// plain text — no escape codes — even when called against a
// fake-tty writer. Without this, CI logs and `vd logs ... | grep`
// would have ANSI gibberish.
func TestNewPodPalette_NoColorEnvStripsANSI(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("FORCE_COLOR", "")

	var buf bytes.Buffer

	p := newPodPalette(&buf)

	got := p.ColorFor("clowk-lp-web.a3f9")("[clowk-lp-web.a3f9]")

	if strings.Contains(got, "\x1b[") {
		t.Errorf("NO_COLOR set but output has ANSI escapes: %q", got)
	}

	if got != "[clowk-lp-web.a3f9]" {
		t.Errorf("NO_COLOR should return plain text verbatim; got %q", got)
	}
}

// TestNewPodPalette_ForceColorEmitsANSI: pipes / non-tty writers
// (test buffers, file redirects) get plain text by default. But
// FORCE_COLOR is the operator's "I know what I'm doing, give me
// colors anyway" override — common when piping through `less -R`
// or capturing logs to a file the operator will open in an
// ANSI-aware viewer.
func TestNewPodPalette_ForceColorEmitsANSI(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "1")

	var buf bytes.Buffer

	p := newPodPalette(&buf)

	got := p.ColorFor("clowk-lp-web.a3f9")("[clowk-lp-web.a3f9]")

	if !strings.Contains(got, "\x1b[") {
		t.Errorf("FORCE_COLOR set but no ANSI in output: %q", got)
	}

	// Reset sequence at the end is the standard "stop coloring"
	// marker — without it, the color bleeds into anything that
	// follows.
	if !strings.Contains(got, "\x1b[0m") {
		t.Errorf("colorized output missing reset sequence: %q", got)
	}
}

// TestNewPodPalette_NoColorBeatsForceColor pins the precedence
// per no-color.org: NO_COLOR is the user's explicit "no colors,
// I mean it" — beats FORCE_COLOR when both are set (e.g.,
// FORCE_COLOR exported globally in shell rc, NO_COLOR set
// per-command). Wrong precedence here would leak escapes into
// CI logs that operators thought they'd disabled.
func TestNewPodPalette_NoColorBeatsForceColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("FORCE_COLOR", "1")

	var buf bytes.Buffer

	p := newPodPalette(&buf)

	got := p.ColorFor("clowk-lp-web.a3f9")("test")

	if strings.Contains(got, "\x1b[") {
		t.Errorf("NO_COLOR must beat FORCE_COLOR; got ANSI in output: %q", got)
	}
}
