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

// TestGetPodsRequestsControllerEndpoint exercises the wire contract:
// the CLI hits GET /pods, optionally with the right query params, and
// the controller answer flows through unchanged.
func TestGetPodsRequestsControllerEndpoint(t *testing.T) {
	var (
		gotPath     string
		gotRawQuery string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery

		resp := map[string]any{
			"status": "ok",
			"data": map[string]any{
				"pods": []controller.Pod{
					{
						Name: "softphone-web.a3f9", Kind: "deployment", Scope: "softphone",
						ResourceName: "web", ReplicaID: "a3f9", Image: "softphone-web:latest",
						Status: "Up 1 minute", Running: true,
					},
				},
			},
		}

		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer

	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"get", "pods", "--kind", "deployment", "--scope", "softphone"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if gotPath != "/pods" {
		t.Errorf("path=%q want /pods", gotPath)
	}

	if !strings.Contains(gotRawQuery, "kind=deployment") || !strings.Contains(gotRawQuery, "scope=softphone") {
		t.Errorf("raw query=%q missing filters", gotRawQuery)
	}
}

func TestGetPodsRendersTextTable(t *testing.T) {
	pods := []controller.Pod{
		{
			Name: "softphone-web.a3f9", Kind: "deployment", Scope: "softphone",
			ResourceName: "web", ReplicaID: "a3f9", Image: "softphone-web:latest",
			Status: "Up 2 hours", Running: true,
		},
		{
			Name: "softphone-web.bb01", Kind: "deployment", Scope: "softphone",
			ResourceName: "web", ReplicaID: "bb01", Image: "softphone-web:latest",
			Status: "Up 5 minutes", Running: true,
		},
	}

	var buf bytes.Buffer
	if err := renderPodsTable(&buf, pods); err != nil {
		t.Fatal(err)
	}

	out := buf.String()

	for _, want := range []string{
		"NAME", "KIND", "SCOPE", "RESOURCE", "IMAGE", "STATUS",
		"softphone-web.a3f9", "softphone-web.bb01", "deployment", "softphone",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}
}

func TestGetPodsRendersLegacyKind(t *testing.T) {
	pods := []controller.Pod{
		{
			Name: "old-app", Image: "old:latest", Status: "Up 1 day", Running: true,
		},
	}

	var buf bytes.Buffer
	if err := renderPodsTable(&buf, pods); err != nil {
		t.Fatal(err)
	}

	out := buf.String()

	if !strings.Contains(out, "(legacy)") {
		t.Errorf("legacy pod missing (legacy) kind marker:\n%s", out)
	}

	if !strings.Contains(out, "old-app") {
		t.Errorf("legacy pod name missing:\n%s", out)
	}
}

func TestGetPodsEmptyMessage(t *testing.T) {
	var buf bytes.Buffer
	if err := renderPodsTable(&buf, nil); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "No voodu-managed containers found") {
		t.Errorf("empty render missing helpful message: %q", buf.String())
	}
}

