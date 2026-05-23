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
// decoration. One row per replica — the replica suffix is in the
// NAME column.
func TestRenderStatsTable_HappyPath(t *testing.T) {
	pods := []controller.PodStats{
		{
			Identity:      controller.StatsIdentity{Kind: "deployment", Scope: "clowk-lp", Name: "web", ReplicaID: "a3f9"},
			ContainerName: "voodu-clowk-lp-web.a3f9",
			Usage: controller.UsageStats{
				CPUPercent:       4.5,
				MemoryUsageBytes: 128 * 1024 * 1024,
				MemoryPercent:    47.2,
			},
			Limits: controller.LimitStats{CPU: "0.4", Memory: "512Mi"},
		},
	}

	var buf bytes.Buffer
	if err := renderStatsTable(&buf, pods); err != nil {
		t.Fatal(err)
	}

	out := buf.String()

	for _, want := range []string{
		"KIND", "NAME", "CPU", "MEMORY",
		"deployment", "clowk-lp/web.a3f9", "0.05/0.4", "128Mi/512Mi",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\noutput:\n%s", want, out)
		}
	}

	for _, gone := range []string{"CPU%", "MEM USED", "MEM LIMIT", "MEM%", "CPU LIMIT", "REPLICAS"} {
		if strings.Contains(out, gone) {
			t.Errorf("unwanted column/label %q still present\noutput:\n%s", gone, out)
		}
	}
}

// TestRenderStatsTable_PerReplicaRows pins that two siblings of one
// resource render as two distinct rows — the whole point of going
// per-pod (so a leaky replica stands out against its siblings).
func TestRenderStatsTable_PerReplicaRows(t *testing.T) {
	pods := []controller.PodStats{
		{
			Identity:      controller.StatsIdentity{Kind: "deployment", Scope: "clowk-vd", Name: "docs", ReplicaID: "35a3"},
			ContainerName: "voodu-clowk-vd-docs.35a3",
			Usage:         controller.UsageStats{CPUPercent: 1.5, MemoryUsageBytes: 40 * 1024 * 1024},
			Limits:        controller.LimitStats{CPU: "0.5", Memory: "100Mi"},
		},
		{
			Identity:      controller.StatsIdentity{Kind: "deployment", Scope: "clowk-vd", Name: "docs", ReplicaID: "8f4c"},
			ContainerName: "voodu-clowk-vd-docs.8f4c",
			Usage:         controller.UsageStats{CPUPercent: 0.1, MemoryUsageBytes: 42 * 1024 * 1024},
			Limits:        controller.LimitStats{CPU: "0.5", Memory: "100Mi"},
		},
	}

	var buf bytes.Buffer
	if err := renderStatsTable(&buf, pods); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")

	if len(lines) != 3 {
		t.Fatalf("expected header + 2 per-replica rows, got %d lines:\n%s", len(lines), out)
	}

	if !strings.Contains(out, "clowk-vd/docs.35a3") || !strings.Contains(out, "clowk-vd/docs.8f4c") {
		t.Errorf("both replica refs should appear verbatim; got:\n%s", out)
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

// TestFormatMemoryShort pins the k8s-style unit ladder — integer
// rendering when the value divides cleanly into the unit, one
// decimal otherwise, "—" sentinel for zero.
func TestFormatMemoryShort(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "—"},
		{1, "1B"},
		{1023, "1023B"},
		{1024, "1Ki"},
		{1536, "1.5Ki"},
		{1024 * 1024, "1Mi"},
		{128 * 1024 * 1024, "128Mi"},
		{512 * 1024 * 1024, "512Mi"},
		{1024 * 1024 * 1024, "1Gi"},
		{1536 * 1024 * 1024, "1.5Gi"},
		{2 * 1024 * 1024 * 1024, "2Gi"},
	}

	for _, c := range cases {
		if got := formatMemoryShort(c.in); got != c.want {
			t.Errorf("formatMemoryShort(%d): got %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFormatMemoryCell pins the "used/limit" shape, including
// the dash on the right when no limit is declared.
func TestFormatMemoryCell(t *testing.T) {
	cases := []struct {
		used  uint64
		limit string
		want  string
	}{
		{128 * 1024 * 1024, "512Mi", "128Mi/512Mi"},
		{512 * 1024 * 1024, "1Gi", "512Mi/1Gi"},
		{64 * 1024 * 1024, "256Mi", "64Mi/256Mi"},
		{128 * 1024 * 1024, "", "128Mi/—"},
		{0, "512Mi", "—/512Mi"},
	}

	for _, c := range cases {
		if got := formatMemoryCell(c.used, c.limit); got != c.want {
			t.Errorf("formatMemoryCell(%d, %q): got %q, want %q", c.used, c.limit, got, c.want)
		}
	}
}

// TestFormatMilliCPU pins the percent-to-millicores conversion:
// 100% = 1000m, 4.5% = 45m, zero → "—", tiny non-zero rounds up.
func TestFormatMilliCPU(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "—"},
		{0.01, "1m"},
		{4.5, "45m"},
		{18, "180m"},
		{1.2, "12m"},
		{100, "1000m"},
	}

	for _, c := range cases {
		if got := formatMilliCPU(c.in); got != c.want {
			t.Errorf("formatMilliCPU(%v): got %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFormatCPUCell pins the unit pairing logic — cores limit
// renders cores used, milli limit renders milli used, empty limit
// falls back to plain millicores.
func TestFormatCPUCell(t *testing.T) {
	cases := []struct {
		pct   float64
		limit string
		want  string
	}{
		{45, "0.5", "0.45/0.5"},
		{60, "2", "0.6/2"},
		{20, "0.5", "0.2/0.5"},
		{45, "500m", "450m/500m"},
		{4.5, "100m", "45m/100m"},
		{0.1, "0.5", "0.001/0.5"},
		{4.5, "", "45m"},
		{0, "0.5", "0/0.5"},
		{0, "", "—"},
		{100, "1", "1/1"},
	}

	for _, c := range cases {
		if got := formatCPUCell(c.pct, c.limit); got != c.want {
			t.Errorf("formatCPUCell(%v, %q): got %q, want %q", c.pct, c.limit, got, c.want)
		}
	}
}

// TestFormatCores pins the cores-notation rendering: trailing
// zeros stripped, ultra-low values get extra precision so they
// don't collapse to "0".
func TestFormatCores(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{2, "2"},
		{0.5, "0.5"},
		{0.6, "0.6"},
		{0.45, "0.45"},
		{0.045, "0.05"},
		{0.001, "0.001"},
		{1.5, "1.5"},
	}

	for _, c := range cases {
		if got := formatCores(c.in); got != c.want {
			t.Errorf("formatCores(%v): got %q, want %q", c.in, got, c.want)
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
