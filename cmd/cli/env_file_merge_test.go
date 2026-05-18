package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

// TestParseEnvFile pins the .env parser shape: KEY=value lines, blank
// + comment lines skipped, quoted values strip outer quotes and
// preserve inner whitespace, `export` prefix tolerated.
func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	content := `
# this is a comment

DATABASE_URL=postgres://localhost:5432/db
REDIS_URL="redis://cache:6379"
SECRET='p@ss"with"quotes'
EMPTY=
SPACES_TRIMMED  =   hello world
QUOTED_SPACES = "  preserved  "
export NODE_ENV=production
`

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	want := map[string]string{
		"DATABASE_URL":   "postgres://localhost:5432/db",
		"REDIS_URL":      "redis://cache:6379",
		"SECRET":         `p@ss"with"quotes`,
		"EMPTY":          "",
		"SPACES_TRIMMED": "hello world",
		"QUOTED_SPACES":  "  preserved  ",
		"NODE_ENV":       "production",
	}

	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}

	if len(got) != len(want) {
		t.Errorf("count mismatch: got %d, want %d (%+v)", len(got), len(want), got)
	}
}

// TestParseEnvFile_MissingEqualsIsError pins the no-equals case: we
// reject loudly rather than silently dropping the line. Operator
// sees the line number and the offending text.
func TestParseEnvFile_MissingEqualsIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	if err := os.WriteFile(path, []byte("BADLINE no equals\n"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := parseEnvFile(path)
	if err == nil {
		t.Fatal("expected error on missing equals sign")
	}
}

// TestMergeEnvFilesInManifest_InlineWins pins the precedence rule:
// inline `env = {...}` always trumps `env_file` values. Operator who
// declared both gets the inline as the effective value.
func TestMergeEnvFilesInManifest_InlineWins(t *testing.T) {
	dir := t.TempDir()

	envFile := filepath.Join(dir, "app.env")
	if err := os.WriteFile(envFile, []byte("FOO=from-file\nBAR=only-in-file\n"), 0644); err != nil {
		t.Fatal(err)
	}

	specJSON, _ := json.Marshal(map[string]any{
		"image":    "nginx",
		"env_file": []string{"app.env"},
		"env":      map[string]string{"FOO": "inline-wins"},
	})

	m := controller.Manifest{
		Kind:  controller.KindDeployment,
		Scope: "test",
		Name:  "web",
		Spec:  specJSON,
	}

	out, err := mergeEnvFilesInManifest(m, dir)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Spec, &got); err != nil {
		t.Fatal(err)
	}

	env, _ := got["env"].(map[string]any)
	if env["FOO"] != "inline-wins" {
		t.Errorf("FOO: got %v, want %q (inline must win)", env["FOO"], "inline-wins")
	}

	if env["BAR"] != "only-in-file" {
		t.Errorf("BAR: got %v, want %q (file value should land when inline absent)", env["BAR"], "only-in-file")
	}

	if _, present := got["env_file"]; present {
		t.Error("env_file should be stripped from wire spec (client-side concern)")
	}
}

// TestMergeEnvFilesInManifest_OrderedLastWins pins that multiple
// env_file entries layer in declared order — later files override
// earlier ones. Matches docker-compose's list semantics.
func TestMergeEnvFilesInManifest_OrderedLastWins(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "base.env"), []byte("FOO=base\nSHARED=base\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "override.env"), []byte("SHARED=override\n"), 0644); err != nil {
		t.Fatal(err)
	}

	specJSON, _ := json.Marshal(map[string]any{
		"image":    "nginx",
		"env_file": []string{"base.env", "override.env"},
	})

	m := controller.Manifest{Kind: controller.KindDeployment, Scope: "t", Name: "w", Spec: specJSON}

	out, err := mergeEnvFilesInManifest(m, dir)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	_ = json.Unmarshal(out.Spec, &got)

	env := got["env"].(map[string]any)

	if env["FOO"] != "base" {
		t.Errorf("FOO: got %v, want %q", env["FOO"], "base")
	}

	if env["SHARED"] != "override" {
		t.Errorf("SHARED: got %v, want %q (later env_file should win)", env["SHARED"], "override")
	}
}

