package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDiffSpecUnchanged(t *testing.T) {
	a := json.RawMessage(`{"replicas":2,"image":"nginx:1"}`)
	b := json.RawMessage(`{"image":"nginx:1","replicas":2}`)

	// Key order differs — must still report no changes.
	if changes := diffSpec(a, b); len(changes) != 0 {
		t.Errorf("expected no changes for key-reordered equal specs, got %+v", changes)
	}
}

func TestDiffSpecTopLevelModify(t *testing.T) {
	local := json.RawMessage(`{"replicas":3,"image":"nginx:1.27"}`)
	remote := json.RawMessage(`{"replicas":2,"image":"nginx:1.26"}`)

	changes := diffSpec(local, remote)

	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %d: %+v", len(changes), changes)
	}

	// Sorted by path: "image" before "replicas".
	if changes[0].Path != "image" || changes[0].Op != '~' {
		t.Errorf("changes[0] = %+v, want image~", changes[0])
	}

	if changes[1].Path != "replicas" || changes[1].Op != '~' {
		t.Errorf("changes[1] = %+v, want replicas~", changes[1])
	}
}

func TestDiffSpecNestedModifyOnly(t *testing.T) {
	// Only tls.email changed — the walker must descend into tls
	// rather than printing the whole block as one `~ tls` line.
	local := json.RawMessage(`{"host":"a","tls":{"email":"new@x","enabled":true,"provider":"letsencrypt"}}`)
	remote := json.RawMessage(`{"host":"a","tls":{"email":"old@x","enabled":true,"provider":"letsencrypt"}}`)

	changes := diffSpec(local, remote)

	if len(changes) != 1 {
		t.Fatalf("expected 1 change (tls.email), got %d: %+v", len(changes), changes)
	}

	if changes[0].Path != "tls.email" {
		t.Errorf("path = %q, want tls.email", changes[0].Path)
	}

	if changes[0].Op != '~' {
		t.Errorf("op = %c, want ~", changes[0].Op)
	}
}

func TestDiffSpecAddField(t *testing.T) {
	local := json.RawMessage(`{"replicas":2,"lang":{"name":"bun"}}`)
	remote := json.RawMessage(`{"replicas":2}`)

	changes := diffSpec(local, remote)

	// Expect a single `+ lang.name` — adding an object whose remote
	// counterpart doesn't exist should still descend, so the renderer
	// gets meaningful paths rather than a blob.
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d: %+v", len(changes), changes)
	}

	if changes[0].Op != '+' || changes[0].Path != "lang.name" {
		t.Errorf("got %+v, want +/lang.name", changes[0])
	}

	if changes[0].New != "bun" {
		t.Errorf("New = %v, want bun", changes[0].New)
	}
}

func TestDiffSpecRemoveField(t *testing.T) {
	local := json.RawMessage(`{"replicas":2}`)
	remote := json.RawMessage(`{"replicas":2,"old_flag":"x"}`)

	changes := diffSpec(local, remote)

	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d: %+v", len(changes), changes)
	}

	if changes[0].Op != '-' || changes[0].Path != "old_flag" {
		t.Errorf("got %+v, want -/old_flag", changes[0])
	}
}

func TestDiffSpecNilSides(t *testing.T) {
	// nil-remote case: everything in local is an addition. This is
	// how `+ new resource` uses the same walker.
	changes := diffSpec(json.RawMessage(`{"replicas":2}`), nil)

	if len(changes) != 1 || changes[0].Op != '+' || changes[0].Path != "replicas" {
		t.Errorf("local-only diff wrong: %+v", changes)
	}

	// nil-local case: everything in remote is a removal. Not used by
	// runDiff directly but keeps the walker symmetric for callers
	// that might want to render pruned-resource specs later.
	changes = diffSpec(nil, json.RawMessage(`{"replicas":2}`))

	if len(changes) != 1 || changes[0].Op != '-' || changes[0].Path != "replicas" {
		t.Errorf("remote-only diff wrong: %+v", changes)
	}
}

func TestDiffSpecArrayAtomic(t *testing.T) {
	// Arrays compare as atomic values — a whole-list replacement is
	// what users usually do, and element-wise diffing adds noise to
	// the common case. Different arrays = one `~` line.
	local := json.RawMessage(`{"ports":["8080","8081"]}`)
	remote := json.RawMessage(`{"ports":["8080"]}`)

	changes := diffSpec(local, remote)

	if len(changes) != 1 || changes[0].Path != "ports" || changes[0].Op != '~' {
		t.Errorf("arrays should diff atomically at their path, got %+v", changes)
	}
}

