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
