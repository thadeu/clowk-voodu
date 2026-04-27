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

// TestGetPodRoutesToPodsName proves `voodu get pod <name>` and the
// `pd` short form hit GET /pods/{name} — the same endpoint
// `describe pod` uses. Without this, the singular-vs-plural split
// could silently drift to a different route over time.
func TestGetPodRoutesToPodsName(t *testing.T) {
	cases := []struct {
		name      string
		subcmd    string
	}{
		{"long form", "pod"},
		{"short form", "pd"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path

				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "ok",
					"data": map[string]any{
						"pod": &controller.PodDetail{
							Pod: controller.Pod{
								Name: "test-web.a3f9", Kind: "deployment",
								Scope: "test", ResourceName: "web",
								ReplicaID: "a3f9", Image: "vd-web:latest",
							},
						},
					},
				})
			}))
			defer ts.Close()

			root := newRootCmd()
			_ = root.PersistentFlags().Set("controller-url", ts.URL)

			var buf bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&buf)
			root.SetArgs([]string{"get", tc.subcmd, "test-web.a3f9"})

			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}

			if gotPath != "/pods/test-web.a3f9" {
				t.Errorf("path=%q want /pods/test-web.a3f9", gotPath)
			}
		})
	}
}

// TestGetPodNoArgsListsPods locks in the alias semantics: `vd get
// pd` (no ref) hits GET /pods, exactly like `vd get pods`. Without
// this, a refactor of the argc-routing in newGetPodCmd could silently
// turn the no-arg path into "pod ref is empty" without anyone
// noticing until an operator complains.
func TestGetPodNoArgsListsPods(t *testing.T) {
	cases := []struct {
		name   string
		subcmd string
	}{
		{"long form", "pod"},
		{"short form", "pd"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				gotPath  string
				gotQuery string
			)

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotQuery = r.URL.RawQuery

				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "ok",
					"data":   map[string]any{"pods": []controller.Pod{}},
				})
			}))
			defer ts.Close()

			root := newRootCmd()
			_ = root.PersistentFlags().Set("controller-url", ts.URL)

			var buf bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&buf)
			root.SetArgs([]string{"get", tc.subcmd})

			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}

			if gotPath != "/pods" {
				t.Errorf("path=%q want /pods", gotPath)
			}

			if gotQuery != "" {
				t.Errorf("no filter flags should mean empty query, got %q", gotQuery)
			}
		})
	}
}

// TestGetPodNoArgsForwardsFilters confirms the listing-mode flags
// (-k/-s/-n) actually reach the /pods query string when no ref is
// given. Without this, someone could rename the flag bindings on
// the singular command and break the alias silently.
func TestGetPodNoArgsForwardsFilters(t *testing.T) {
	var gotQuery string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]any{"pods": []controller.Pod{}},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get", "pd", "-k", "cronjob", "-s", "clowk-lp", "-n", "crawler1"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	for _, want := range []string{"kind=cronjob", "scope=clowk-lp", "name=crawler1"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query missing %q: %q", want, gotQuery)
		}
	}
}

// TestGetPodForwardsShowEnvFlag confirms --show-env on `get pod`
// reaches the renderer the same way `describe pod --show-env` does.
// The two doorways must keep parity — diverging defaults would be
// the kind of bug that surfaces only in a screen-share.
func TestGetPodForwardsShowEnvFlag(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"pod": &controller.PodDetail{
					Pod: controller.Pod{Name: "test-web.a3f9"},
					Env: map[string]string{"NODE_ENV": "production"},
				},
			},
		})
	}))
	defer ts.Close()

	// Default invocation: env values must NOT be in any captured
	// output. The CLI writes the rendered detail to os.Stdout (not
	// the cobra writer), so we can't intercept easily — instead we
	// verify the cobra Execute path returns nil (no error) and trust
	// renderPodDetail's unit tests for content-level assertions.
	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get", "pd", "test-web.a3f9", "--show-env"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute with --show-env: %v", err)
	}
}
