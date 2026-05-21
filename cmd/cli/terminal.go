// terminal.go — capability detection for the CLI's renderer.
//
// The renderer asks one question at startup:
//
//   - Is stdout a TTY?  → controls whether we animate at all.
//     Answered by writerIsTerminal (lives in stream_filter.go, shared
//     with the eventRenderer).
//
// We previously also detected inline-image support (iTerm2/kitty) to
// emit a GIF spinner. Removed — multi-row inline images stacked
// vertically on every tick because single-line `\r\x1b[2K` overprint
// can't clear a 2-row image. Braille spinner is the universal answer.
//
// Detection is env-based, not protocol-probe based. Probing would mean
// emitting an OSC sequence and waiting for a response — adds round-
// trip latency to every `voodu apply` and breaks under tmux/screen
// passthrough. Env vars are deterministic and zero-cost.

package main

import (
	"os"
	"strings"
)

// supportsTruecolor returns whether the host terminal claims 24-bit
// color support. We honour the standard $COLORTERM=truecolor signal
// when present; default to true otherwise — the renderer falls back
// gracefully on terminals that don't support it (escape codes are
// reinterpreted as the nearest indexed color).
//
// Currently unused — style.go emits truecolor unconditionally, with
// NO_COLOR as the only off-switch. Reserved here for the rare future
// case of needing to opt-out (e.g. on a 16-color SSH terminal where
// truecolor degradation produces unreadable greys).
func supportsTruecolor() bool {
	// NO_COLOR wins — even on a terminal that advertises truecolor,
	// the user explicitly said "no color, please." Checking this
	// first means COLORTERM=truecolor can't override the operator's
	// preference.
	if os.Getenv("NO_COLOR") != "" {
		return false
	}

	switch strings.ToLower(os.Getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return true
	}

	return true
}
