package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromDirWithPluginYML(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "plugin.yml"), `
name: postgres
version: 0.1.0
description: Manage Postgres instances
commands:
  - name: create
    help: Provision a new instance
env:
  POSTGRES_DEFAULT_VERSION: "16"
`)

	mkdir(t, filepath.Join(dir, "commands"))
	writeFile(t, filepath.Join(dir, "commands", "create"), "#!/bin/bash\necho hi\n")
	os.Chmod(filepath.Join(dir, "commands", "create"), 0755)

	p, err := LoadFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if p.Manifest.Name != "postgres" {
		t.Errorf("name: got %q", p.Manifest.Name)
	}

	if p.Manifest.Env["POSTGRES_DEFAULT_VERSION"] != "16" {
		t.Errorf("env not loaded: %+v", p.Manifest.Env)
	}

	if _, ok := p.Commands["create"]; !ok {
		t.Errorf("create command not discovered: %+v", p.Commands)
	}
}

func TestLoadFromDirBareCommandsNoYAML(t *testing.T) {
	dir := t.TempDir()

	mkdir(t, filepath.Join(dir, "commands"))
	writeFile(t, filepath.Join(dir, "commands", "name"), "#!/bin/bash\necho redis\n")
	writeFile(t, filepath.Join(dir, "commands", "info"), "#!/bin/bash\necho info\n")
	writeFile(t, filepath.Join(dir, "commands", "help"), "#!/bin/bash\necho help\n")

	p, err := LoadFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if p.Manifest.Name != "redis" {
		t.Errorf("bare commands/name fallback failed: got %q", p.Manifest.Name)
	}

	for _, want := range []string{"name", "info", "help"} {
		if _, ok := p.Commands[want]; !ok {
			t.Errorf("command %s not discovered", want)
		}
	}
}

func TestLoadFromDirBinWinsOverCommands(t *testing.T) {
	dir := t.TempDir()

	mkdir(t, filepath.Join(dir, "commands"))
	mkdir(t, filepath.Join(dir, "bin"))

	writeFile(t, filepath.Join(dir, "commands", "create"), "old")
	writeFile(t, filepath.Join(dir, "bin", "create"), "new")

	p, err := LoadFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	got := p.Commands["create"]
	if filepath.Base(filepath.Dir(got)) != "bin" {
		t.Errorf("expected bin/ to win, got path %q", got)
	}
}

func TestLoadFromDirNameFallbackToDirBasename(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "caddy")

	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	p, err := LoadFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if p.Manifest.Name != "caddy" {
		t.Errorf("basename fallback: got %q", p.Manifest.Name)
	}
}

func TestLoadAllSkipsBadPlugins(t *testing.T) {
	root := t.TempDir()

	good := filepath.Join(root, "hello")
	mkdir(t, filepath.Join(good, "commands"))
	writeFile(t, filepath.Join(good, "commands", "say"), "#!/bin/bash\necho hi\n")

	bad := filepath.Join(root, "bad")
	mkdir(t, bad)
	writeFile(t, filepath.Join(bad, "plugin.yml"), "not: valid: yaml: [")

	plugins, errs := LoadAll(root)

	if len(plugins) != 1 || plugins[0].Manifest.Name != "hello" {
		t.Errorf("expected 1 good plugin 'hello', got %+v", plugins)
	}

	if len(errs) != 1 {
		t.Errorf("expected 1 error for bad plugin, got %+v", errs)
	}
}

func TestLoadByName_DirectoryMatchFastPath(t *testing.T) {
	// Canonical case: <root>/postgres exists → LoadByName returns
	// it without scanning sibling directories.
	root := t.TempDir()

	pgDir := filepath.Join(root, "postgres")
	mkdir(t, pgDir)

	writeFile(t, filepath.Join(pgDir, "plugin.yml"), `
name: postgres
aliases: [pg]
`)

	mkdir(t, filepath.Join(pgDir, "bin"))
	writeFile(t, filepath.Join(pgDir, "bin", "expand"), "#!/bin/bash\n")

	p, err := LoadByName(root, "postgres")
	if err != nil {
		t.Fatal(err)
	}

	if p.Manifest.Name != "postgres" {
		t.Errorf("got name %q", p.Manifest.Name)
	}
}

