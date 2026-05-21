package main

import (
	"strings"
	"testing"
)

// TestColorize_NoColorStripsEscapes pins that NO_COLOR=1 returns the
// bare symbol with no ANSI bytes. Operators relying on no-color.org
// semantics (CI logs, screen readers, accessibility-conscious shells)
// must see clean text — a regression here would break those workflows
// silently.
func TestColorize_NoColorStripsEscapes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	noColor = true // package-level cache normally set at init
	defer func() { noColor = false }()

	got := colorize(cMint400, "x")
	if got != "x" {
		t.Errorf("NO_COLOR set: want %q, got %q (contains escapes? %v)", "x", got, strings.Contains(got, "\x1b"))
	}
}

// TestColorize_EmitsTruecolor pins the wire format — the renderer
// gambles on terminals being modern enough for 24-bit color. A
// regression that fell back to 256 or 16 colors would dim the brand
// signal even on terminals that support more.
func TestColorize_EmitsTruecolor(t *testing.T) {
	noColor = false

	got := colorize(cMint400, "x")

	// Must contain the truecolor prefix + reset.
	if !strings.Contains(got, "\x1b[38;2;111;226;166m") {
		t.Errorf("missing mint-400 truecolor prefix in: %q", got)
	}

	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("missing reset suffix in: %q", got)
	}
}

// TestCheckVsCheckFinal pins the brand kit's "aurora reserved for
// terminal success" rule: check() uses mint-400, checkFinal() uses
// aurora. Two visually distinct ✓s — operator scanning a long apply
// can tell intermediate step completions from the final "everything's
// done" line at a glance.
func TestCheckVsCheckFinal(t *testing.T) {
	noColor = false

	intermediate := check()
	final := checkFinal()

	if !strings.Contains(intermediate, "111;226;166") {
		t.Errorf("check() should use mint-400 #6FE2A6, got: %q", intermediate)
	}

	if !strings.Contains(final, "199;245;221") {
		t.Errorf("checkFinal() should use aurora #C7F5DD, got: %q", final)
	}

	if intermediate == final {
		t.Error("check() and checkFinal() must produce visually distinct output")
	}
}

// TestDiffSymbolsHaveDistinctColors pins the +/~/- vocabulary. A
// regression that painted all three with the same color would
// flatten the diff visual and force operators to read every line
// to identify the operation class.
func TestDiffSymbolsHaveDistinctColors(t *testing.T) {
	noColor = false

	add := plus()
	mod := tilde()
	del := minusPruned()

	if add == mod || mod == del || add == del {
		t.Errorf("diff symbols must be visually distinct, got: +=%q ~=%q -=%q", add, mod, del)
	}

	// + is mint (additive = brand color)
	if !strings.Contains(add, "111;226;166") {
		t.Errorf("plus() should use mint-400, got: %q", add)
	}

	// ~ is amber (modify = warm contrast)
	if !strings.Contains(mod, "255;194;71") {
		t.Errorf("tilde() should use amber, got: %q", mod)
	}

	// - is rose-dim (remove = muted red, not alarming)
	if !strings.Contains(del, "180;75;75") {
		t.Errorf("minusPruned() should use rose-dim, got: %q", del)
	}
}
