package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

// TestExecPickTarget_ContainerNamePassesThrough confirms a ref with
// a dot is treated as a container name and bypasses /pods listing.
// Without this, exec would do an unnecessary lookup for refs the
// operator already disambiguated.
func TestExecPickTarget_ContainerNamePassesThrough(t *testing.T) {
	root := newRootCmd()

	got, err := pickExecTarget(root, "clowk-lp-web.a3f9", "")
	if err != nil {
		t.Fatalf("pickExecTarget: %v", err)
	}

	if got != "clowk-lp-web.a3f9" {
		t.Errorf("got %q, want clowk-lp-web.a3f9", got)
	}
}

// TestExecPickTarget_OverrideWins locks in --container precedence:
// the flag overrides whatever the ref would resolve to. Useful when
// the operator already eyeballed `vd get pd` and wants to skip the
// resolution.
func TestExecPickTarget_OverrideWins(t *testing.T) {
	root := newRootCmd()

	got, err := pickExecTarget(root, "clowk-lp/web", "explicit.aaaa")
	if err != nil {
		t.Fatalf("pickExecTarget: %v", err)
	}

	if got != "explicit.aaaa" {
		t.Errorf("got %q, want explicit.aaaa", got)
	}
}

// TestExecPickTarget_SinglePodResolved exercises the happy
// scope/name path: one match → return its container name.
func TestExecPickTarget_SinglePodResolved(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"pods": []controller.Pod{
					{Name: "clowk-lp-web.aaaa", Kind: "deployment", Scope: "clowk-lp", ResourceName: "web"},
				},
			},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	got, err := pickExecTarget(root, "clowk-lp/web", "")
	if err != nil {
		t.Fatalf("pickExecTarget: %v", err)
	}

	if got != "clowk-lp-web.aaaa" {
		t.Errorf("got %q, want clowk-lp-web.aaaa", got)
	}
}

// TestExecPickTarget_ScopeNameAutoPicksRunningReplica covers the
// k8s-style ergonomic: when scope/name matches multiple replicas,
// auto-pick the best one (running first, then by recency) instead
// of erroring. Operators reach for `vd exec scope/name -- bash` to
// debug a deploy, not to pick a specific replica id from a list.
func TestExecPickTarget_ScopeNameAutoPicksRunningReplica(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"pods": []controller.Pod{
					{Name: "clowk-lp-web.aaaa", Running: false, CreatedAt: "2026-04-25T00:02:00Z"},
					{Name: "clowk-lp-web.bbbb", Running: true, CreatedAt: "2026-04-25T00:00:00Z"},
					{Name: "clowk-lp-web.cccc", Running: true, CreatedAt: "2026-04-25T00:01:00Z"},
				},
			},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	got, err := pickExecTarget(root, "clowk-lp/web", "")
	if err != nil {
		t.Fatalf("pickExecTarget: %v", err)
	}

	// Among running replicas, the most recent CreatedAt wins.
	// `cccc` (running, 00:01) over `bbbb` (running, 00:00) over
	// `aaaa` (stopped, even though most recent overall).
	if got != "clowk-lp-web.cccc" {
		t.Errorf("got %q, want clowk-lp-web.cccc (running + latest)", got)
	}
}

// TestExecPickTarget_ScopeNameAllStoppedFallsBackToLatest confirms
// the no-running-replicas case still resolves: pick the most-recent
// stopped one. Useful for debugging a recently-crashed deploy via
// post-mortem exec.
func TestExecPickTarget_ScopeNameAllStoppedFallsBackToLatest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"pods": []controller.Pod{
					{Name: "clowk-lp-web.aaaa", Running: false, CreatedAt: "2026-04-25T00:00:00Z"},
					{Name: "clowk-lp-web.bbbb", Running: false, CreatedAt: "2026-04-25T00:02:00Z"},
				},
			},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	got, err := pickExecTarget(root, "clowk-lp/web", "")
	if err != nil {
		t.Fatalf("pickExecTarget: %v", err)
	}

	if got != "clowk-lp-web.bbbb" {
		t.Errorf("got %q, want clowk-lp-web.bbbb (latest stopped)", got)
	}
}

