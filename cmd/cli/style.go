// style.go is the single source of truth for the voodu CLI's visual
// vocabulary — palette, symbols, and the truecolor ANSI escape codes
// that render them. Every line the renderer emits to a TTY routes
// through one of these helpers.
//
// Why hand-rolled ANSI vs. a styling library:
//
//   The renderer is small (event_renderer + diff_render + spinner) and
//   the styling discipline is narrow (one palette, six symbols, three
//   levels of de-emphasis). Pulling in lipgloss / fatih-color would
//   add a transitive surface for ~40 lines of escape codes. Stay lean.
//
// Color model: truecolor (24-bit) — \x1b[38;2;R;G;Bm.
//
//   Targets macOS Terminal.app, iTerm2, Alacritty, kitty, wezterm,
//   VS Code terminal, modern xterm. All ship truecolor by default.
//   Older SSH terminals that fall back to 256-color degrade gracefully
//   to the nearest indexed value — the brand isn't pixel-perfect there
//   but the layout stays intact.
//
// NO_COLOR (https://no-color.org) is honoured: when set to any
// non-empty value, every coloured helper returns the bare string with
// the symbol intact but the escape sequences stripped. Pipe-to-file
// (non-TTY) also disables colour via the renderer's tty check before
// these helpers are reached, so NO_COLOR is a belt-and-braces signal
// for users who want colour off in an interactive shell.

package main

import (
	"os"
	"strings"
)

// Brand palette — values mirror brand_v1/spec.json. The apply flow
// speaks a three-tier ✓ vocabulary so an operator can scan setup vs.
// story vs. done at a glance:
//
//	checking phase (remote-state probe) → gray   (checkChecking) — recedes
//	central deploy flow (pack/stream/    → white  (checkFlow)     — the story
//	  extract/build/prune)                 (terminal default fg)
//	terminus (✓ apply complete)          → mint-400 (checkFinal)  — done
//
// "White" is the terminal's default foreground, not a hardcoded #FFFFFF:
// it reads white on a dark theme and stays legible on a light one — the
// same trick paintLabel uses for the action head. mint-400 stays the
// generic command ✓ (check(), used outside the apply flow). Aurora was
// the old terminus color; the terminus is mint now, so aurora is a
// reserved palette token.
const (
	// Truecolor escape prefix template — fill in via fmt.Sprintf if
	// you need ad-hoc colors; the named constants below pre-bake the
	// brand palette so the hot path doesn't allocate.
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"

	// Mint scale — see brand_v1/README.md "Color tokens".
	// mint-400 is the primary accent for symbols and in-progress states.
	cMint400 = "\x1b[38;2;111;226;166m" // #6FE2A6 Cell — primary
	cMint300 = "\x1b[38;2;131;234;180m" // #83EAB4 Spring — softer mint, currently unused but reserved
	cMint600 = "\x1b[38;2;61;190;133m"  // #3DBE85 Moss — darker variant, reserved

	// Aurora — eye-slit signal color (brand kit page 6). Formerly the
	// final-success terminus; the terminus is mint-400 now (operators
	// asked for a vivid green "done"), so aurora is a reserved palette
	// token like mint-300/600 above.
	cAurora = "\x1b[38;2;199;245;221m" // #C7F5DD — reserved

	// Status colors for resource diffs (kind/scope/name + key=value).
	// Amber for modify (~) and rose for remove (-) give each diff
	// class a distinct visual weight without leaving the brand
	// palette feel. Add (+) stays mint-400 so the eye reads "green =
	// new" instinctively.
	cAmber   = "\x1b[38;2;255;194;71m"  // #FFC247 — modify (~)
	cRose    = "\x1b[38;2;255;107;107m" // #FF6B6B — error / fail (✗)
	cRoseDim = "\x1b[38;2;180;75;75m"   // dimmer rose — remove / pruned (-)

	// Gray — secondary "description" text: a step's target/scope, the
	// parenthetical detail, the elapsed-time tail. Recedes behind the
	// action label so the eye lands on the verb first. A defined gray
	// (not ANSI dim) keeps the weight consistent across terminals.
	cGray = "\x1b[38;2;148;148;148m" // #949494
)

// Symbol vocabulary. Each maps to a visual class:
//
//	→ in-progress / step boundary
//	✓ completed (mint, or aurora at terminus)
//	✗ failed
//	+ resource added or created
//	~ resource modified
//	- resource removed / pruned
//	⚠ warning
const (
	symArrow = "→"
	symCheck = "✓"
	symCross = "✗"
	symPlus  = "+"
	symTilde = "~"
	symMinus = "-"
	symWarn  = "⚠"
)

// noColor is set once at init from $NO_COLOR. Reading the env in every
// styling call would cost a syscall per character — cache and respect
// the standard's "any non-empty value disables color" rule.
var noColor = os.Getenv("NO_COLOR") != ""

