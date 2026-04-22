package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServerConfigGetApp(t *testing.T) {
	cfg := &ServerConfig{
		Apps: map[string]App{
			"api": {Path: "/some/path"},
		},
	}

	app, err := cfg.GetApp("api")
	if err != nil {
		t.Fatalf("expected app, got err: %v", err)
	}

	if app.Name != "api" {
		t.Errorf("Name should be populated, got %q", app.Name)
	}

	if _, err := cfg.GetApp("missing"); err == nil {
		t.Error("missing app should error")
	}
}

func TestServerConfigValidate(t *testing.T) {
	empty := &ServerConfig{Apps: map[string]App{}}
	if err := empty.Validate(); err == nil {
		t.Error("empty config should fail validation")
	}

	missingPath := &ServerConfig{Apps: map[string]App{"api": {}}}
	if err := missingPath.Validate(); err == nil {
		t.Error("app without path or image should fail")
	}

	ok := &ServerConfig{Apps: map[string]App{"api": {Path: "/x"}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid config errored: %v", err)
	}
}

func TestLoadUserConfigEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := LoadUserConfig()
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Apps) != 0 {
		t.Errorf("expected empty apps, got %d", len(cfg.Apps))
	}
}

func TestSaveLoadUserConfigRoundtrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	in := &Config{Apps: map[string]App{"api": {Lang: "go"}}}

	if err := SaveUserConfig(in); err != nil {
		t.Fatalf("save: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(home, ".voodu", "config.yml"))
	if len(data) == 0 {
		t.Fatal("expected file to be written")
	}

	out, err := LoadUserConfig()
	if err != nil {
		t.Fatal(err)
	}

	if out.Apps["api"].Lang != "go" {
		t.Errorf("roundtrip lost Lang: %+v", out)
	}
}
