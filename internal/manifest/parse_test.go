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
deployment "api" {
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
	// Mirrors the flat "voodu.yml-style" attribute set: no image, a
	// workdir/dockerfile/path pointing at a subtree, lang hints, and a
	// post_deploy hook. The resolution rule (image → registry vs. path →
	// build) is enforced by the handler — the parser just has to carry
	// every field through cleanly.
	src := `
deployment "api" {
  workdir      = "apps/esl"
  dockerfile   = "Dockerfile.api"
  path         = "cmd/api"
  lang         = "go"
  go_version   = "1.25"
  ports        = ["127.0.0.1:9092:9092"]
  volumes      = ["/opt/gokku/volumes/rtp:/app/recordings"]
  network      = "bridge"
  restart      = "unless-stopped"
  env          = { RAILS_ENV = "production" }
  post_deploy  = ["./bin/migrate"]
  health_check = "/healthz"
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

	if spec.Lang != "go" || spec.GoVersion != "1.25" {
		t.Errorf("lang hints lost: lang=%q go_version=%q", spec.Lang, spec.GoVersion)
	}

	if len(spec.PostDeploy) != 1 || spec.PostDeploy[0] != "./bin/migrate" {
		t.Errorf("post_deploy: %+v", spec.PostDeploy)
	}

	if spec.HealthCheck != "/healthz" || spec.Restart != "unless-stopped" || spec.Network != "bridge" {
		t.Errorf("runtime fields: %+v", spec)
	}
}

func TestParseHCLDeploymentImageOptional(t *testing.T) {
	// Minimal build-mode deployment: no image, no path either. Handler
	// will treat this as "build from repo root" (legacy gokku deploy).
	src := `deployment "api" {}`

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

	if spec.Image != "" || spec.Path != "" || spec.Workdir != "" {
		t.Errorf("bare deployment should have empty source fields: %+v", spec)
	}
}

func TestParseHCLMultiKind(t *testing.T) {
	src := `
deployment "api" {
  image = "a:1"
}
database "main" {
  engine = "postgres"
}
ingress "api" {
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

	writeAt(t, filepath.Join(dir, "a.hcl"), `deployment "api" { image = "x:1" }`)
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

func TestParseReaderStdin(t *testing.T) {
	src := `deployment "api" { image = "nginx:1" }`

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
deployment "api" {
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
	src := `[{"kind":"deployment","name":"api","spec":{"image":"nginx","env":{"URL":"${STILL_HERE}"}}}]`

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
