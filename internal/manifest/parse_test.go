package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

func TestParseHCLDeployment(t *testing.T) {
	src := `
deployment "test" "api" {
  image = "nginx:${TAG:-1}"
  replicas = 3
  ports = ["8080", "8443"]
}
`
	tmp := writeTemp(t, "dep.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	if mans[0].Kind != controller.KindDeployment || mans[0].Name != "api" {
		t.Errorf("unexpected header: %+v", mans[0])
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Image != "nginx:1" {
		t.Errorf("interpolation failed: %q", spec.Image)
	}

	if spec.Replicas != 3 {
		t.Errorf("replicas: got %d", spec.Replicas)
	}

	if len(spec.Ports) != 2 {
		t.Errorf("ports: %+v", spec.Ports)
	}
}

func TestParseHCLDeploymentBuildMode(t *testing.T) {
	// The pipeline/container concerns stay at root (workdir, path,
	// ports, env, post_deploy, ...) while runtime build inputs live
	// inside the unified `lang {}` block. Go-specific cross-compile
	// flags (GOOS/GOARCH/CGO_ENABLED) ride inside build_args.
	src := `
deployment "test" "api" {
  workdir      = "apps/esl"
  dockerfile   = "Dockerfile.api"
  path         = "cmd/api"
  ports        = ["127.0.0.1:9092:9092"]
  volumes      = ["/opt/gokku/volumes/rtp:/app/recordings"]
  network      = "bridge"
  restart      = "unless-stopped"
  env          = { RAILS_ENV = "production" }
  post_deploy  = ["./bin/migrate"]
  health_check = "/healthz"

  lang {
    name    = "go"
    version = "1.25"
    build_args = {
      GOOS        = "linux"
      GOARCH      = "amd64"
      CGO_ENABLED = "0"
      GIT_SHA     = "abc123"
    }
  }
}
`
	tmp := writeTemp(t, "build.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Image != "" {
		t.Errorf("image should be empty in build mode, got %q", spec.Image)
	}

	if spec.Workdir != "apps/esl" || spec.Dockerfile != "Dockerfile.api" || spec.Path != "cmd/api" {
		t.Errorf("source fields not carried: %+v", spec)
	}

	if spec.Lang == nil {
		t.Fatal("lang block lost")
	}

	if spec.Lang.Name != "go" || spec.Lang.Version != "1.25" {
		t.Errorf("lang block fields lost: %+v", spec.Lang)
	}

	if spec.Lang.BuildArgs["GOOS"] != "linux" || spec.Lang.BuildArgs["GIT_SHA"] != "abc123" {
		t.Errorf("build_args lost: %+v", spec.Lang.BuildArgs)
	}

	if len(spec.PostDeploy) != 1 || spec.PostDeploy[0] != "./bin/migrate" {
		t.Errorf("post_deploy: %+v", spec.PostDeploy)
	}

	if spec.HealthCheck != "/healthz" || spec.Restart != "unless-stopped" || spec.Network != "bridge" {
		t.Errorf("runtime fields: %+v", spec)
	}
}

func TestParseHCLDeploymentLangBlockExotic(t *testing.T) {
	// The lang block accepts any name string — platforms the handler
	// registry doesn't know about (Elixir, Java, Haskell) still land
	// cleanly in the spec; handler dispatch at build time falls through
	// to the generic Dockerfile path.
	src := `
deployment "test" "api" {
  lang {
    name    = "elixir"
    version = "1.17"
    build_args = {
      MIX_ENV = "prod"
    }
  }
}
`
	tmp := writeTemp(t, "exotic.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Lang == nil || spec.Lang.Name != "elixir" || spec.Lang.Version != "1.17" {
		t.Errorf("exotic lang not carried: %+v", spec.Lang)
	}

	if spec.Lang.BuildArgs["MIX_ENV"] != "prod" {
		t.Errorf("build_args lost: %+v", spec.Lang.BuildArgs)
	}
}

func TestParseHCLDeploymentImageOptional(t *testing.T) {
	// Minimal build-mode deployment: no image, no path either. Parser
	// fills in `path="."` / `dockerfile="Dockerfile"` so `deployment {}`
	// means "build the repo root with ./Dockerfile" without requiring
	// the operator to spell it out.
	src := `deployment "test" "api" {}`

	tmp := writeTemp(t, "bare.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatalf("image should be optional for build mode: %v", err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Image != "" {
		t.Errorf("image should stay empty: %+v", spec)
	}

	if spec.Path != "." {
		t.Errorf("path default not applied: got %q, want %q", spec.Path, ".")
	}

	if spec.Dockerfile != "Dockerfile" {
		t.Errorf("dockerfile default not applied: got %q, want %q", spec.Dockerfile, "Dockerfile")
	}

	if spec.Workdir != "" {
		t.Errorf("workdir should stay empty (no default): %+v", spec)
	}
}

func TestParseHCLMultiKind(t *testing.T) {
	src := `
deployment "test" "api" {
  image = "a:1"
}
database "main" {
  engine = "postgres"
}
ingress "test" "api" {
  host    = "api.example.com"
  service = "api"
}
service "web" {
  target = "api"
}
`
	tmp := writeTemp(t, "stack.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 4 {
		t.Fatalf("want 4 manifests, got %d", len(mans))
	}
}

func TestParseYAMLMultiDoc(t *testing.T) {
	src := `
---
kind: deployment
scope: test
name: api
spec:
  image: nginx:1
  replicas: 2
---
kind: database
name: main
spec:
  engine: postgres
`
	tmp := writeTemp(t, "stack.yaml", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 2 {
		t.Fatalf("want 2 manifests, got %d", len(mans))
	}

	if mans[0].Kind != controller.KindDeployment || mans[1].Kind != controller.KindDatabase {
		t.Errorf("unexpected kinds: %+v %+v", mans[0], mans[1])
	}
}

func TestParseDirMixedFormats(t *testing.T) {
	dir := t.TempDir()

	writeAt(t, filepath.Join(dir, "a.hcl"), `deployment "test" "api" { image = "x:1" }`)
	writeAt(t, filepath.Join(dir, "b.yaml"), "kind: database\nname: main\nspec:\n  engine: postgres\n")
	writeAt(t, filepath.Join(dir, "README.md"), "ignored")

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}

	writeAt(t, filepath.Join(sub, "c.yml"), "kind: service\nname: web\nspec:\n  target: api\n")

	mans, err := ParseDir(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(mans), mans)
	}
}