func TestLoadByName_AliasFallbackScan(t *testing.T) {
	// `vd pg:psql` lookup: /opt/voodu/plugins/pg/ doesn't exist,
	// but /opt/voodu/plugins/postgres/plugin.yml has
	// aliases: [pg]. LoadByName must scan + match.
	root := t.TempDir()

	pgDir := filepath.Join(root, "postgres")
	mkdir(t, pgDir)

	writeFile(t, filepath.Join(pgDir, "plugin.yml"), `
name: postgres
aliases: [pg, postgresql]
`)

	mkdir(t, filepath.Join(pgDir, "bin"))
	writeFile(t, filepath.Join(pgDir, "bin", "psql"), "#!/bin/bash\n")

	for _, alias := range []string{"pg", "postgresql"} {
		p, err := LoadByName(root, alias)
		if err != nil {
			t.Errorf("alias %q: %v", alias, err)
			continue
		}

		if p.Manifest.Name != "postgres" {
			t.Errorf("alias %q resolved to wrong plugin %q", alias, p.Manifest.Name)
		}
	}
}

func TestLoadByName_NotFound(t *testing.T) {
	root := t.TempDir()

	pgDir := filepath.Join(root, "postgres")
	mkdir(t, pgDir)
	writeFile(t, filepath.Join(pgDir, "plugin.yml"), "name: postgres\n")
	mkdir(t, filepath.Join(pgDir, "bin"))

	_, err := LoadByName(root, "nonexistent")
	if err == nil {
		t.Fatal("expected error when neither dir match nor alias match")
	}
}

// TestLoadFromDir_EntrypointModeRoutesAllCommandsToOneBinary pins
// the entrypoint convention: when plugin.yml declares
// `entrypoint: bin/<plugin>`, every command in the manifest
// resolves to that single binary. No per-command bin/<cmd> shims
// needed.
func TestLoadFromDir_EntrypointModeRoutesAllCommandsToOneBinary(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "plugin.yml"), `
name: postgres
entrypoint: bin/voodu-postgres
commands:
  - name: info
  - name: backups
  - name: backups:capture
  - name: backups:cancel
`)

	mkdir(t, filepath.Join(dir, "bin"))
	writeFile(t, filepath.Join(dir, "bin", "voodu-postgres"), "#!/bin/bash\necho ok\n")
	os.Chmod(filepath.Join(dir, "bin", "voodu-postgres"), 0755)

	p, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	wantPath := filepath.Join(dir, "bin", "voodu-postgres")

	for _, cmd := range []string{"info", "backups", "backups:capture", "backups:cancel"} {
		got, ok := p.Commands[cmd]
		if !ok {
			t.Errorf("command %q not registered: %v", cmd, p.Commands)
			continue
		}

		if got != wantPath {
			t.Errorf("command %q resolved to %q, want %q", cmd, got, wantPath)
		}
	}
}

// TestLoadFromDir_EntrypointModeIgnoresStalePerCommandBins pins
// the priority: when entrypoint is set, lingering bin/<command>
// files (e.g. from a half-migrated plugin) are ignored. Otherwise
// the loader could route the same command to two different
// executables non-deterministically.
func TestLoadFromDir_EntrypointModeIgnoresStalePerCommandBins(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "plugin.yml"), `
name: demo
entrypoint: bin/demo
commands:
  - name: foo
`)

	mkdir(t, filepath.Join(dir, "bin"))
	writeFile(t, filepath.Join(dir, "bin", "demo"), "#!/bin/bash\necho hi\n")
	writeFile(t, filepath.Join(dir, "bin", "foo"), "#!/bin/bash\necho stale\n")
	os.Chmod(filepath.Join(dir, "bin", "demo"), 0755)
	os.Chmod(filepath.Join(dir, "bin", "foo"), 0755)

	p, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	wantPath := filepath.Join(dir, "bin", "demo")
	if p.Commands["foo"] != wantPath {
		t.Errorf("entrypoint should win over stale bin/foo: got %q, want %q",
			p.Commands["foo"], wantPath)
	}
}

