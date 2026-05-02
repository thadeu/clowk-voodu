package main

import (
	"hash/fnv"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// podColorPalette is the closed set of ANSI 256-color codes the
// log fan-out cycles through to assign one consistent color per
// pod. Choices avoid red (1) and green (2) — those already mean
// "delete" and "add" in the diff palette and would carry false
// signal in log output. Bright cyan / yellow / magenta / blue /
// orange / purple read clearly on both light and dark terminals.
//
// Cycling through 6 colors means more than 6 pods in one stream
// will share colors. Acceptable: stern / kail do the same;
// operators almost never tail >6 pods at once, and the per-line
// `[name]` prefix still disambiguates if they do.
//
// Palette values reference lipgloss color slots (8-bit ANSI).
// lipgloss/termenv auto-downgrades to closest 16-color match on
// terminals that lack 256-color support.
var podColorPalette = []string{
	"51",  // bright cyan
	"226", // bright yellow
	"201", // bright magenta
	"33",  // sky blue
	"208", // orange
	"141", // light purple
}

// podPalette is the seam log streaming uses to colorize per-pod
// labels (`[name]` prefixes and `==> name <==` headers). Same
// shape as diffPalette — a single function the caller invokes
// without thinking about color profiles, NO_COLOR, etc.
//
// Color attribution: hash of the pod name → index into the
// palette. Same pod always gets the same color across runs, so
// operators learn `pod-a3f9 = cyan` after seeing it once and
// can scan multi-pod streams by color alone.
type podPalette struct {
	// ColorFor returns a colorizer function for the named pod.
	// Calling the returned func with a string wraps it in the
	// pod's assigned ANSI codes. Returns the identity function
	// (plain text) when colors are disabled.
	ColorFor func(podName string) func(s ...string) string
}

// newPodPalette wires the colorizer to the supplied writer's
// terminal profile. NO_COLOR turns everything off (per
// no-color.org); FORCE_COLOR forces ANSI256 even when the
// writer isn't a tty (piping, CI). lipgloss handles tty
// detection via the writer's fd when both env vars are unset.
//
// The returned ColorFor is safe to call concurrently — the
// underlying lipgloss styles are stateless after construction.
func newPodPalette(w io.Writer) podPalette {
	plain := func(s ...string) string { return strings.Join(s, " ") }

	// No-color short-circuit: every pod gets the identity
	// colorizer, callers don't need to branch on the env.
	if v := os.Getenv("NO_COLOR"); v != "" {
		return podPalette{
			ColorFor: func(string) func(...string) string { return plain },
		}
	}

	r := lipgloss.NewRenderer(w)

	if v := os.Getenv("FORCE_COLOR"); v != "" {
		r.SetColorProfile(termenv.ANSI256)
	}

	// Pre-build one style per palette slot — saves an allocation
	// per log line. Colorizers close over their slot so callers
	// don't need to thread the index around.
	colorizers := make([]func(...string) string, len(podColorPalette))
	for i, code := range podColorPalette {
		colorizers[i] = r.NewStyle().Foreground(lipgloss.Color(code)).Render
	}

	return podPalette{
		ColorFor: func(podName string) func(...string) string {
			return colorizers[podColorIndex(podName)]
		},
	}
}

// podColorIndex hashes a pod name to an index in podColorPalette.
// FNV-32a is plenty for this — uniform distribution across our
// 6 slots, ~zero collision sensitivity matters for log labels
// (worst case: two pods share a color, operator reads the name).
//
// Stable across runs (no random seed) so the SAME pod always
// gets the SAME color in the same operator's terminal — that's
// the muscle memory the feature is built for.
func podColorIndex(podName string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(podName))

	return int(h.Sum32() % uint32(len(podColorPalette)))
}
