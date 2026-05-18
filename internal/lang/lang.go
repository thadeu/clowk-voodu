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
// Shape is flat (not nested like the manifest's `build {}` block) —
// handlers don't care about the registry-vs-build distinction, they
// only see build inputs. The flattening happens in
// deploy.Spec.toBuildSpec.
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

	// Context is the directory sent to `docker build` (docker-compose
	// matches this name). Was historically called `Workdir` here —
	// renamed for consistency with the operator-facing HCL surface
	// (`build { context = "..." }`).
	Context string

	Env         map[string]string
	Ports       []string
	Volumes     []string
	NetworkMode string

	// BuildArgs is the docker `--build-arg KEY=value` map populated
	// from the manifest's `build.args = {...}`. Handlers like Golang
	// inject their own platform defaults (GOOS/GOARCH/CGO_ENABLED)
	// internally and let operator-supplied entries here override on
	// key collision.
	BuildArgs map[string]string

	Lang *LangBuildSpec
}

// LangBuildSpec is the unified runtime hint. Name picks the handler;
// Version/Entrypoint are forwarded as-is. Build args live on the
// parent BuildSpec.BuildArgs (one source of truth — the manifest's
// `build.args`).
type LangBuildSpec struct {
	Name       string
	Version    string
	Entrypoint string
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
//
// Resolution order matters: a Dockerfile at the release root ALWAYS wins,
// even if a language-specific marker is also present. A Rails repo that
// ships its own Dockerfile should go through the Generic (docker) handler
// and use that file verbatim — not have the Rails handler take over and
// try to auto-generate one. Downstream handlers trust this ordering.
func DetectLanguage(releaseDir string) (string, error) {
	dockerfile := filepath.Join(releaseDir, "Dockerfile")

	if _, err := os.Stat(dockerfile); err == nil {
		fmt.Printf("-----> Detected Dockerfile at %s → using 'docker' strategy\n", dockerfile)
		return "docker", nil
	}

	if lang := detectLanguageInDir(releaseDir); lang != "" {
		fmt.Printf("-----> No Dockerfile at %s — detected '%s' from marker files\n", dockerfile, lang)
		return lang, nil
	}

	if lang := detectLanguageRecursive(releaseDir, 2); lang != "" {
		fmt.Printf("-----> No Dockerfile at release root — detected '%s' in subdirectory\n", lang)
		return lang, nil
	}

	fmt.Printf("-----> No Dockerfile or language markers found — falling back to 'generic'\n")

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

	if langType != "" {
		fmt.Printf("-----> Using explicit lang strategy: %q (from manifest)\n", langType)
	} else {
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
