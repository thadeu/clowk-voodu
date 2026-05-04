// Tests for FetchSpec — the receive-pack entry point that resolves
// build configuration for an inbound `<scope>/<name>` ref. Pinning:
//
//   - Deployment-only ref returns the deployment's spec.
//   - Statefulset-only ref returns the statefulset's spec.
//   - Both kinds present with the same (scope, name) → ErrSpecAmbiguous.
//   - Neither present → (nil, nil), the well-known "first apply hasn't
//     happened yet" path that lets receive-pack fall back to defaults.
//
// The controller HTTP surface is mocked with httptest — we don't spin
// up a real controller, just answer /apply queries the way the real
// one would.

package deploy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

// fakeController returns an http.Handler that answers /apply queries
// with a fixed manifest list. The handler filters by ?kind= the way
// the real /apply endpoint does, so callers see only manifests of the
// requested kind.
func fakeController(t *testing.T, manifests []controller.Manifest) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/apply", func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("kind")

		var filtered []controller.Manifest

		for _, m := range manifests {
			if string(m.Kind) == kind {
				filtered = append(filtered, m)
			}
		}

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(map[string]any{
			"data": filtered,
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	})

	return httptest.NewServer(mux)
}

func encodeSpec(t *testing.T, v any) json.RawMessage {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return b
}

func TestFetchSpec_DeploymentOnly(t *testing.T) {
	srv := fakeController(t, []controller.Manifest{{
		Kind:  controller.KindDeployment,
		Scope: "prod",
		Name:  "api",
		Spec: encodeSpec(t, wireDeploymentSpec{
			Image:      "",
			Dockerfile: "Dockerfile.api",
			Path:       "apps/api",
			Lang:       &wireLangSpec{Name: "go", Version: "1.25"},
		}),
	}})
	defer srv.Close()

	spec, err := FetchSpec(srv.URL, "prod", "api")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if spec == nil {
		t.Fatal("expected spec, got nil")
	}

	if spec.Dockerfile != "Dockerfile.api" {
		t.Errorf("dockerfile lost in roundtrip: %q", spec.Dockerfile)
	}

	if spec.Lang == nil || spec.Lang.Name != "go" {
		t.Errorf("lang lost: %+v", spec.Lang)
	}
}

func TestFetchSpec_StatefulsetOnly(t *testing.T) {
	srv := fakeController(t, []controller.Manifest{{
		Kind:  controller.KindStatefulset,
		Scope: "data",
		Name:  "pg",
		Spec: encodeSpec(t, wireStatefulsetSpec{
			Image:      "",
			Workdir:    "infra/postgres",
			Dockerfile: "Dockerfile.pg",
			Path:       ".",
		}),
	}})
	defer srv.Close()

	spec, err := FetchSpec(srv.URL, "data", "pg")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if spec == nil {
		t.Fatal("expected spec, got nil")
	}

	if spec.Workdir != "infra/postgres" {
		t.Errorf("workdir lost: %q", spec.Workdir)
	}

	if spec.Dockerfile != "Dockerfile.pg" {
		t.Errorf("dockerfile lost: %q", spec.Dockerfile)
	}
}

func TestFetchSpec_AmbiguousWhenBothKindsExist(t *testing.T) {
	// Deployment AND statefulset under same (scope, name) — receive-pack
	// can't pick. Operator must rename one to disambiguate.
	srv := fakeController(t, []controller.Manifest{
		{
			Kind:  controller.KindDeployment,
			Scope: "x",
			Name:  "y",
			Spec:  encodeSpec(t, wireDeploymentSpec{Path: "."}),
		},
		{
			Kind:  controller.KindStatefulset,
			Scope: "x",
			Name:  "y",
			Spec:  encodeSpec(t, wireStatefulsetSpec{Path: "."}),
		},
	})
	defer srv.Close()

	_, err := FetchSpec(srv.URL, "x", "y")
	if err == nil {
		t.Fatal("expected ErrSpecAmbiguous, got nil")
	}

	if err != ErrSpecAmbiguous {
		t.Errorf("expected ErrSpecAmbiguous, got: %v", err)
	}
}

func TestFetchSpec_NeitherKindExists(t *testing.T) {
	// First-apply scenario: nothing in the controller for this ref.
	// Must return (nil, nil) so receive-pack can fall back to defaults.
	srv := fakeController(t, nil)
	defer srv.Close()

	spec, err := FetchSpec(srv.URL, "scope", "name")
	if err != nil {
		t.Fatalf("nil result should not error: %v", err)
	}

	if spec != nil {
		t.Errorf("expected nil spec for unknown ref, got %+v", spec)
	}
}

func TestFetchSpec_EmptyControllerURLReturnsNil(t *testing.T) {
	// Used by receive-pack callers that haven't been configured yet —
	// don't error, just return nil so the fallback path runs.
	spec, err := FetchSpec("", "scope", "name")
	if err != nil {
		t.Fatalf("empty controller URL must not error: %v", err)
	}

	if spec != nil {
		t.Errorf("expected nil spec, got %+v", spec)
	}
}

func TestSpecFromStatefulsetWire_PreservesAllFields(t *testing.T) {
	w := wireStatefulsetSpec{
		Image:       "postgres:16",
		Dockerfile:  "Dockerfile.pg",
		Path:        "infra/postgres",
		Workdir:     "subdir",
		Env:         map[string]string{"PGAPPNAME": "voodu"},
		Ports:       []string{"5432"},
		Volumes:     []string{"/data"},
		NetworkMode: "bridge",
		Lang: &wireLangSpec{
			Name:       "generic",
			Version:    "16",
			Entrypoint: "docker-entrypoint.sh",
			BuildArgs:  map[string]string{"PG_MAJOR": "16"},
		},
	}

	s := specFromStatefulsetWire(w)

	if s.Image != w.Image || s.Dockerfile != w.Dockerfile || s.Path != w.Path || s.Workdir != w.Workdir {
		t.Errorf("scalar fields lost: %+v", s)
	}

	if s.Env["PGAPPNAME"] != "voodu" {
		t.Errorf("env lost: %+v", s.Env)
	}

	if len(s.Ports) != 1 || s.Ports[0] != "5432" {
		t.Errorf("ports lost: %+v", s.Ports)
	}

	if s.Lang == nil || s.Lang.Name != "generic" || s.Lang.BuildArgs["PG_MAJOR"] != "16" {
		t.Errorf("lang lost: %+v", s.Lang)
	}
}