func TestDiffSpecTypeMismatch(t *testing.T) {
	// String → number: treat as opaque modification, don't try to be
	// clever. Rare in practice but mustn't crash.
	local := json.RawMessage(`{"ttl":30}`)
	remote := json.RawMessage(`{"ttl":"30s"}`)

	changes := diffSpec(local, remote)

	if len(changes) != 1 || changes[0].Op != '~' {
		t.Errorf("type change must diff as ~, got %+v", changes)
	}
}

func TestRenderResourceDiffOutput(t *testing.T) {
	// Lock in the line format — four-space indent, op marker, path,
	// padding to align values. The exact shape is part of the CLI
	// contract and what gets embedded in docs. bytes.Buffer isn't a
	// tty, so newDiffPalette returns no-color — the expected strings
	// are plain ASCII.
	changes := []fieldChange{
		{Path: "path", Op: '~', Old: ".", New: "../"},
		{Path: "replicas", Op: '~', Old: float64(2), New: float64(1)},
		{Path: "lang.name", Op: '+', New: "bun"},
	}

	var buf bytes.Buffer

	renderResourceDiff(&buf, changes, newDiffPalette(&buf))

	got := buf.String()

	wantLines := []string{
		`    ~ path       "."  →  "../"`,
		`    ~ replicas   2  →  1`,
		`    + lang.name  "bun"`,
	}

	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Errorf("missing line %q in output:\n%s", line, got)
		}
	}
}

// TestRenderResourceDiffCompactOutput pins the compact (one-line)
// rendering shape used by `voodu apply` previews. Format:
//
//	+ kind/scope/name   field1=value1 field2=value2
//
// Asserts: header is present, fields appear in `key=value` form,
// each separated by spaces. No `old → new` arrows (that's the
// detailed mode's job).
func TestRenderResourceDiffCompactOutput(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	changes := []fieldChange{
		{Path: "image", Op: '+', New: "ghcr.io/myorg/api:1.7"},
		{Path: "replicas", Op: '+', New: float64(3)},
	}

	var buf bytes.Buffer

	renderResourceDiffCompact(&buf, "deployment/prod/api", changes, newDiffPalette(&buf), '+')

	got := buf.String()

	for _, want := range []string{
		"+",
		"deployment/prod/api",
		"image=ghcr.io/myorg/api:1.7",
		"replicas=3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing fragment %q in output:\n%q", want, got)
		}
	}

	// Compact mode MUST NOT use the detailed `→` arrow shape — that's
	// reserved for renderResourceDiff (--verbose mode).
	if strings.Contains(got, "→") {
		t.Errorf("compact mode leaked detailed-mode arrow:\n%q", got)
	}
}

// TestRenderResourceDiffCompactCapsBody pins the 80-col cap behavior.
// A resource with many changed fields should surface the first N
// fitting fields and drop the rest with a "(+N more)" tail rather
// than wrap or overflow.
func TestRenderResourceDiffCompactCapsBody(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	// 12 changes × ~15 chars each = ~180 chars body. Way past 80.
	changes := make([]fieldChange, 0, 12)
	for i := 0; i < 12; i++ {
		changes = append(changes, fieldChange{
			Path: "field_" + string(rune('a'+i)),
			Op:   '~',
			New:  "value_" + string(rune('a'+i)),
		})
	}

	var buf bytes.Buffer

	renderResourceDiffCompact(&buf, "deployment/prod/api", changes, newDiffPalette(&buf), '~')

	got := buf.String()

	if !strings.Contains(got, "more)") {
		t.Errorf("expected '(+N more)' tail when fields exceed cap, got:\n%q", got)
	}
}

// TestJoinCapped pins the helper used by compact rendering. Cap edge
// cases (empty input, exactly-at-cap, just-over) drive most of the
// rendering UX, so they're worth their own pin.
func TestJoinCapped(t *testing.T) {
	cases := []struct {
		name    string
		parts   []string
		cap     int
		wantSub string
		notSub  string
	}{
		{
			name:    "empty",
			parts:   []string{},
			cap:     80,
			wantSub: "",
		},
		{
			name:    "fits under cap",
			parts:   []string{"a=1", "b=2"},
			cap:     80,
			wantSub: "a=1 b=2",
			notSub:  "more",
		},
		{
			name:    "exceeds cap → truncation tail",
			parts:   []string{"first=long_value_here", "second=another_long_value", "third=yet_another"},
			cap:     20,
			wantSub: "more)",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := joinCapped(c.parts, c.cap)

			if c.wantSub != "" && !strings.Contains(got, c.wantSub) {
				t.Errorf("want substring %q in %q", c.wantSub, got)
			}

			if c.notSub != "" && strings.Contains(got, c.notSub) {
				t.Errorf("unwanted substring %q in %q", c.notSub, got)
			}
		})
	}
}

