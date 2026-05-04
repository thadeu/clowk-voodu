package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

func TestRewriteForStdinStream_NoRewriteForNonManifestCmd(t *testing.T) {
	args := []string{"config", "set", "FOO=bar", "-a", "api"}

	got, err := rewriteForStdinStream(args)
	if err != nil {
		t.Fatal(err)
	}

	if got.stdin != nil {
		t.Errorf("expected nil stdin for non-manifest command, got %v", got.stdin)
	}

	if !reflect.DeepEqual(got.args, args) {
		t.Errorf("args mutated: got %v, want %v", got.args, args)
	}

	if len(got.buildModeDeploys) > 0 {
		t.Errorf("non-manifest command must never flag a source push")
	}
}

func TestRewriteForStdinStream_NoFilesNoStdin(t *testing.T) {
	// `apply` with no -f — let the remote emit its own error.
	args := []string{"apply"}

	got, err := rewriteForStdinStream(args)
	if err != nil {
		t.Fatal(err)
	}

	if got.stdin != nil {
		t.Errorf("expected no stdin when no -f given")
	}

	if !reflect.DeepEqual(got.args, args) {
		t.Errorf("args mutated: got %v", got.args)
	}
}

func TestRewriteForStdinStream_StdinPassthrough(t *testing.T) {
	args := []string{"apply", "-f", "-", "--format", "yaml"}

	got, err := rewriteForStdinStream(args)
	if err != nil {
		t.Fatal(err)
	}

	if got.stdin != nil {
		t.Errorf("user-supplied stdin must not be swapped out")
	}

	if !reflect.DeepEqual(got.args, args) {
		t.Errorf("args mutated: got %v, want %v", got.args, args)
	}
}

func TestRewriteForStdinStream_ReadsFileAndEmitsJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stack.hcl")

	stack := `deployment "test" "api" {
  image = "nginx:1.27"
  replicas = 2
}
`

	if err := os.WriteFile(path, []byte(stack), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := rewriteForStdinStream([]string{"apply", "-f", path, "-a", "api"})
	if err != nil {
		t.Fatal(err)
	}

	if got.stdin == nil {
		t.Fatal("expected stdin reader")
	}

	// New argv should retain -a api, drop the local path, and carry the
	// explicit stdin/json directive.
	want := []string{"apply", "-a", "api", "-f", "-", "--format", "json"}
	if !reflect.DeepEqual(got.args, want) {
		t.Errorf("argv:\n  got:  %v\n  want: %v", got.args, want)
	}

	body, err := io.ReadAll(got.stdin)
	if err != nil {
		t.Fatal(err)
	}

	var mans []controller.Manifest
	if err := json.Unmarshal(body, &mans); err != nil {
		t.Fatalf("stdin not valid JSON: %v\npayload: %s", err, body)
	}

	if len(mans) != 1 || mans[0].Kind != controller.KindDeployment || mans[0].Name != "api" {
		t.Errorf("unexpected manifests: %+v", mans)
	}

	// Deployment declares `image = "nginx:1.27"` — registry mode, so no
	// source push should be signalled.
	if len(got.buildModeDeploys) > 0 {
		t.Errorf("registry-mode deployment must not trigger source push")
	}
}

func TestRewriteForStdinStream_BuildModeFlagsSourcePush(t *testing.T) {
	// Two deployments in one file: one registry-mode, one build-mode.
	// Any build-mode manifest means apply has to stream its source via
	// `voodu receive-pack` before POSTing manifests so the controller
	// has an image to reference.
	dir := t.TempDir()
	path := filepath.Join(dir, "stack.hcl")

	stack := `deployment "prod" "web" {
  image    = "nginx:1.27"
  replicas = 1
}

deployment "prod" "api" {
  workdir = "apps/api"
  ports   = ["3000"]

  lang {
    name    = "go"
    version = "1.25"
  }
}
`

	if err := os.WriteFile(path, []byte(stack), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := rewriteForStdinStream([]string{"apply", "-f", path, "-a", "prod"})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.buildModeDeploys) != 1 {
		t.Fatalf("expected 1 build-mode deploy, got %d: %+v", len(got.buildModeDeploys), got.buildModeDeploys)
	}

	bm := got.buildModeDeploys[0]
	if bm.Scope != "prod" || bm.Name != "api" {
		t.Errorf("build-mode deploy ref = %s/%s, want prod/api", bm.Scope, bm.Name)
	}

	if bm.Path != "." {
		t.Errorf("build-mode deploy path = %q, want %q (default for root-context)", bm.Path, ".")
	}

	// diff and delete must NEVER trigger a push, even if the manifest is
	// build-mode — they don't need source on the server.
	for _, cmd := range []string{"diff", "delete"} {
		r, err := rewriteForStdinStream([]string{cmd, "-f", path, "-a", "prod"})
		if err != nil {
			t.Fatal(err)
		}

		if len(r.buildModeDeploys) > 0 {
			t.Errorf("%s must never flag source push", cmd)
		}
	}
}

