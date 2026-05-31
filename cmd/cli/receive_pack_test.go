package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
)

func TestParseScopedRef(t *testing.T) {
	cases := []struct {
		in    string
		scope string
		name  string
		ok    bool
	}{
		{"clowk/web", "clowk", "web", true},
		{"web", "", "web", true},
		{"", "", "", false},
		{"/web", "", "", false},
		{"clowk/", "", "", false},
		{"a/b/c", "", "", false},
		{"a//b", "", "", false},
	}

	for _, tc := range cases {
		scope, name, err := parseScopedRef(tc.in)

		if tc.ok {
			if err != nil {
				t.Errorf("parseScopedRef(%q) unexpected error: %v", tc.in, err)
				continue
			}

			if scope != tc.scope || name != tc.name {
				t.Errorf("parseScopedRef(%q) = (%q,%q), want (%q,%q)", tc.in, scope, name, tc.scope, tc.name)
			}

			continue
		}

		if err == nil {
			t.Errorf("parseScopedRef(%q) expected error, got (%q,%q)", tc.in, scope, name)
		}
	}
}

// rootWithControllerURL builds a minimal cobra command tree carrying the
// --controller-url persistent flag the resolveReceivePackSpec test path
// reads via controllerURL(). The real CLI wires this up in main; the
// test re-creates just enough of that surface to exercise the precedence
// logic without spinning up the whole command graph.
func rootWithControllerURL(t *testing.T, url string) *cobra.Command {
	t.Helper()

	root := &cobra.Command{Use: "voodu"}
	root.PersistentFlags().String("controller-url", url, "")

	return root
}

// TestResolveReceivePackSpec_InlineSpecWinsOverController pins the
// chicken-and-egg fix: when --spec is present, receive-pack MUST NOT
// hit the controller — the CLI shipped authoritative build config
// inline. A controller stub that would 500 confirms we never call it
// on the happy path.
func TestResolveReceivePackSpec_InlineSpecWinsOverController(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("controller hit despite inline --spec: %s %s", r.Method, r.URL)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	inline := map[string]any{
		"build": map[string]any{
			"context":    "./apps/esl",
			"dockerfile": "Dockerfile",
			"args": map[string]any{
				"SERVICE": "adapter",
			},
		},
	}

	raw, err := json.Marshal(inline)
	if err != nil {
		t.Fatal(err)
	}

	root := rootWithControllerURL(t, srv.URL)

	spec, err := resolveReceivePackSpec(root, "fsw", "adapter", base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if spec == nil || spec.Build == nil {
		t.Fatal("expected build spec, got nil")
	}

	if spec.Build.Args["SERVICE"] != "adapter" {
		t.Errorf("SERVICE arg = %q, want %q (inline --spec lost in roundtrip)",
			spec.Build.Args["SERVICE"], "adapter")
	}
}

// TestResolveReceivePackSpec_FallsBackToController covers the back-
// compat path: older CLIs that don't pass --spec still resolve build
// config via the controller HTTP API. The stub mimics the real /apply
// handler's "return matching kind" shape.
func TestResolveReceivePackSpec_FallsBackToController(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("kind")

		var data []controller.Manifest

		if kind == string(controller.KindDeployment) {
			data = []controller.Manifest{{
				Kind:  controller.KindDeployment,
				Scope: "fsw",
				Name:  "api",
				Spec:  json.RawMessage(`{"build":{"args":{"SERVICE":"api"}}}`),
			}}
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	root := rootWithControllerURL(t, srv.URL)

	spec, err := resolveReceivePackSpec(root, "fsw", "api", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if spec == nil || spec.Build == nil {
		t.Fatal("expected build spec from controller fallback, got nil")
	}

	if spec.Build.Args["SERVICE"] != "api" {
		t.Errorf("SERVICE arg = %q, want %q", spec.Build.Args["SERVICE"], "api")
	}
}

// TestResolveReceivePackSpec_MalformedInline surfaces a typo in the
// base64 payload instead of silently falling back to the controller —
// silent fallback is exactly what produced the original "all pods run
// the api binary" bug.
func TestResolveReceivePackSpec_MalformedInline(t *testing.T) {
	root := rootWithControllerURL(t, "http://127.0.0.1:1")

	if _, err := resolveReceivePackSpec(root, "fsw", "api", "not-base64!!"); err == nil {
		t.Fatal("expected decode error for malformed --spec, got nil")
	}
}

// TestBuildPlan pins the build-once fan-out selection: the FIRST
// deployment/job becomes the single build target (primary), every other
// deployment/job reuses its image, and ingress (no source) is excluded.
// Declaration order is load-bearing — the primary is always the first
// Procfile line.
func TestBuildPlan(t *testing.T) {
	dep := func(scope, name string) controller.Manifest {
		return controller.Manifest{Kind: controller.KindDeployment, Scope: scope, Name: name}
	}
	job := func(scope, name string) controller.Manifest {
		return controller.Manifest{Kind: controller.KindJob, Scope: scope, Name: name}
	}
	ing := func(scope, name string) controller.Manifest {
		return controller.Manifest{Kind: controller.KindIngress, Scope: scope, Name: name}
	}

	cases := []struct {
		name        string
		mans        []controller.Manifest
		wantPrimary string
		wantReuse   []string
	}{
		{
			name:        "web+worker+sync with web ingress",
			mans:        []controller.Manifest{dep("clowk", "web"), dep("clowk", "worker"), dep("clowk", "sync"), ing("clowk", "web")},
			wantPrimary: "clowk-web",
			wantReuse:   []string{"clowk-worker", "clowk-sync"},
		},
		{
			name:        "release job first, order preserved",
			mans:        []controller.Manifest{job("clowk", "release"), dep("clowk", "web")},
			wantPrimary: "clowk-release",
			wantReuse:   []string{"clowk-web"},
		},
		{
			name:        "single deployment, no reuse",
			mans:        []controller.Manifest{dep("clowk", "web")},
			wantPrimary: "clowk-web",
			wantReuse:   nil,
		},
		{
			name:        "ingress only yields no buildable",
			mans:        []controller.Manifest{ing("clowk", "web")},
			wantPrimary: "",
			wantReuse:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primary, reuse := buildPlan(tc.mans)

			if primary != tc.wantPrimary {
				t.Errorf("primary = %q, want %q", primary, tc.wantPrimary)
			}

			if len(reuse) != len(tc.wantReuse) {
				t.Fatalf("reuse = %v, want %v", reuse, tc.wantReuse)
			}

			for i := range reuse {
				if reuse[i] != tc.wantReuse[i] {
					t.Errorf("reuse[%d] = %q, want %q", i, reuse[i], tc.wantReuse[i])
				}
			}
		})
	}
}