// TestGetPodsGroupsByRole pins the section-with-divider rendering:
// pods are bucketed by voodu.role and each section gets a header
// like `=== role (count) ===`. Backup jobs don't drown out the
// actual services anymore.
func TestGetPodsGroupsByRole(t *testing.T) {
	pods := []controller.Pod{
		{Name: "clowk-lp-web.221a", Kind: "deployment", Scope: "clowk-lp", ResourceName: "web", Role: "deployment", Image: "web:latest", Status: "Up", Running: true},
		{Name: "clowk-lp-db.0", Kind: "statefulset", Scope: "clowk-lp", ResourceName: "db", Role: "statefulset", Image: "postgres:16", Status: "Up", Running: true},
		{Name: "clowk-lp-db.bk.b001.fac3", Kind: "job", Scope: "clowk-lp", ResourceName: "db.bk.b001", Role: "backup", Image: "postgres:16", Status: "Exited (0)", Running: false},
		{Name: "clowk-lp-db.bk.b002.f150", Kind: "job", Scope: "clowk-lp", ResourceName: "db.bk.b002", Role: "backup", Image: "postgres:16", Status: "Exited (0)", Running: false},
	}

	var buf bytes.Buffer
	if err := renderPodsTable(&buf, pods); err != nil {
		t.Fatal(err)
	}

	out := buf.String()

	// Each role shows up as its own section header with count.
	wantSections := []string{
		"=== deployment (1) ===",
		"=== statefulset (1) ===",
		"=== backup (2) ===",
	}

	for _, want := range wantSections {
		if !strings.Contains(out, want) {
			t.Errorf("missing section header %q in output:\n%s", want, out)
		}
	}

	// Priority order: deployment before statefulset before backup.
	deployIdx := strings.Index(out, "=== deployment")
	stateIdx := strings.Index(out, "=== statefulset")
	backupIdx := strings.Index(out, "=== backup")

	if deployIdx < 0 || stateIdx < 0 || backupIdx < 0 {
		t.Fatal("one of the expected sections missing")
	}

	if !(deployIdx < stateIdx && stateIdx < backupIdx) {
		t.Errorf("section priority wrong: deployment(%d), statefulset(%d), backup(%d)",
			deployIdx, stateIdx, backupIdx)
	}
}

// TestGetPodsRoleEmptyGoesToOrphans covers the no-role path: a
// container without voodu.role — even WITH a kind label — lands in
// the orphans section, NOT a kind-named one. This way the operator
// sees a single "what needs migrating" bucket instead of role and
// kind groups commingled.
func TestGetPodsRoleEmptyGoesToOrphans(t *testing.T) {
	pods := []controller.Pod{
		// Has Kind but no Role (e.g. a pre-M0+role container).
		{Name: "clowk-lp-web.aaaa", Kind: "deployment", Scope: "clowk-lp", ResourceName: "web", Image: "web:latest", Status: "Up", Running: true},
		// Truly orphan — no Kind, no Role.
		{Name: "old-rogue-app", Image: "old:1", Status: "Up", Running: true},
	}

	var buf bytes.Buffer
	if err := renderPodsTable(&buf, pods); err != nil {
		t.Fatal(err)
	}

	out := buf.String()

	// Both pods land in the SAME orphans section.
	if !strings.Contains(out, "=== orphans (2) ===") {
		t.Errorf("expected single orphans section with count 2:\n%s", out)
	}

	// No kind-named section gets created from the Kind fallback.
	if strings.Contains(out, "=== deployment") {
		t.Errorf("pod with empty Role should NOT create a deployment section:\n%s", out)
	}

	// Both pod names render inside the orphans table.
	for _, want := range []string{"clowk-lp-web.aaaa", "old-rogue-app"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing pod %q in orphans table:\n%s", want, out)
		}
	}
}

// TestGetPodsOrphansAlwaysLast pins the priority: orphans bucket
// goes after EVERY other section regardless of count. Operators
// should see live structured pods first, with the to-migrate
// archaeology at the bottom.
func TestGetPodsOrphansAlwaysLast(t *testing.T) {
	pods := []controller.Pod{
		{Name: "old-app-1", Image: "old:1", Status: "Up", Running: true},
		{Name: "clowk-lp-web.aaaa", Kind: "deployment", Scope: "clowk-lp", ResourceName: "web", Role: "deployment", Image: "web:latest", Status: "Up", Running: true},
	}

	var buf bytes.Buffer
	if err := renderPodsTable(&buf, pods); err != nil {
		t.Fatal(err)
	}

	out := buf.String()

	deployIdx := strings.Index(out, "=== deployment")
	orphIdx := strings.Index(out, "=== orphans")

	if deployIdx < 0 || orphIdx < 0 {
		t.Fatalf("missing sections:\n%s", out)
	}

	if deployIdx > orphIdx {
		t.Errorf("orphans section must come last; got deployment(%d) > orphans(%d)",
			deployIdx, orphIdx)
	}
}
