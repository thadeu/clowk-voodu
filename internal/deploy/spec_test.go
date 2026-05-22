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
			Image: "",
			Build: &wireBuildSpec{
				Context:    "apps/api",
				Dockerfile: "Dockerfile.api",
				Lang:       &wireLangSpec{Name: "go", Version: "1.25"},
			},
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

	if spec.Build == nil {
		t.Fatal("build lost in roundtrip")
	}

	if spec.Build.Dockerfile != "Dockerfile.api" {
		t.Errorf("dockerfile lost in roundtrip: %q", spec.Build.Dockerfile)
	}

	if spec.Build.Lang == nil || spec.Build.Lang.Name != "go" {
		t.Errorf("lang lost: %+v", spec.Build.Lang)
	}
}

func TestFetchSpec_StatefulsetOnly(t *testing.T) {
	srv := fakeController(t, []controller.Manifest{{
		Kind:  controller.KindStatefulset,
		Scope: "data",
		Name:  "pg",
		Spec: encodeSpec(t, wireStatefulsetSpec{
			Image: "",
			Build: &wireBuildSpec{
				Context:    "infra/postgres",
				Dockerfile: "Dockerfile.pg",
			},
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

	if spec.Build == nil {
		t.Fatal("build lost")
	}

	if spec.Build.Context != "infra/postgres" {
		t.Errorf("context lost: %q", spec.Build.Context)
	}

	if spec.Build.Dockerfile != "Dockerfile.pg" {
		t.Errorf("dockerfile lost: %q", spec.Build.Dockerfile)
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
			Spec:  encodeSpec(t, wireDeploymentSpec{Build: &wireBuildSpec{Context: "."}}),
		},
		{
			Kind:  controller.KindStatefulset,
			Scope: "x",
			Name:  "y",
			Spec:  encodeSpec(t, wireStatefulsetSpec{Build: &wireBuildSpec{Context: "."}}),
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

// TestSpecFromCLIJSON_DeploymentShape pins the happy path: the CLI's
// parsed manifest.DeploymentSpec JSON round-trips into the same Spec
// FetchSpec would have produced via the controller — same Context,
// Dockerfile, BuildArgs. The "all pods point at api" bug existed
// precisely because this round-trip didn't exist; the build never saw
// the spec on the first apply.
func TestSpecFromCLIJSON_DeploymentShape(t *testing.T) {
	raw, err := json.Marshal(wireDeploymentSpec{
		Image: "",
		Build: &wireBuildSpec{
			Context:    "./apps/esl",
			Dockerfile: "Dockerfile",
			Args: map[string]string{
				"SERVICE": "adapter",
			},
			Lang: &wireLangSpec{Name: "go", Version: "1.26"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	spec, err := SpecFromCLIJSON(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if spec.Build == nil {
		t.Fatal("build block lost in roundtrip")
	}

	if spec.Build.Context != "./apps/esl" {
		t.Errorf("context = %q, want %q", spec.Build.Context, "./apps/esl")
	}

	if spec.Build.Args["SERVICE"] != "adapter" {
		t.Errorf("SERVICE arg = %q, want %q (regression of the per-resource override bug)",
			spec.Build.Args["SERVICE"], "adapter")
	}

	if spec.Build.Lang == nil || spec.Build.Lang.Version != "1.26" {
		t.Errorf("lang lost: %+v", spec.Build.Lang)
	}
}

// TestSpecFromCLIJSON_StatefulsetShape covers the statefulset case.
// Statefulset spec is a strict subset of deployment spec for the
// build-relevant fields, so the same decoder works — verify nothing
// crashes when PostDeploy/KeepReleases are absent.
func TestSpecFromCLIJSON_StatefulsetShape(t *testing.T) {
	raw, err := json.Marshal(wireStatefulsetSpec{
		Image: "",
		Build: &wireBuildSpec{
			Context:    "infra/postgres",
			Dockerfile: "Dockerfile.pg",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	spec, err := SpecFromCLIJSON(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if spec.Build == nil {
		t.Fatal("build block lost")
	}

	if spec.Build.Context != "infra/postgres" {
		t.Errorf("context = %q", spec.Build.Context)
	}

	if spec.Build.Dockerfile != "Dockerfile.pg" {
		t.Errorf("dockerfile = %q", spec.Build.Dockerfile)
	}
}

func TestSpecFromCLIJSON_EmptyErrors(t *testing.T) {
	if _, err := SpecFromCLIJSON(nil); err == nil {
		t.Error("nil bytes must error")
	}

	if _, err := SpecFromCLIJSON([]byte{}); err == nil {
		t.Error("empty bytes must error")
	}
}

func TestSpecFromCLIJSON_MalformedErrors(t *testing.T) {
	if _, err := SpecFromCLIJSON([]byte("not json")); err == nil {
		t.Error("garbage payload must error instead of silently falling back to zero-value")
	}
}

func TestSpecFromStatefulsetWire_PreservesAllFields(t *testing.T) {
	w := wireStatefulsetSpec{
		Image:       "",
		Env:         map[string]string{"PGAPPNAME": "voodu"},
		Ports:       []string{"5432"},
		Volumes:     []string{"/data"},
		NetworkMode: "bridge",
		Build: &wireBuildSpec{
			Context:    "infra/postgres",
			Dockerfile: "Dockerfile.pg",
			Path:       ".",
			Args:       map[string]string{"PG_MAJOR": "16"},
			Lang: &wireLangSpec{
				Name:       "generic",
				Version:    "16",
				Entrypoint: "docker-entrypoint.sh",
			},
		},
	}

	s := specFromStatefulsetWire(w)

	if s.Image != w.Image {
		t.Errorf("image lost: %q", s.Image)
	}

	if s.Build == nil {
		t.Fatal("build lost")
	}

	if s.Build.Context != "infra/postgres" || s.Build.Dockerfile != "Dockerfile.pg" {
		t.Errorf("build scalar fields lost: %+v", s.Build)
	}

	if s.Build.Args["PG_MAJOR"] != "16" {
		t.Errorf("build.args lost: %+v", s.Build.Args)
	}

	if s.Env["PGAPPNAME"] != "voodu" {
		t.Errorf("env lost: %+v", s.Env)
	}

	if len(s.Ports) != 1 || s.Ports[0] != "5432" {
		t.Errorf("ports lost: %+v", s.Ports)
	}

	if s.Build.Lang == nil || s.Build.Lang.Name != "generic" {
		t.Errorf("lang lost: %+v", s.Build.Lang)
	}
}