// All voodu-branded extensions parse as HCL. Keeps the extension set
// from silently drifting out of sync between formatFromExt and the
// parseHCL synthesized path.
func TestParseFileVooduExtensions(t *testing.T) {
	src := `deployment "test" "api" { image = "nginx:1" }`

	for _, ext := range []string{".hcl", ".voodu", ".vdu", ".vd"} {
		t.Run(ext, func(t *testing.T) {
			tmp := writeTemp(t, "web"+ext, src)

			mans, err := ParseFile(tmp, nil)
			if err != nil {
				t.Fatalf("ParseFile(%s): %v", tmp, err)
			}

			if len(mans) != 1 || mans[0].Name != "api" {
				t.Errorf("unexpected: %+v", mans)
			}
		})
	}
}

func TestParseReaderStdin(t *testing.T) {
	src := `deployment "test" "api" { image = "nginx:1" }`

	mans, err := ParseReader(strings.NewReader(src), FormatHCL, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 1 || mans[0].Name != "api" {
		t.Errorf("unexpected: %+v", mans)
	}
}

func TestParseJSONRoundTrip(t *testing.T) {
	src := `
deployment "test" "api" {
  image = "nginx:1.27"
  replicas = 2
}
`

	vars := map[string]string{}

	mans, err := ParseReader(strings.NewReader(src), FormatHCL, vars)
	if err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(mans)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ParseReader(strings.NewReader(string(body)), FormatJSON, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Kind != controller.KindDeployment || got[0].Name != "api" {
		t.Fatalf("round-trip changed shape: %+v", got)
	}

	if string(got[0].Spec) != string(mans[0].Spec) {
		t.Errorf("spec mismatch:\n  got:  %s\n  want: %s", got[0].Spec, mans[0].Spec)
	}
}

func TestParseJSONSkipsInterpolation(t *testing.T) {
	// JSON payloads are already-parsed manifests; ${VAR} inside a string
	// value must be preserved verbatim (it's legitimate content, not a
	// variable reference).
	src := `[{"kind":"deployment","scope":"test","name":"api","spec":{"image":"nginx","env":{"URL":"${STILL_HERE}"}}}]`

	got, err := ParseReader(strings.NewReader(src), FormatJSON, map[string]string{"STILL_HERE": "expanded"})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d manifests", len(got))
	}

	if !strings.Contains(string(got[0].Spec), "${STILL_HERE}") {
		t.Errorf("JSON path ran interpolation, spec was: %s", got[0].Spec)
	}
}

func TestParseRejectsUnknownKind(t *testing.T) {
	src := "kind: potato\nname: x\nspec: {}\n"

	tmp := writeTemp(t, "bad.yaml", src)

	_, err := ParseFile(tmp, nil)
	if err == nil || !strings.Contains(err.Error(), "potato") {
		t.Errorf("want unknown-kind error, got %v", err)
	}
}

func TestParseUnknownExtension(t *testing.T) {
	tmp := writeTemp(t, "bad.json", "{}")

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("want error for unknown extension")
	}
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), name)
	writeAt(t, p, content)

	return p
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
