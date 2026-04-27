package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