// Style helpers — every emitter goes through one of these. Each
// returns "symbol + space" so the call site reads as `out.Write(arrow() +
// "label\n")` rather than threading color codes by hand.

func arrow() string        { return colorize(cMint400, symArrow) }
func check() string        { return colorize(cMint400, symCheck) }
func checkApplied() string { return colorize(cAmber, symCheck) }
func cross() string        { return colorize(cRose, symCross) }
func plus() string         { return colorize(cMint400, symPlus) }
func tilde() string        { return colorize(cAmber, symTilde) }
func minusPruned() string  { return colorize(cRoseDim, symMinus) }
func warn() string         { return colorize(cAmber, symWarn) }

// The apply flow's three-tier ✓ vocabulary — see the palette comment
// above. checkChecking (gray) recedes the preliminary remote-state
// probe; checkFlow (terminal default fg, "white" on dark) carries the
// central deploy narrative; checkFinal (mint-400) marks the terminus.

// checkChecking is the gray ✓ for the preliminary remote-state probe —
// setup work that recedes behind the deploy narrative that follows.
func checkChecking() string { return colorize(cGray, symCheck) }

// checkFlow is the central-flow ✓ — packing, streaming, extracting,
// building, pruning. It carries NO color escape, so it renders in the
// terminal's default foreground: "white" on a dark theme, legible on a
// light one. Distinct from check()'s mint ✓ used outside the apply flow.
func checkFlow() string { return symCheck }

// checkFinal is the terminus ✓ — the "apply complete" line. Mint-400,
// the brand's vivid "done" green. (Was aurora; operators asked for a
// punchier terminus, so the pale aurora moved to a reserved token.)
func checkFinal() string { return colorize(cMint400, symCheck) }

// dim wraps text in ANSI dim (\x1b[2m). Used for elapsed-time tails
// like `(11.8s)` after step labels.
func dim(s string) string {
	if noColor {
		return s
	}
	return ansiDim + s + ansiReset
}

// mintText wraps a string in mint-400 — for "key=value" highlights on
// diff lines and the final URL line.
func mintText(s string) string {
	return colorize(cMint400, s)
}

// descText colors secondary "description" text gray — a step's target,
// scope, parenthetical detail, or elapsed-time tail. The color vocabulary
// the renderer speaks: success ✓ = mint (check), failure ✗ = rose (cross),
// the action label = terminal default (reads white on a dark theme without
// hardcoding a color that would vanish on a light one), description = this
// gray.
func descText(s string) string {
	return colorize(cGray, s)
}

// splitLabelDetail divides a step label into its action head and an
// optional description tail — everything from the first " — " or " ("
// onward (e.g. "streaming over ssh — soft-web" → "streaming over ssh" +
// " — soft-web"; "packing . (procfile → scope soft)" → "packing ." +
// " (procfile → scope soft)"). The tail recedes to gray; the head stays
// in the default foreground.
func splitLabelDetail(label string) (action, detail string) {
	idx := -1

	for _, sep := range []string{" — ", " ("} {
		if i := strings.Index(label, sep); i >= 0 && (idx < 0 || i < idx) {
			idx = i
		}
	}

	if idx < 0 {
		return label, ""
	}

	return label[:idx], label[idx:]
}

// paintLabel renders a step label with the action/description color
// split: action head in default fg, description tail in gray.
func paintLabel(label string) string {
	action, detail := splitLabelDetail(label)
	if detail == "" {
		return action
	}

	return action + descText(detail)
}

// isCheckingLabel reports whether a committed step label is the
// preliminary remote-state probe (the phase-1 diff spinner: "checking
// remote state…"). It's the one step that recedes to gray — every other
// step is part of the central deploy flow and renders in default fg.
// Case-insensitive so the legacy capital-C "Checking " banner matches too.
func isCheckingLabel(label string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(label)), "checking")
}

// stepGlyph picks the committed-step ✓ by tier: gray for the checking
// phase, terminal-default "white" for the central deploy flow. Failure
// (✗ rose) and the terminus (mint ✓) are rendered by their own helpers.
func stepGlyph(label string) string {
	if isCheckingLabel(label) {
		return checkChecking()
	}

	return checkFlow()
}

// stepLabel paints a committed step label by tier: the checking phase
// goes fully gray (the whole line recedes); the central flow keeps its
// action in default fg with the detail tail gray (paintLabel).
func stepLabel(label string) string {
	if isCheckingLabel(label) {
		return descText(label)
	}

	return paintLabel(label)
}

// colorize is the inner primitive. NO_COLOR strips escapes; otherwise
// wraps with the supplied color + reset.
func colorize(c, s string) string {
	if noColor {
		return s
	}
	return c + s + ansiReset
}