// TestLoadFromDir_EntrypointMissingFileTolerated pins the
// install-hook-friendly behaviour: LoadFromDir does NOT validate
// that the entrypoint file exists. Many plugins fetch their
// binary in the post-install lifecycle hook, which fires AFTER
// LoadFromDir. Strict validation here would forbid that pattern.
//
// Run() handles the missing-file case with a clear error at
// invocation time — see exec_test.go's
// TestRunEntrypointMissingBinaryErrors.
func TestLoadFromDir_EntrypointMissingFileTolerated(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "plugin.yml"), `
name: demo
entrypoint: bin/missing-binary
commands:
  - name: foo
`)

	p, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("LoadFromDir should tolerate missing entrypoint (install hook fetches it): %v", err)
	}

	// The Commands map still resolves to the (not-yet-existing)
	// path so Run can produce a clear error later.
	if got := p.Commands["foo"]; got != filepath.Join(dir, "bin", "missing-binary") {
		t.Errorf("entrypoint should still resolve in Commands map: got %q", got)
	}
}

// TestLoadFromDir_NoEntrypointKeepsLegacyBehaviour confirms
// back-compat: plugins without entrypoint still get per-command
// bin/<command> resolution, unchanged from before this feature.
func TestLoadFromDir_NoEntrypointKeepsLegacyBehaviour(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "plugin.yml"), `
name: legacy
commands:
  - name: foo
  - name: bar
`)

	mkdir(t, filepath.Join(dir, "bin"))
	writeFile(t, filepath.Join(dir, "bin", "foo"), "#!/bin/bash\necho foo\n")
	writeFile(t, filepath.Join(dir, "bin", "bar"), "#!/bin/bash\necho bar\n")
	os.Chmod(filepath.Join(dir, "bin", "foo"), 0755)
	os.Chmod(filepath.Join(dir, "bin", "bar"), 0755)

	p, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if p.Commands["foo"] != filepath.Join(dir, "bin", "foo") {
		t.Errorf("legacy: foo should resolve to bin/foo, got %q", p.Commands["foo"])
	}

	if p.Commands["bar"] != filepath.Join(dir, "bin", "bar") {
		t.Errorf("legacy: bar should resolve to bin/bar, got %q", p.Commands["bar"])
	}
}

func TestLoadByName_BrokenSiblingPluginDoesntBlockAliasMatch(t *testing.T) {
	// Defensive: one corrupted plugin.yml in the plugins root
	// shouldn't prevent an unrelated alias from resolving. The
	// fallback scan tolerates per-plugin load errors.
	root := t.TempDir()

	pgDir := filepath.Join(root, "postgres")
	mkdir(t, pgDir)
	writeFile(t, filepath.Join(pgDir, "plugin.yml"), "name: postgres\naliases: [pg]\n")
	mkdir(t, filepath.Join(pgDir, "bin"))

	// Broken plugin: invalid YAML.
	brokenDir := filepath.Join(root, "broken")
	mkdir(t, brokenDir)
	writeFile(t, filepath.Join(brokenDir, "plugin.yml"), "this is not: { valid yaml")
	mkdir(t, filepath.Join(brokenDir, "bin"))

	p, err := LoadByName(root, "pg")
	if err != nil {
		t.Fatalf("alias resolution should succeed despite sibling broken plugin: %v", err)
	}

	if p.Manifest.Name != "postgres" {
		t.Errorf("got %q", p.Manifest.Name)
	}
}

func TestLoadByName_EmptyArguments(t *testing.T) {
	if _, err := LoadByName("", "postgres"); err == nil {
		t.Error("expected error for empty root")
	}

	if _, err := LoadByName("/tmp", ""); err == nil {
		t.Error("expected error for empty name")
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
}
