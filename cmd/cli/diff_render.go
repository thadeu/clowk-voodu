package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"go.voodu.clowk.in/internal/controller"
)

// diffPalette wraps three `func(string) string` colorizers — one per
// diff op. lipgloss (backed by termenv) handles tty detection,
// NO_COLOR, and color-profile downgrading for limited terminals. We
// keep a single callable shape so the renderer stays palette-agnostic.
type diffPalette struct {
	Add, Del, Mod func(strs ...string) string
}

// newDiffPalette builds the per-op colorizers. A custom
// lipgloss.Renderer pointed at the caller's writer ensures the
// profile is derived from THAT writer's fd, not from the global
// default renderer (which snapshots os.Stdout at package init —
// unreliable when called later from a cobra cmd).
//
// Color picks (brand-exact truecolor — same values as style.go):
//   - `+` add    → mint-400 #6FE2A6 (Cell, primary brand accent)
//   - `~` modify → amber   #FFC247 (warm contrast for "modify" weight)
//   - `-` remove → rose    #B44B4B (dimmed rose, less alarming than full red)
//
// lipgloss auto-degrades to the closest indexed color on terminals
// that lack truecolor support, so the brand intent survives even when
// the exact hex doesn't render.
//
// Override precedence (highest wins):
//  1. NO_COLOR set (non-empty)           → plain text always
//  2. FORCE_COLOR set (non-empty)        → force ANSI256 regardless of tty
//  3. lipgloss/termenv default detection → uses w's fd + TERM + CI hints
func newDiffPalette(w io.Writer) diffPalette {
	plain := func(strs ...string) string { return strings.Join(strs, " ") }

	// NO_COLOR short-circuit — per no-color.org, any non-empty value
	// disables color everywhere. Empty-string counts as unset so tests
	// can clear it via t.Setenv without accidentally tripping it.
	if v := os.Getenv("NO_COLOR"); v != "" {
		return diffPalette{Add: plain, Del: plain, Mod: plain}
	}

	r := lipgloss.NewRenderer(w)

	// FORCE_COLOR override: force ANSI256 so the palette emits even
	// when w isn't a tty (piping, redirection to a file, CI tail).
	// Without this, lipgloss would see a non-tty writer and pick
	// `Ascii` (no-color) profile, swallowing our colors.
	if v := os.Getenv("FORCE_COLOR"); v != "" {
		r.SetColorProfile(termenv.ANSI256)
	}

	// Brand palette — values match cMint400 / cAmber / cRoseDim in
	// style.go. lipgloss downgrades to nearest indexed color on
	// non-truecolor terminals.
	return diffPalette{
		Add: r.NewStyle().Foreground(lipgloss.Color("#6FE2A6")).Render,
		Del: r.NewStyle().Foreground(lipgloss.Color("#B44B4B")).Render,
		Mod: r.NewStyle().Foreground(lipgloss.Color("#FFC247")).Render,
	}
}

// fieldChange is one atomic unit of the spec diff: a single JSON path
// (dotted, e.g. "tls.email") with its before/after values and an op
// (~ modify, + add, - remove). Flat so the renderer can sort and align
// without recursing.
type fieldChange struct {
	Path string
	Op   byte
	Old  any
	New  any
}

// diffSpec compares two JSON specs (the blobs the controller stores
// under Manifest.Spec) and returns every leaf that differs, flattened
// to dotted paths. Nested objects are descended into so a change to
// `tls.email` surfaces as one line, not as "the whole tls block
// changed". Arrays are treated atomically — element-wise diffing of
// `ports = ["8080", "8081"]` would make the output noisier than helpful
// for the most common case (whole-list replacements).
//
// Either side can be nil: a nil local → every key in remote is `-`,
// a nil remote → every key in local is `+`. That's how `+ new resource`
// and `- pruned resource` reuse the same walker.
func diffSpec(local, remote json.RawMessage) []fieldChange {
	var (
		l any
		r any
	)

	// Empty RawMessage is treated as "no object" — same shape as a
	// missing field at the parent level. Explicit nil check before
	// unmarshal keeps `{}` vs. missing distinct at intermediate levels
	// but identical at the top (both mean "no fields").
	if len(local) > 0 {
		_ = json.Unmarshal(local, &l)
	}

	if len(remote) > 0 {
		_ = json.Unmarshal(remote, &r)
	}

	var out []fieldChange

	walkFields("", l, r, &out)

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })

	return out
}