// TestExecPickTarget_BareScopeStillErrorsOnMultiMatch protects the
// "scope alone is too ambiguous" path: a bare scope can match
// containers from different kinds (deployment + job + cronjob) and
// auto-picking across kinds would be too surprising. Operator must
// pass --container or use scope/name to narrow.
func TestExecPickTarget_BareScopeStillErrorsOnMultiMatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"pods": []controller.Pod{
					{Name: "clowk-lp-web.aaaa", Kind: "deployment"},
					{Name: "clowk-lp-migrate.bbbb", Kind: "job"},
				},
			},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	_, err := pickExecTarget(root, "clowk-lp", "")
	if err == nil {
		t.Fatal("expected error for bare-scope multi-match across kinds")
	}

	for _, want := range []string{"matches 2 containers", "--container"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %q", want, err.Error())
		}
	}
}

// TestAutoPrependEnv_TerminalWiringPreserved is the regression
// guard for the vim-arrows fix: TTY exec must auto-forward TERM
// (and LANG/LC_ALL when set) so the remote shell loads the right
// termcap. Without this, arrow keys / line editing / colors break
// inside the container.
func TestAutoPrependEnv_TerminalWiringPreserved(t *testing.T) {
	t.Setenv("TERM", "xterm-kitty")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "")

	got := autoPrependEnv(nil, "TERM", "xterm-256color")
	if !contains(got, "TERM=xterm-kitty") {
		t.Errorf("local TERM should win, got %v", got)
	}

	got = autoPrependEnv(nil, "LANG", "")
	if !contains(got, "LANG=en_US.UTF-8") {
		t.Errorf("LANG should propagate, got %v", got)
	}

	// LC_ALL empty locally + empty fallback → skip entirely. We
	// don't want to pass "LC_ALL=" because some apps treat that
	// differently from "LC_ALL unset".
	got = autoPrependEnv(nil, "LC_ALL", "")
	if len(got) != 0 {
		t.Errorf("empty LC_ALL with empty fallback should skip, got %v", got)
	}
}

// TestAutoPrependEnv_UserOverrideWins locks in the precedence:
// when the operator explicitly passes -e TERM=screen, that
// trumps the auto-forward. Without this, our defaults would
// silently override deliberate operator intent.
func TestAutoPrependEnv_UserOverrideWins(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")

	existing := []string{"TERM=screen", "FOO=bar"}

	got := autoPrependEnv(existing, "TERM", "xterm-256color")

	if !contains(got, "TERM=screen") {
		t.Errorf("user-provided TERM should win, got %v", got)
	}

	if contains(got, "TERM=xterm-256color") {
		t.Errorf("auto value must not be added when user provided one, got %v", got)
	}
}

// TestAutoPrependEnv_FallbackWhenLocalUnset confirms the safety
// net: if the operator's shell somehow has no TERM (rare but
// possible in CI), we fall back to xterm-256color rather than
// shipping nothing and breaking the remote terminal.
func TestAutoPrependEnv_FallbackWhenLocalUnset(t *testing.T) {
	t.Setenv("TERM", "")

	got := autoPrependEnv(nil, "TERM", "xterm-256color")
	if !contains(got, "TERM=xterm-256color") {
		t.Errorf("fallback should kick in when local TERM is empty, got %v", got)
	}
}

// contains is a tiny helper for the env-slice assertions.
func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}

	return false
}

// TestExecPickTarget_NoMatchErrors confirms zero-result feedback:
// scope/name resolves to nothing → clear error mentioning the ref.
func TestExecPickTarget_NoMatchErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]any{"pods": []controller.Pod{}},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	_, err := pickExecTarget(root, "missing/app", "")
	if err == nil {
		t.Fatal("expected error for unmatched ref")
	}

	if !strings.Contains(err.Error(), "missing/app") {
		t.Errorf("error should name the ref, got %q", err.Error())
	}
}
