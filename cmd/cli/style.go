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

import "os"

// Brand palette — values mirror brand_v1/spec.json. Aurora (slit) is
// reserved per the brand kit's "active states only" rule: voodu uses
// it ONLY on the final success line (✓ apply complete) and on the
// terminal "everything healthy" summary. Every other ✓ along the way
// is mint-400.
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

	// Aurora — eye-slit signal color. Reserved for the final
	// success terminus per brand kit page 6.
	cAurora = "\x1b[38;2;199;245;221m" // #C7F5DD

	// Status colors for resource diffs (kind/scope/name + key=value).
	// Amber for modify (~) and rose for remove (-) give each diff
	// class a distinct visual weight without leaving the brand
	// palette feel. Add (+) stays mint-400 so the eye reads "green =
	// new" instinctively.
	cAmber   = "\x1b[38;2;255;194;71m"  // #FFC247 — modify (~)
	cRose    = "\x1b[38;2;255;107;107m" // #FF6B6B — error / fail (✗)
	cRoseDim = "\x1b[38;2;180;75;75m"   // dimmer rose — remove / pruned (-)
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

func arrow() string         { return colorize(cMint400, symArrow) }
func check() string         { return colorize(cMint400, symCheck) }
func checkFinal() string    { return colorize(cAurora, symCheck) }
func cross() string         { return colorize(cRose, symCross) }
func plus() string          { return colorize(cMint400, symPlus) }
func tilde() string         { return colorize(cAmber, symTilde) }
func minusPruned() string   { return colorize(cRoseDim, symMinus) }
func warn() string          { return colorize(cAmber, symWarn) }

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

// auroraText wraps a string in aurora — terminus lines only.
func auroraText(s string) string {
	return colorize(cAurora, s)
}

// colorize is the inner primitive. NO_COLOR strips escapes; otherwise
// wraps with the supplied color + reset.
func colorize(c, s string) string {
	if noColor {
		return s
	}
	return c + s + ansiReset
}
