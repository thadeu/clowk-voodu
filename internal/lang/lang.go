// Package lang implements per-language build/deploy strategies. Each type
// satisfies the Lang interface. Handlers consume a BuildSpec — a minimal
// struct carrying only what the build pipeline needs — so this package
// stays decoupled from the manifest / controller packages.
package lang

import (
	"fmt"
	"os"
	"path/filepath"
)

// BuildSpec carries every field the language handlers consume. It is the
// in-process contract between the server-side build pipeline and the
// language-specific Build/Deploy/Dockerfile generation code.
//
// Shape mirrors manifest.DeploymentSpec: a flat root of container-shape
// and source-resolution fields, plus an optional `Lang` block carrying
// build-time inputs for the chosen runtime.
//
// Populated in two places:
//
//   - receive-pack fetches the manifest DeploymentSpec from the
//     controller and converts it via deploy.Spec.toBuildSpec.
//   - auto-detect path: when the controller has no manifest yet,
//     NewLang sniffs the release directory and synthesises Lang.Name.
//
// Zero values mean "use the handler's default" — handlers must tolerate
// a nil Lang pointer and empty strings, treating them as a signal to
// auto-detect or pick a sane default.
type BuildSpec struct {
	Image      string
	Dockerfile string
	Path       string
	Workdir    string

	Env         map[string]string
	Ports       []string
	Volumes     []string
	NetworkMode string

	Lang *LangBuildSpec
}

// LangBuildSpec is the unified runtime block. Name picks the handler;
// Version/Entrypoint/BuildArgs are forwarded as-is. Handlers read what
// they need and ignore the rest.
type LangBuildSpec struct {
	Name       string
	Version    string
	Entrypoint string
	BuildArgs  map[string]string
}

// LangName returns the resolved runtime name, or "" if unset.
func (s *BuildSpec) LangName() string {
	if s == nil || s.Lang == nil {
		return ""
	}

	return s.Lang.Name
}

// setLangName seeds Lang.Name when auto-detect fires without a manifest
// block. Allocates the sub-struct lazily so handlers never see nil after
// NewLang has run.
func (s *BuildSpec) setLangName(name string) {
	if s.Lang == nil {
		s.Lang = &LangBuildSpec{}
	}

	s.Lang.Name = name
}

type Lang interface {
	Build(appName string, spec *BuildSpec, releaseDir string) error
	Deploy(appName string, spec *BuildSpec, releaseDir string) error
	Restart(appName string, spec *BuildSpec) error
	Cleanup(appName string, spec *BuildSpec) error
	DetectLanguage(releaseDir string) (string, error)
	EnsureDockerfile(releaseDir string, appName string, spec *BuildSpec) error
}

// DetectLanguage automatically detects the programming language based on project files.
func DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "Dockerfile")); err == nil {
		return "docker", nil
	}

	if lang := detectLanguageInDir(releaseDir); lang != "" {
		return lang, nil
	}

	if lang := detectLanguageRecursive(releaseDir, 2); lang != "" {
		return lang, nil
	}

	return "generic", nil
}

func detectLanguageInDir(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return "go"
	}

	if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		return "nodejs"
	}

	if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
		return "python"
	}

	if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err == nil {
		return "python"
	}

	if _, err := os.Stat(filepath.Join(dir, "config/application.rb")); err == nil {
		return "rails"
	}

	if _, err := os.Stat(filepath.Join(dir, "Gemfile")); err == nil {
		return "ruby"
	}

	return ""
}

func detectLanguageRecursive(dir string, maxDepth int) string {
	if maxDepth <= 0 {
		return ""
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		if entry.IsDir() {
			subdir := filepath.Join(dir, entry.Name())

			if lang := detectLanguageInDir(subdir); lang != "" {
				return lang
			}

			if lang := detectLanguageRecursive(subdir, maxDepth-1); lang != "" {
				return lang
			}
		}
	}

	return ""
}

// NewLang creates a language handler. Priority: explicit spec.Lang.Name,
// then sniffing the release dir. The resolved name is stored back on the
// spec so callers have a single source of truth.
func NewLang(spec *BuildSpec, releaseDir string) (Lang, error) {
	langType := spec.LangName()

	if langType == "" {
		detected, err := DetectLanguage(releaseDir)
		if err != nil {
			return nil, fmt.Errorf("failed to detect language: %v", err)
		}

		langType = detected
	}

	spec.setLangName(langType)

	switch langType {
	case "go":
		return &Golang{}, nil
	case "python":
		return &Python{}, nil
	case "nodejs":
		return &Nodejs{}, nil
	case "ruby":
		return &Ruby{}, nil
	case "rails":
		return &Rails{}, nil
	case "docker":
		return &Generic{}, nil
	default:
		return &Generic{}, nil
	}
}