func TestRewriteForStdinStream_BuildModeStatefulset(t *testing.T) {
	// Statefulset with no Image triggers the same source-push pipeline
	// as a build-mode deployment. Use case: postgres + pgvector built
	// inline from a Dockerfile that does `FROM postgres:16` + apt-get.
	dir := t.TempDir()
	path := filepath.Join(dir, "stack.hcl")

	stack := `statefulset "prod" "db" {
  workdir    = "infra/postgres"
  dockerfile = "Dockerfile.pg"
  replicas   = 1
}
`

	if err := os.WriteFile(path, []byte(stack), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := rewriteForStdinStream([]string{"apply", "-f", path, "-a", "prod"})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.buildModeDeploys) != 1 {
		t.Fatalf("expected 1 build-mode statefulset, got %d: %+v", len(got.buildModeDeploys), got.buildModeDeploys)
	}

	bm := got.buildModeDeploys[0]
	if bm.Scope != "prod" || bm.Name != "db" {
		t.Errorf("ref = %s/%s, want prod/db", bm.Scope, bm.Name)
	}

	// applyDefaults should fill Path = "." for build-mode statefulset
	// (no explicit path = root context, like deployment).
	if bm.Path != "." {
		t.Errorf("path = %q, want %q (applyDefaults should fill root)", bm.Path, ".")
	}
}

func TestRewriteForStdinStream_RegistryModeStatefulsetSkipsPush(t *testing.T) {
	// Statefulset with explicit image — must NOT trigger source push.
	// Mirror of the deployment registry-mode test.
	dir := t.TempDir()
	path := filepath.Join(dir, "stack.hcl")

	stack := `statefulset "prod" "redis" {
  image    = "redis:8"
  replicas = 1
}
`

	if err := os.WriteFile(path, []byte(stack), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := rewriteForStdinStream([]string{"apply", "-f", path, "-a", "prod"})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.buildModeDeploys) > 0 {
		t.Errorf("registry-mode statefulset must not trigger source push, got %+v", got.buildModeDeploys)
	}
}

func TestRewriteForStdinStream_BuildModeDeploymentAndStatefulset(t *testing.T) {
	// Mixed file: one deployment + one statefulset, both build-mode.
	// Both should appear in buildModeDeploys (same source-push pipeline
	// services both kinds).
	dir := t.TempDir()
	path := filepath.Join(dir, "stack.hcl")

	stack := `deployment "prod" "api" {
  ports = ["3000"]

  lang {
    name    = "go"
    version = "1.25"
  }
}

statefulset "prod" "db" {
  dockerfile = "Dockerfile.pg"
}
`

	if err := os.WriteFile(path, []byte(stack), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := rewriteForStdinStream([]string{"apply", "-f", path, "-a", "prod"})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.buildModeDeploys) != 2 {
		t.Fatalf("expected 2 build-mode entries (deployment + statefulset), got %d: %+v", len(got.buildModeDeploys), got.buildModeDeploys)
	}

	names := map[string]bool{}
	for _, bm := range got.buildModeDeploys {
		names[bm.Name] = true
	}

	if !names["api"] || !names["db"] {
		t.Errorf("expected both 'api' (deployment) and 'db' (statefulset) flagged, got %v", names)
	}
}

func TestSplitFileAndFormatFlags(t *testing.T) {
	paths, format, rest := splitFileAndFormatFlags([]string{
		"apply", "-f", "a.hcl", "--file", "b.yml", "--format=yaml", "-a", "api",
	})

	if !reflect.DeepEqual(paths, []string{"a.hcl", "b.yml"}) {
		t.Errorf("paths: %v", paths)
	}

	if format != "yaml" {
		t.Errorf("format: %q", format)
	}

	if !reflect.DeepEqual(rest, []string{"apply", "-a", "api"}) {
		t.Errorf("rest: %v", rest)
	}
}

func TestFindPrimaryCommand(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"apply", "-f", "x"}, 0},
		{[]string{"-o", "json", "apply"}, 2},
		{[]string{"--output=json", "config", "set"}, 1},
		{[]string{"-a", "api", "logs"}, 2},
		{[]string{}, -1},
		{[]string{"--help"}, -1},
	}

	for _, c := range cases {
		if got := findPrimaryCommand(c.args); got != c.want {
			t.Errorf("findPrimaryCommand(%v) = %d, want %d", c.args, got, c.want)
		}
	}
}