// walkFields is the recursive core. Prefix carries the parent path so
// leaves report as "tls.email" rather than just "email". Both sides
// are expected to be the result of json.Unmarshal into `any`, so we
// get map[string]any / []any / primitives.
func walkFields(prefix string, local, remote any, out *[]fieldChange) {
	// Both nil = nothing to say.
	if local == nil && remote == nil {
		return
	}

	lMap, lIsMap := local.(map[string]any)
	rMap, rIsMap := remote.(map[string]any)

	// When at least one side is an object, descend per key union. A
	// nil counterpart is treated as an empty map so "new resource"
	// and "+ lang.name" land as dotted fields rather than one opaque
	// `+ lang = {...}` blob. Feels more like Terraform plan and
	// makes code review diffs actually legible.
	if lIsMap || rIsMap {
		if lMap == nil {
			lMap = map[string]any{}
		}

		if rMap == nil {
			rMap = map[string]any{}
		}

		for _, k := range unionKeys(lMap, rMap) {
			child := k

			if prefix != "" {
				child = prefix + "." + k
			}

			walkFields(child, lMap[k], rMap[k], out)
		}

		return
	}

	// Both sides are scalars/arrays (or mismatched non-object types).
	// Compare as opaque values at the current prefix.
	if local == nil {
		*out = append(*out, fieldChange{Path: prefix, Op: '-', Old: remote})
		return
	}

	if remote == nil {
		*out = append(*out, fieldChange{Path: prefix, Op: '+', New: local})
		return
	}

	if reflect.DeepEqual(local, remote) {
		return
	}

	*out = append(*out, fieldChange{Path: prefix, Op: '~', Old: remote, New: local})
}

