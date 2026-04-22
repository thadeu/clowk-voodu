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

func TestLoadFromDirGokkuStyleNoYAML(t *testing.T) {
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
		t.Errorf("gokku name fallback failed: got %q", p.Manifest.Name)
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
