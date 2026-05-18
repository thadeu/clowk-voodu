// Tests for `vd stats`. Three layers:
//
//   - parseStatsRef: pure parsing, no HTTP, exhaustive cases for
//     every supported ref shape (bare scope, scope/name, kind/
//     scope/name, single-segment-as-kind, etc.)
//   - HTTP roundtrip: stub /stats and verify the CLI hits the right
//     URL with the right query params for each filter source
//     (positional ref AND explicit flags should map to the same
//     wire shape).
//   - Text rendering: every formatBytes / formatPercent /
//     limit-echo case the table relies on.

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

// TestParseStatsRef covers the disambiguation table — single
// segments that match knownKinds become --kind, others become
// --scope; multi-segment refs split deterministically.
func TestParseStatsRef(t *testing.T) {
	cases := []struct {
		ref       string
		wantKind  string
		wantScope string
		wantName  string
		wantErr   bool
	}{
		{"", "", "", "", false},

		// Single segment — knownKinds vs scope.
		{"deployment", "deployment", "", "", false},
		{"statefulset", "statefulset", "", "", false},
		{"job", "job", "", "", false},
		{"cronjob", "cronjob", "", "", false},
		{"ingress", "ingress", "", "", false},
		{"clowk-lp", "", "clowk-lp", "", false}, // not a known kind → scope

		// Two segments — always scope/name.
		{"clowk-lp/web", "", "clowk-lp", "web", false},
		{"data/pg", "", "data", "pg", false},

		// Three segments — kind/scope/name.
		{"deployment/clowk-lp/web", "deployment", "clowk-lp", "web", false},
		{"statefulset/data/pg", "statefulset", "data", "pg", false},

		// Three segments where the first isn't a known kind → error.
		{"unknown/scope/name", "", "", "", true},

		// Malformed.
		{"a//b", "", "", "", true},
		{"/leading", "", "", "", true},
		{"trailing/", "", "", "", true},
		{"a/b/c/d", "", "", "", true},
	}

	for _, c := range cases {
		t.Run(c.ref, func(t *testing.T) {
			k, s, n, err := parseStatsRef(c.ref)

			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got kind=%q scope=%q name=%q", k, s, n)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if k != c.wantKind || s != c.wantScope || n != c.wantName {
				t.Errorf("got kind=%q scope=%q name=%q; want kind=%q scope=%q name=%q",
					k, s, n, c.wantKind, c.wantScope, c.wantName)
			}
		})
	}
}

// TestStatsCmd_HitsControllerWithFilters drives the CLI end-to-end
// against an httptest server and asserts the outgoing query
// string. Both positional refs and explicit flags must land on the
// same `kind=&scope=&name=` shape.
func TestStatsCmd_HitsControllerWithFilters(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantQuery string
	}{
		{"no filter", []string{"stats"}, ""},
		{"positional scope", []string{"stats", "clowk-lp"}, "scope=clowk-lp"},
		{"positional scope/name", []string{"stats", "clowk-lp/web"}, "name=web&scope=clowk-lp"},
		{"positional kind", []string{"stats", "deployment"}, "kind=deployment"},
		{"positional kind/scope/name", []string{"stats", "deployment/clowk-lp/web"}, "kind=deployment&name=web&scope=clowk-lp"},
		{"flag form", []string{"stats", "-k", "deployment", "-s", "clowk-lp"}, "kind=deployment&scope=clowk-lp"},
		{"orphans flag", []string{"stats", "--orphans"}, "orphans=true"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotQuery string

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery

				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "ok",
					"data":   map[string]any{"pods": []controller.PodStats{}},
				})
			}))

			defer ts.Close()

			root := newRootCmd()
			_ = root.PersistentFlags().Set("controller-url", ts.URL)

			var buf bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&buf)
			root.SetArgs(c.args)

			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}

			if gotQuery != c.wantQuery {
				t.Errorf("query: got %q, want %q", gotQuery, c.wantQuery)
			}
		})
	}
}

// TestStatsCmd_RejectsRefPlusFlags pins the mutex: positional ref
// and --kind/--scope/--name are alternative spellings of the same
// filter, accepting both would let operators write contradictory
// instructions. Reject up front with a clear error.
func TestStatsCmd_RejectsRefPlusFlags(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"stats", "clowk-lp", "--kind", "deployment"})

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when mixing positional + flags")
	}

	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("expected 'not both' in error: %v", err)
	}
}

// TestStatsCmd_RendersJSON exercises the -o json passthrough.
// JSON output skips renderStatsTable and writes directly to
// stdout — capture via os.Pipe so we can assert the envelope
// content without coupling the test to renderStatsTable.
func TestStatsCmd_RendersJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"pods": []controller.PodStats{
					{
						Identity:      controller.StatsIdentity{Kind: "deployment", Scope: "x", Name: "web", ReplicaID: "a3f9"},
						ContainerName: "voodu-x-web.a3f9",
						Usage:         controller.UsageStats{CPUPercent: 12.5, MemoryUsageBytes: 100_000_000, MemoryPercent: 5.0},
						Limits:        controller.LimitStats{CPU: "0.4", Memory: "254Mi", MemoryBytes: 266338304},
					},
				},
			},
		})
	}))

	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)
	_ = root.PersistentFlags().Set("output", "json")

	got := captureStdout(t, func() {
		root.SetArgs([]string{"stats"})
		if err := root.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
	})

	if !strings.Contains(got, `"cpu_percent": 12.5`) {
		t.Errorf("expected cpu_percent in JSON output, got:\n%s", got)
	}

	if !strings.Contains(got, `"memory": "254Mi"`) {
		t.Errorf("expected memory limit in JSON output, got:\n%s", got)
	}
}