// TestCompactValue pins value formatting for compact diffs. String
// truncation matters most — long image tags / URLs would blow out the
// 80-col cap if not clipped.
func TestCompactValue(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "(unset)"},
		{"short string", "hello", "hello"},
		{"bool", true, "true"},
		{"number", float64(3), "3"},
		{"map collapses", map[string]any{"k": "v"}, "{...}"},
		{"list collapses", []any{1, 2}, "[...]"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := compactValue(c.in); got != c.want {
				t.Errorf("compactValue(%#v) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	// Long string truncation — pinned separately because we want to
	// check the tail-ellipsis marker, not just the length.
	long := strings.Repeat("x", 100)

	got := compactValue(long)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("long string should be ellipsis-truncated, got: %q", got)
	}

	if len(got) > 60 {
		t.Errorf("truncated value over cap: len=%d", len(got))
	}
}

// TestRenderResourceDiffColor pins the ANSI wrapping behavior: with
// FORCE_COLOR on, the op markers must carry escape sequences emitted
// by lipgloss/termenv. Exact wrapping is the library's choice (e.g.
// `\x1b[32m+\x1b[0m`), so we assert on escape presence rather than a
// specific byte sequence — refactors inside lipgloss shouldn't
// break this test.
func TestRenderResourceDiffColor(t *testing.T) {
	t.Setenv("FORCE_COLOR", "1")
	t.Setenv("NO_COLOR", "")

	changes := []fieldChange{
		{Path: "image", Op: '~', Old: "a", New: "b"},
		{Path: "extra", Op: '+', New: "c"},
		{Path: "old", Op: '-', Old: "d"},
	}

	var buf bytes.Buffer

	renderResourceDiff(&buf, changes, newDiffPalette(&buf))

	got := buf.String()

	// The palette must have inserted ANSI escapes around each op.
	// lipgloss emits `\x1b[...m<str>\x1b[0m`, so every op marker
	// should be adjacent to an ESC byte.
	for _, frag := range []string{"\x1b[", "\x1b[0m"} {
		if !strings.Contains(got, frag) {
			t.Errorf("missing ANSI fragment %q in output:\n%q", frag, got)
		}
	}

	// Sanity check: exactly three colored markers (one per op).
	if n := strings.Count(got, "\x1b[0m"); n != 3 {
		t.Errorf("expected 3 ANSI resets (one per op), got %d: %q", n, got)
	}
}

// TestDiffPaletteNoColor covers the environment-override paths: when
// NO_COLOR is set OR the writer is not a tty, the palette's paint
// funcs must return the string verbatim so programmatic consumers
// (tests, pipes) see plain text. FORCE_COLOR loses to NO_COLOR per
// the no-color.org convention.
func TestDiffPaletteNoColor(t *testing.T) {
	var buf bytes.Buffer

	// Case 1: buffer (non-tty) + no env overrides → plain text.
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "")

	p := newDiffPalette(&buf)
	if got := p.Add("+"); got != "+" {
		t.Errorf("non-tty writer must yield plain text, got %q", got)
	}

	// Case 2: NO_COLOR set → plain text even with FORCE_COLOR.
	t.Setenv("NO_COLOR", "1")
	t.Setenv("FORCE_COLOR", "1")

	p = newDiffPalette(&buf)
	if got := p.Add("+"); got != "+" {
		t.Errorf("NO_COLOR must win over FORCE_COLOR, got %q", got)
	}
}

func TestDiffSummary(t *testing.T) {
	cases := []struct {
		added, modified, pruned int
		want                    string
	}{
		{0, 0, 0, "no changes"},
		{1, 0, 0, "1 to add"},
		{0, 2, 0, "2 to modify"},
		{0, 0, 3, "3 to prune"},
		{1, 2, 3, "1 to add, 2 to modify, 3 to prune"},
	}

	for _, c := range cases {
		if got := diffSummary(c.added, c.modified, c.pruned); got != c.want {
			t.Errorf("diffSummary(%d,%d,%d) = %q, want %q", c.added, c.modified, c.pruned, got, c.want)
		}
	}
}
