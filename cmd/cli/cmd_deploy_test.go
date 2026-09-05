package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// deployTestServer stands in for the controller, recording what the CLI sent.
func deployTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)

	return srv
}

func runDeploy(t *testing.T, srv *httptest.Server, args ...string) (string, error) {
	t.Helper()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", srv.URL)

	var buf bytes.Buffer

	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)

	out := captureStdout(t, func() {
		if err := root.Execute(); err != nil {
			buf.WriteString(err.Error())
		}
	})

	return out + buf.String(), nil
}

func TestTriggerCreateSendsTheTrustStatement(t *testing.T) {
	var (
		mu   sync.Mutex
		body map[string]any
	)

	srv := deployTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/deploy/triggers" {
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Unlock()

			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"data": map[string]any{
					"id": "trg1", "repo": "acme/web", "branch": "main",
					"allow_scopes": []string{"runa"}, "enabled": true,
				},
			})

			return
		}

		t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
	})

	out, _ := runDeploy(t, srv,
		"deploy", "trigger", "create", "--repo", "acme/web", "--branch", "main", "--allow-scope", "runa")

	mu.Lock()
	defer mu.Unlock()

	if body["repo"] != "acme/web" || body["branch"] != "main" {
		t.Fatalf("body = %v", body)
	}

	scopes, _ := body["allow_scopes"].([]any)

	if len(scopes) != 1 || scopes[0] != "runa" {
		t.Fatalf("allow_scopes = %v", body["allow_scopes"])
	}

	if !strings.Contains(out, "trg1") {
		t.Errorf("the id must be printed, it is what the control plane fires against: %q", out)
	}

	// The operator's next step is a file in the repository, and saying so is
	// what stops them looking for a flag that configures branches here.
	if !strings.Contains(out, ".voodu") {
		t.Errorf("output should point at the repository file: %q", out)
	}
}

// `--allow-scope a,b` silently creating a scope literally named "a,b" is the
// kind of thing nobody notices until a deploy is refused.
func TestAllowScopeIsRepeatableNotCommaSeparated(t *testing.T) {
	var (
		mu   sync.Mutex
		body map[string]any
	)

	srv := deployTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]any{"id": "trg1", "repo": "acme/web", "branch": "main"},
		})
	})

	runDeploy(t, srv, "deploy", "trigger", "create",
		"--repo", "acme/web", "--branch", "main",
		"--allow-scope", "runa", "--allow-scope", "staging")

	mu.Lock()
	defer mu.Unlock()

	scopes, _ := body["allow_scopes"].([]any)

	if len(scopes) != 2 {
		t.Fatalf("allow_scopes = %v, want two separate values", body["allow_scopes"])
	}
}

// An empty box should say how to fill it, not print an empty table.
func TestTriggerListEmptyStateExplainsItself(t *testing.T) {
	srv := deployTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]any{"triggers": []any{}},
		})
	})

	out, _ := runDeploy(t, srv, "deploy", "trigger", "list")

	if !strings.Contains(out, "No deploy triggers") {
		t.Fatalf("empty state: %q", out)
	}

	if !strings.Contains(out, "vd deploy trigger create") {
		t.Errorf("the empty state should name the way out: %q", out)
	}
}

// A trigger that never fired is usually the answer to why a deploy is not
// happening, so the column says so plainly rather than showing a blank.
func TestTriggerListShowsNeverFired(t *testing.T) {
	srv := deployTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{"triggers": []any{
				map[string]any{
					"id": "trg1", "repo": "acme/web", "branch": "main",
					"allow_scopes": []string{"runa"}, "enabled": false,
				},
			}},
		})
	})

	out, _ := runDeploy(t, srv, "deploy", "trigger", "list")

	if !strings.Contains(out, "paused") {
		t.Errorf("a paused trigger must say so: %q", out)
	}

	if !strings.Contains(out, "—") {
		t.Errorf("never-fired should render a dash, not a blank: %q", out)
	}
}

// The controller already explains what was wrong with the input; re-describing
// it here would be a second, worse explanation.
func TestTriggerErrorsSurfaceTheControllerMessage(t *testing.T) {
	srv := deployTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "error",
			"error":  "trigger: allow_scopes is required",
		})
	})

	out, _ := runDeploy(t, srv, "deploy", "trigger", "create", "--repo", "acme/web", "--branch", "main")

	if !strings.Contains(out, "allow_scopes is required") {
		t.Fatalf("the controller's message must reach the operator: %q", out)
	}
}

// Pausing keeps the authorisation. It reads the record first so the other
// fields survive a toggle — a PUT that dropped them would silently widen or
// narrow what the repository may touch.
func TestDisableKeepsTheOtherFields(t *testing.T) {
	var (
		mu   sync.Mutex
		sent map[string]any
	)

	srv := deployTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"data": map[string]any{
					"id": "trg1", "repo": "acme/web", "branch": "main",
					"allow_scopes": []string{"runa", "staging"}, "enabled": true,
				},
			})

			return
		}

		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&sent)
		mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"id": "trg1", "repo": "acme/web", "branch": "main",
				"allow_scopes": []string{"runa", "staging"}, "enabled": false,
			},
		})
	})

	runDeploy(t, srv, "deploy", "trigger", "disable", "trg1")

	mu.Lock()
	defer mu.Unlock()

	if sent["enabled"] != false {
		t.Fatalf("enabled was not sent as false: %v", sent)
	}

	scopes, _ := sent["allow_scopes"].([]any)

	if len(scopes) != 2 {
		t.Fatalf("a toggle dropped the scopes: %v", sent)
	}
}