// captureStdout lives in forward_test.go — reused across CLI tests.

// TestRenderStatsTable covers the text-mode columns: header
// labels, value formatting per column, empty result, orphan row
// decoration.
func TestRenderStatsTable_HappyPath(t *testing.T) {
	pods := []controller.PodStats{
		{
			Identity:      controller.StatsIdentity{Kind: "deployment", Scope: "clowk-lp", Name: "web", ReplicaID: "a3f9"},
			ContainerName: "voodu-clowk-lp-web.a3f9",
			Usage: controller.UsageStats{
				CPUPercent:       47.5,
				MemoryUsageBytes: 120 * 1024 * 1024,
				MemoryPercent:    47.2,
			},
			Limits: controller.LimitStats{CPU: "0.4", Memory: "254Mi"},
		},
	}

	var buf bytes.Buffer
	if err := renderStatsTable(&buf, pods); err != nil {
		t.Fatal(err)
	}

	out := buf.String()

	for _, want := range []string{
		"KIND", "REF", "CPU%", "MEM USED", "MEM LIMIT", "MEM%", "CPU LIMIT",
		"deployment", "clowk-lp/web.a3f9", "47.5%", "120.0MiB", "254Mi", "47.2%", "0.4",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestRenderStatsTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := renderStatsTable(&buf, nil); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "No running pods matched") {
		t.Errorf("expected empty-result message, got:\n%s", buf.String())
	}
}

// TestRenderStatsTable_OrphanDecoration pins the "(orphan)" label
// the renderer appends to the KIND column so operators scan
// orphans visually. Two cases: pure orphan with no kind, and a
// kind-bearing pod whose manifest disappeared.
func TestRenderStatsTable_OrphanDecoration(t *testing.T) {
	pods := []controller.PodStats{
		// Pure orphan: no kind, no scope.
		{
			ContainerName: "voodu-legacy",
			Usage:         controller.UsageStats{CPUPercent: 1, MemoryUsageBytes: 1024},
			Orphan:        true,
		},
		// Half-orphan: had identity but manifest is gone.
		{
			Identity:      controller.StatsIdentity{Kind: "deployment", Scope: "ghost", Name: "leaked"},
			ContainerName: "voodu-ghost-leaked.1",
			Usage:         controller.UsageStats{CPUPercent: 1, MemoryUsageBytes: 1024},
			Orphan:        true,
		},
	}

	var buf bytes.Buffer
	_ = renderStatsTable(&buf, pods)

	out := buf.String()

	if !strings.Contains(out, "(orphan)") {
		t.Error("orphan marker missing")
	}

	if !strings.Contains(out, "deployment (orphan)") {
		t.Error("half-orphan should show kind + (orphan)")
	}
}

// TestFormatBytes pins the unit ladder — KiB / MiB / GiB choice
// at the boundary values plus the "—" sentinel for zero.
func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "—"},
		{1, "1B"},
		{1023, "1023B"},
		{1024, "1.0KiB"},
		{1024 * 1024, "1.0MiB"},
		{120 * 1024 * 1024, "120.0MiB"},
		{1024 * 1024 * 1024, "1.0GiB"},
		{2 * 1024 * 1024 * 1024, "2.0GiB"},
	}

	for _, c := range cases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatPercent(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "—"},
		{0.14, "0.1%"},
		{47.5, "47.5%"},
		{100, "100.0%"},
	}

	for _, c := range cases {
		if got := formatPercent(c.in); got != c.want {
			t.Errorf("formatPercent(%v): got %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFormatStatsRef covers the visible-reference shape — pulls
// scope/name.replica from identity, falls back to container name
// when identity is bare.
func TestFormatStatsRef(t *testing.T) {
	cases := []struct {
		in   controller.PodStats
		want string
	}{
		{
			controller.PodStats{
				Identity:      controller.StatsIdentity{Scope: "clowk-lp", Name: "web", ReplicaID: "a3f9"},
				ContainerName: "voodu-clowk-lp-web.a3f9",
			},
			"clowk-lp/web.a3f9",
		},
		{
			controller.PodStats{
				Identity:      controller.StatsIdentity{Name: "redis"},
				ContainerName: "voodu-redis",
			},
			"redis",
		},
		{
			controller.PodStats{
				ContainerName: "voodu-legacy",
			},
			"voodu-legacy",
		},
	}

	for _, c := range cases {
		if got := formatStatsRef(c.in); got != c.want {
			t.Errorf("formatStatsRef: got %q, want %q (in=%+v)", got, c.want, c.in)
		}
	}
}
