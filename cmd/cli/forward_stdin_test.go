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

	if got.needsSourcePush {
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

	stack := `deployment "api" {
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
	if got.needsSourcePush {
		t.Errorf("registry-mode deployment must not trigger source push")
	}
}

func TestRewriteForStdinStream_BuildModeFlagsSourcePush(t *testing.T) {
	// Two deployments in one file: one registry-mode, one build-mode.
	// Any build-mode manifest means apply has to `git push` before
	// POSTing manifests — that's the Option B source transport.
	dir := t.TempDir()
	path := filepath.Join(dir, "stack.hcl")

	stack := `deployment "web" {
  image    = "nginx:1.27"
  replicas = 1
}

deployment "api" {
  workdir    = "apps/api"
  lang       = "go"
  go_version = "1.25"
  ports      = ["3000"]
}
`

	if err := os.WriteFile(path, []byte(stack), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := rewriteForStdinStream([]string{"apply", "-f", path, "-a", "prod"})
	if err != nil {
		t.Fatal(err)
	}

	if !got.needsSourcePush {
		t.Errorf("manifest with a build-mode deployment must flag source push")
	}

	// diff and delete must NEVER trigger a push, even if the manifest is
	// build-mode — they don't need source on the server.
	for _, cmd := range []string{"diff", "delete"} {
		r, err := rewriteForStdinStream([]string{cmd, "-f", path, "-a", "prod"})
		if err != nil {
			t.Fatal(err)
		}

		if r.needsSourcePush {
			t.Errorf("%s must never flag source push", cmd)
		}
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