// unionKeys returns the sorted set of keys present in either map.
// Sorting at every level means the final sort in diffSpec gets stable
// input and the rendered output is deterministic — critical for CI
// diffs where a flapping ordering would trigger false alarms.
func unionKeys(a, b map[string]any) []string {
	seen := map[string]struct{}{}

	for k := range a {
		seen[k] = struct{}{}
	}

	for k := range b {
		seen[k] = struct{}{}
	}

	keys := make([]string, 0, len(seen))

	for k := range seen {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// formatValue renders a field value with just enough fidelity for a
// diff line: strings keep their quotes, numbers/bools are bare, nested
// objects/arrays collapse to compact JSON. Strings hold the most signal
// (URLs, image tags, hostnames) so getting them right matters more
// than pretty-printing maps the walker didn't descend into.
func formatValue(v any) string {
	if v == nil {
		return "(not set)"
	}

	switch x := v.(type) {
	case string:
		return fmt.Sprintf("%q", x)
	case bool, float64, int, int64:
		return fmt.Sprintf("%v", x)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}

		return string(b)
	}
}

// renderResourceDiff prints the field changes for one resource under
// an already-printed header (the `~ kind/scope/name` line). Each
// change is indented 4 spaces; longest-path column is left-padded so
// old/new values line up, which makes scanning several fields fast.
// The op marker (+/-/~) is colored per the palette so scanning a long
// diff is a matter of glancing at the left gutter.
//
// Example output (colors stripped):
//
//	    ~ path      "."  →  "../"
//	    ~ replicas  2  →  1
//	    + lang.name  "bun"
func renderResourceDiff(w io.Writer, changes []fieldChange, p diffPalette) {
	maxPath := 0

	for _, c := range changes {
		if len(c.Path) > maxPath {
			maxPath = len(c.Path)
		}
	}

	for _, c := range changes {
		pad := strings.Repeat(" ", maxPath-len(c.Path))

		switch c.Op {
		case '~':
			// Modify keeps the gutter marker orange but leaves the
			// body uncolored — the `old → new` shape already makes the
			// change obvious, and painting the whole line the same
			// shade as the arrow washes out the value delta.
			fmt.Fprintf(w, "    %s %s%s  %s  →  %s\n",
				p.Mod("~"), c.Path, pad, formatValue(c.Old), formatValue(c.New))
		case '+':
			// Whole-line green for adds. Additions are "one value" on
			// the line, so coloring the path + value together keeps the
			// eye moving in one direction and mirrors Terraform plan.
			line := fmt.Sprintf("+ %s%s  %s", c.Path, pad, formatValue(c.New))
			fmt.Fprintf(w, "    %s\n", p.Add(line))
		case '-':
			// Whole-line red for removes, same reasoning as adds.
			line := fmt.Sprintf("- %s%s  %s", c.Path, pad, formatValue(c.Old))
			fmt.Fprintf(w, "    %s\n", p.Del(line))
		}
	}
}

// renderResourceDiffCompact prints a one-line summary of the changes
// for one resource. Used by `voodu apply` (default) to keep the
// output narrative-style, matching the landing page mockup:
//
//   + deployment/prod/api  replicas=3 image=ghcr.io/myorg/api:1.7
//   ~ ingress/prod/api     tls.email=ops@example.com
//   + redis/clowk-lp/redis-ha sentinel.monitor=clowk-lp/redis
//
// The detailed field-by-field path (renderResourceDiff) is reserved
// for `voodu apply --verbose` and `voodu diff` (where field-level
// detail is the point of the command).
//
// Selection logic for which fields to surface in the compact line:
//   - For new resources (+): the "headline" declared fields the
//     operator would expect to see for that kind. Hard-coded per-kind
//     priority list with fall-through to alphabetical.
//   - For modified resources (~): the changed field paths, alphabetical.
//
// Cap on body width: ~80 columns. Surplus fields are dropped with a
// trailing `(+N more)` so the line still fits in a normal terminal
// and a glance is enough to spot the change class.
func renderResourceDiffCompact(w io.Writer, header string, changes []fieldChange, p diffPalette, op byte) {
	// Filter to the changes we want to surface on the compact line.
	// + and ~ both surface "New" value (current state being written).
	// - surfaces "Old" (what's going away).
	parts := make([]string, 0, len(changes))

	for _, c := range changes {
		switch op {
		case '+', '~':
			if c.Op == '-' {
				continue
			}

			val := c.New
			if val == nil {
				val = c.Old
			}

			parts = append(parts, fmt.Sprintf("%s=%s", c.Path, compactValue(val)))

		case '-':
			parts = append(parts, fmt.Sprintf("%s=%s", c.Path, compactValue(c.Old)))
		}
	}

	body := joinCapped(parts, 80)

	var prefix string

	switch op {
	case '+':
		prefix = p.Add("+")
	case '~':
		prefix = p.Mod("~")
	case '-':
		prefix = p.Del("-")
	default:
		prefix = string(op)
	}

	if body == "" {
		fmt.Fprintf(w, "%s %s\n", prefix, header)
	} else {
		fmt.Fprintf(w, "%s %s  %s\n", prefix, header, body)
	}
}

// compactValue is the inline-form of a value for compact diffs. Keeps
// strings unquoted (the key=value form is unambiguous) and clamps
// long strings so a 200-char image tag with SHA suffix doesn't blow
// out the line. Nested objects collapse to `{...}` — the operator who
// wants the full structure runs `voodu apply --verbose`.
func compactValue(v any) string {
	if v == nil {
		return "(unset)"
	}

	switch x := v.(type) {
	case string:
		if len(x) > 60 {
			return x[:57] + "..."
		}
		return x
	case bool, float64, int, int64:
		return fmt.Sprintf("%v", x)
	case map[string]any:
		return "{...}"
	case []any:
		return "[...]"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		s := string(b)
		if len(s) > 60 {
			return s[:57] + "..."
		}
		return s
	}
}

// joinCapped joins parts with single spaces, stopping when the
// running width exceeds cap. A `(+N more)` tail signals truncation
// so the operator knows there's more behind the curtain.
func joinCapped(parts []string, cap int) string {
	if len(parts) == 0 {
		return ""
	}

	var b strings.Builder

	width := 0

	for i, p := range parts {
		// +1 for the space between fields (except the first).
		need := len(p)
		if i > 0 {
			need++
		}

		if width+need > cap {
			remaining := len(parts) - i

			fmt.Fprintf(&b, " (+%d more)", remaining)

			return b.String()
		}

		if i > 0 {
			b.WriteByte(' ')
		}

		b.WriteString(p)
		width += need
	}

	return b.String()
}

// diffSummary produces the one-liner printed at the end of `voodu
// diff`. Shape mirrors terraform plan: "N to add, N to modify, N to
// prune". All-zeroes case collapses to "no changes" so glance-reading
// the end of the output is enough to know if a pipeline step should
// proceed.
func diffSummary(added, modified, pruned int) string {
	if added == 0 && modified == 0 && pruned == 0 {
		return "no changes"
	}

	var parts []string

	if added > 0 {
		parts = append(parts, fmt.Sprintf("%d to add", added))
	}

	if modified > 0 {
		parts = append(parts, fmt.Sprintf("%d to modify", modified))
	}

	if pruned > 0 {
		parts = append(parts, fmt.Sprintf("%d to prune", pruned))
	}

	return strings.Join(parts, ", ")
}

// diffResponse is the decoded shape of POST /apply?dry_run=true. Keeps
// the CLI decoupled from controller internals — if the server ever
// grows richer diff output, only this struct changes.
type diffResponse struct {
	Status string `json:"status"`
	Data   struct {
		Applied []*controller.Manifest `json:"applied"`
		Current []*controller.Manifest `json:"current"`
		Pruned  []string               `json:"pruned"`
		DryRun  bool                   `json:"dry_run"`
	} `json:"data"`
}