// TestMergeEnvFilesInManifest_CronJobNested pins the cronjob shape:
// env_file lives inside the `job` sub-object on the wire, not flat
// at root. The helper must recurse into job.{} to find it.
func TestMergeEnvFilesInManifest_CronJobNested(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "cron.env"), []byte("HEARTBEAT_URL=https://hc.io/x\n"), 0644); err != nil {
		t.Fatal(err)
	}

	specJSON, _ := json.Marshal(map[string]any{
		"schedule": "0 * * * *",
		"job": map[string]any{
			"image":    "cron-runner:latest",
			"env_file": []string{"cron.env"},
		},
	})

	m := controller.Manifest{Kind: controller.KindCronJob, Scope: "t", Name: "beat", Spec: specJSON}

	out, err := mergeEnvFilesInManifest(m, dir)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	_ = json.Unmarshal(out.Spec, &got)

	job := got["job"].(map[string]any)
	env := job["env"].(map[string]any)

	if env["HEARTBEAT_URL"] != "https://hc.io/x" {
		t.Errorf("HEARTBEAT_URL: got %v", env["HEARTBEAT_URL"])
	}

	if _, present := job["env_file"]; present {
		t.Error("env_file should be stripped from cronjob job sub-object")
	}
}

// TestMergeEnvFilesInManifest_MissingFileIsError ensures the
// operator sees a hard error for a typo'd path rather than silently
// running with no env vars from the file.
func TestMergeEnvFilesInManifest_MissingFileIsError(t *testing.T) {
	dir := t.TempDir()

	specJSON, _ := json.Marshal(map[string]any{
		"image":    "nginx",
		"env_file": []string{"missing.env"},
	})

	m := controller.Manifest{Kind: controller.KindDeployment, Scope: "t", Name: "w", Spec: specJSON}

	_, err := mergeEnvFilesInManifest(m, dir)
	if err == nil {
		t.Fatal("expected error on missing env file")
	}
}

// TestMergeEnvFilesInManifest_AbsolutePathSupported pins that absolute
// env_file paths bypass the baseDir join — useful for shared secrets
// living at a known system-wide location.
func TestMergeEnvFilesInManifest_AbsolutePathSupported(t *testing.T) {
	dir := t.TempDir()

	abs := filepath.Join(dir, "absolute.env")
	if err := os.WriteFile(abs, []byte("ABS=ok\n"), 0644); err != nil {
		t.Fatal(err)
	}

	specJSON, _ := json.Marshal(map[string]any{
		"image":    "nginx",
		"env_file": []string{abs},
	})

	m := controller.Manifest{Kind: controller.KindDeployment, Scope: "t", Name: "w", Spec: specJSON}

	// Pass a baseDir that DOESN'T contain the file to prove the abs
	// path was taken verbatim rather than re-joined.
	otherDir := t.TempDir()

	out, err := mergeEnvFilesInManifest(m, otherDir)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	_ = json.Unmarshal(out.Spec, &got)

	env := got["env"].(map[string]any)
	if env["ABS"] != "ok" {
		t.Errorf("ABS: got %v", env["ABS"])
	}
}

// TestMergeEnvFilesInManifests_SkipsIngress pins that non-env-bearing
// kinds (ingress, asset, plugin blocks) pass through unchanged — the
// helper shouldn't try to mutate manifests that have no env field.
func TestMergeEnvFilesInManifests_SkipsIngress(t *testing.T) {
	specJSON, _ := json.Marshal(map[string]any{"host": "example.com"})

	in := []controller.Manifest{{
		Kind:  controller.KindIngress,
		Scope: "t",
		Name:  "web",
		Spec:  specJSON,
	}}

	out, err := mergeEnvFilesInManifests(in, "")
	if err != nil {
		t.Fatal(err)
	}

	if string(out[0].Spec) != string(specJSON) {
		t.Errorf("ingress spec mutated: %s", out[0].Spec)
	}
}
