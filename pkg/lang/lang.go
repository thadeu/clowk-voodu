package lang

import (
	"fmt"
	"os"
	"path/filepath"

	"go.gokku-vm.com/pkg"
)

type Lang interface {
	Build(appName string, app *pkg.App, releaseDir string) error
	Deploy(appName string, app *pkg.App, releaseDir string) error
	Restart(appName string, app *pkg.App) error
	Cleanup(appName string, app *pkg.App) error
	DetectLanguage(releaseDir string) (string, error)
	EnsureDockerfile(releaseDir string, appName string, app *pkg.App) error
	GetDefaultConfig() *pkg.App
}

// DetectLanguage automatically detects the programming language based on project files
func DetectLanguage(releaseDir string) (string, error) {
	// Check for existing Dockerfile first (highest priority)
	if _, err := os.Stat(filepath.Join(releaseDir, "Dockerfile")); err == nil {
		return "docker", nil
	}

	// Check for language files in root directory
	if lang := detectLanguageInDir(releaseDir); lang != "" {
		return lang, nil
	}

	// Check for language files in subdirectories (recursive, max depth 2)
	if lang := detectLanguageRecursive(releaseDir, 2); lang != "" {
		return lang, nil
	}

	// Default to generic
	return "generic", nil
}

// detectLanguageInDir checks for language files in a specific directory
func detectLanguageInDir(dir string) string {
	// Check for Go
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return "go"
	}

	// Check for Node.js
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		return "nodejs"
	}

	// Check for Python
	if _, err := os.Stat(filepath.Join(dir, "requirements.txt")); err == nil {
		return "python"
	}
	if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err == nil {
		return "python"
	}

	// Check for Rails
	if _, err := os.Stat(filepath.Join(dir, "config/application.rb")); err == nil {
		return "rails"
	}

	// Check for Ruby
	if _, err := os.Stat(filepath.Join(dir, "Gemfile")); err == nil {
		return "ruby"
	}

	return ""
}

// detectLanguageRecursive checks for language files in subdirectories
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

			// Continue searching in subdirectories
			if lang := detectLanguageRecursive(subdir, maxDepth-1); lang != "" {
				return lang
			}
		}
	}

	return ""
}

// NewLang creates a language handler based on detected or configured language
func NewLang(app *pkg.App, releaseDir string) (Lang, error) {
	var langType string
	var err error

	// Use configured language or auto-detect
	if app.Lang != "" {
		langType = app.Lang
	} else {
		langType, err = DetectLanguage(releaseDir)

		if err != nil {
			return nil, fmt.Errorf("failed to detect language: %v", err)
		}
	}

	// Update app.Lang with detected language
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
