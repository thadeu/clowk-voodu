// Package lang implements per-language build/deploy strategies. Each type
// satisfies the Lang interface. Ported from the Gokku codebase with path
// and type-reference updates for Voodu.
package lang

import (
	"fmt"
	"os"
	"path/filepath"

	"go.voodu.clowk.in/internal/config"
)

type Lang interface {
	Build(appName string, app *config.App, releaseDir string) error
	Deploy(appName string, app *config.App, releaseDir string) error
	Restart(appName string, app *config.App) error
	Cleanup(appName string, app *config.App) error
	DetectLanguage(releaseDir string) (string, error)
	EnsureDockerfile(releaseDir string, appName string, app *config.App) error
	GetDefaultConfig() *config.App
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

// NewLang creates a language handler based on detected or configured language.
func NewLang(app *config.App, releaseDir string) (Lang, error) {
	var langType string
	var err error

	if app.Lang != "" {
		langType = app.Lang
	} else {
		langType, err = DetectLanguage(releaseDir)

		if err != nil {
			return nil, fmt.Errorf("failed to detect language: %v", err)
		}
	}

	app.Lang = langType

	switch langType {
	case "go":
		return &Golang{app: app}, nil
	case "python":
		return &Python{app: app}, nil
	case "nodejs":
		return &Nodejs{app: app}, nil
	case "ruby":
		return &Ruby{app: app}, nil
	case "rails":
		return &Rails{app: app}, nil
	case "docker":
		return &Generic{app: app}, nil
	default:
		return &Generic{app: app}, nil
	}
}
