package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSource(t *testing.T) {
	tmp := t.TempDir()

	cases := []struct {
		name string
		in   string
		kind SourceKind
	}{
		{"absolute path", "/tmp/whatever", SourceLocal},
		{"relative ./", "./foo", SourceLocal},
		{"existing local dir", tmp, SourceLocal},
		{"github shorthand", "owner/repo", SourceGit},
		{"github.com prefix", "github.com/owner/repo", SourceGit},
		{"https url", "https://gitlab.com/o/r.git", SourceGit},
		{"ssh url", "git@github.com:o/r.git", SourceGit},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseSource(c.in)
			if err != nil {
				t.Fatal(err)
			}

			if got.Kind != c.kind {
				t.Errorf("got %s, want %s", got.Kind, c.kind)
			}
		})
	}
}

func TestNormaliseGitURL(t *testing.T) {
	cases := map[string]string{
		"owner/repo":                         "https://github.com/owner/repo",
		"github.com/owner/repo":              "https://github.com/owner/repo",
		"https://gitlab.com/a/b.git":         "https://gitlab.com/a/b.git",
		"git@github.com:owner/repo.git":      "git@github.com:owner/repo.git",
	}

	for in, want := range cases {
		if got := normaliseGitURL(in); got != want {
			t.Errorf("normaliseGitURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInstallerInstallsLocalPlugin(t *testing.T) {
	src := t.TempDir()

	if err := os.MkdirAll(filepath.Join(src, "commands"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(src, "plugin.yml"),
		[]byte("name: hello\nversion: 0.1.0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(src, "commands", "say"),
		[]byte("#!/bin/bash\necho \"hello $1\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	inst := &Installer{Root: root}

	p, err := inst.Install(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}

	if p.Manifest.Name != "hello" {
		t.Errorf("name: %q", p.Manifest.Name)
	}

	want := filepath.Join(root, "hello")
	if p.Dir != want {
		t.Errorf("dir: got %q, want %q", p.Dir, want)
	}

	info, err := os.Stat(filepath.Join(want, "commands", "say"))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm()&0111 == 0 {
		t.Errorf("command not executable: %v", info.Mode())
	}
}

func TestInstallerReplacesExisting(t *testing.T) {
	root := t.TempDir()
	inst := &Installer{Root: root}

	makeSource := func(t *testing.T, version string) string {
		dir := t.TempDir()

		if err := os.MkdirAll(filepath.Join(dir, "commands"), 0755); err != nil {
			t.Fatal(err)
		}

		_ = os.WriteFile(filepath.Join(dir, "plugin.yml"),
			[]byte("name: hello\nversion: "+version+"\n"), 0644)
		_ = os.WriteFile(filepath.Join(dir, "commands", "say"),
			[]byte("#!/bin/bash\n"), 0644)

		return dir
	}

	_, err := inst.Install(context.Background(), makeSource(t, "1"))
	if err != nil {
		t.Fatal(err)
	}

	p2, err := inst.Install(context.Background(), makeSource(t, "2"))
	if err != nil {
		t.Fatal(err)
	}

	if p2.Manifest.Version != "2" {
		t.Errorf("version: %q", p2.Manifest.Version)
	}
}

func TestInstallerRemoveMissing(t *testing.T) {
	inst := &Installer{Root: t.TempDir()}

	ok, err := inst.Remove(context.Background(), "ghost")
	if err != nil {
		t.Fatal(err)
	}

	if ok {
		t.Error("want ok=false for missing plugin")
	}
}

func TestInstallerRemoveExisting(t *testing.T) {
	src := t.TempDir()

	_ = os.MkdirAll(filepath.Join(src, "commands"), 0755)
	_ = os.WriteFile(filepath.Join(src, "plugin.yml"), []byte("name: gone\n"), 0644)
	_ = os.WriteFile(filepath.Join(src, "commands", "x"), []byte("#!/bin/bash\n"), 0644)

	root := t.TempDir()
	inst := &Installer{Root: root}

	if _, err := inst.Install(context.Background(), src); err != nil {
		t.Fatal(err)
	}

	ok, err := inst.Remove(context.Background(), "gone")
	if err != nil {
		t.Fatal(err)
	}

	if !ok {
		t.Error("want ok=true after installing")
	}

	if _, err := os.Stat(filepath.Join(root, "gone")); !os.IsNotExist(err) {
		t.Errorf("plugin dir still present: %v", err)
	}
}
