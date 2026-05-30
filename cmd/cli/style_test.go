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

// TestCheckTiers pins the three-tier ✓ vocabulary the apply flow speaks:
// the preliminary remote-state probe recedes to gray (checkChecking), the
// central deploy narrative is terminal-default "white" (checkFlow, no ANSI
// escape), and the final "apply complete" terminus is mint-400 (checkFinal).
// The three must read distinctly so an operator scanning a long apply can
// tell setup from story from done at a glance. check() (the generic command
// ✓ used outside the apply flow) stays mint and is distinct from checkFlow.
func TestCheckTiers(t *testing.T) {
	noColor = false

	checking := checkChecking()
	flow := checkFlow()
	final := checkFinal()
	generic := check()

	// checking phase → gray #949494.
	if !strings.Contains(checking, "148;148;148") {
		t.Errorf("checkChecking() should use gray #949494, got: %q", checking)
	}

	// central deploy flow → terminal default fg, no ANSI escape at all.
	if strings.Contains(flow, "\x1b") {
		t.Errorf("checkFlow() should be default-fg (no ANSI escape), got: %q", flow)
	}

	// terminus → mint-400 #6FE2A6.
	if !strings.Contains(final, "111;226;166") {
		t.Errorf("checkFinal() should use mint-400 #6FE2A6, got: %q", final)
	}

	// generic command ✓ → mint-400, and distinct from the white central-flow ✓.
	if !strings.Contains(generic, "111;226;166") {
		t.Errorf("check() should use mint-400 #6FE2A6, got: %q", generic)
	}

	// The three apply-flow tiers must be visually distinct from each other.
	if checking == flow || flow == final || checking == final {
		t.Errorf("the three ✓ tiers must be distinct: checking=%q flow=%q final=%q", checking, flow, final)
	}
}

// TestStepTierRouting pins which committed step lines recede to gray vs.
// render white: only the "checking …" probe is gray; every other step
// (the central deploy flow) is default-fg white.
func TestStepTierRouting(t *testing.T) {
	noColor = false

	cases := []struct {
		label    string
		checking bool
	}{
		{"checking remote state...", true},
		{"Checking remote state", true}, // legacy capital-C
		{"packing controller", false},
		{"streaming over ssh — fsw-controller", false},
		{"extracting release 5b4cb322d427", false},
		{"building release", false},
	}

	for _, tc := range cases {
		if got := isCheckingLabel(tc.label); got != tc.checking {
			t.Errorf("isCheckingLabel(%q) = %v, want %v", tc.label, got, tc.checking)
		}

		glyph := stepGlyph(tc.label)

		if tc.checking {
			if !strings.Contains(glyph, "148;148;148") {
				t.Errorf("checking glyph should be gray, got: %q", glyph)
			}

			if lbl := stepLabel(tc.label); !strings.Contains(lbl, "148;148;148") {
				t.Errorf("checking label should be fully gray, got: %q", lbl)
			}
		} else if strings.Contains(glyph, "\x1b") {
			t.Errorf("central-flow glyph should be default-fg (no ANSI), got: %q", glyph)
		}
	}
}

// TestSplitLabelDetail pins the action/description boundary: everything
// from the first " — " or " (" onward is the description tail, and the
// earliest separator wins. Drives paintLabel's gray-the-tail behavior.
func TestSplitLabelDetail(t *testing.T) {
	cases := []struct {
		label, action, detail string
	}{
		{"building release", "building release", ""},
		{"streaming over ssh — soft-web", "streaming over ssh", " — soft-web"},
		{"packing . (procfile → scope soft)", "packing .", " (procfile → scope soft)"},
		{"deployment/soft/web", "deployment/soft/web", ""},
		// Earliest separator wins: " (" at index 1 beats " — " later.
		{"a (b) — c", "a", " (b) — c"},
	}

	for _, tc := range cases {
		action, detail := splitLabelDetail(tc.label)
		if action != tc.action || detail != tc.detail {
			t.Errorf("splitLabelDetail(%q) = (%q,%q), want (%q,%q)",
				tc.label, action, detail, tc.action, tc.detail)
		}
	}
}

// TestPaintLabel pins the color split: the action head stays plain
// (terminal default fg), the description tail is wrapped gray. With
// NO_COLOR it's a lossless round-trip (split then rejoin, no bytes
// added or dropped).
func TestPaintLabel(t *testing.T) {
	t.Run("color: action plain, detail gray", func(t *testing.T) {
		noColor = false

		got := paintLabel("streaming over ssh — soft-web")

		// Action is not wrapped — the line starts with the bare verb.
		if !strings.HasPrefix(got, "streaming over ssh") {
			t.Errorf("action head should be plain, got: %q", got)
		}

		// Detail is grayed.
		if !strings.Contains(got, "148;148;148") {
			t.Errorf("detail tail should be gray, got: %q", got)
		}
	})

	t.Run("no detail: no color", func(t *testing.T) {
		noColor = false

		if got := paintLabel("building release"); got != "building release" {
			t.Errorf("label without a detail tail must be untouched, got: %q", got)
		}
	})

	t.Run("NO_COLOR: lossless round-trip", func(t *testing.T) {
		noColor = true
		defer func() { noColor = false }()

		for _, label := range []string{
			"streaming over ssh — soft-web",
			"packing . (procfile → scope soft)",
			"building release",
		} {
			if got := paintLabel(label); got != label {
				t.Errorf("NO_COLOR paintLabel(%q) = %q, want round-trip", label, got)
			}
		}
	})
}

// TestDescText pins the description gray + NO_COLOR strip.
func TestDescText(t *testing.T) {
	noColor = false

	if got := descText("(0s)"); !strings.Contains(got, "148;148;148") || !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("descText should wrap in gray + reset, got: %q", got)
	}

	noColor = true
	defer func() { noColor = false }()

	if got := descText("(0s)"); got != "(0s)" {
		t.Errorf("NO_COLOR descText should be bare, got: %q", got)
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
