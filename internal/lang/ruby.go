package lang

import (
	"fmt"
	"os"
	"path/filepath"

	"go.voodu.clowk.in/internal/util"
)

type Ruby struct{}

func (l *Ruby) block(spec *BuildSpec) *LangBuildSpec {
	if spec != nil && spec.Lang != nil {
		return spec.Lang
	}

	return &LangBuildSpec{}
}

func (l *Ruby) Build(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Fprintln(spec.out(), "-----> building ruby application...")

	if spec.Image != "" && util.IsRegistryImage(spec.Image, util.GetCustomRegistries(appName)) {
		fmt.Fprintln(spec.out(), "-----> using pre-built image from registry...")

		if err := util.PullRegistryImage(spec.Image); err != nil {
			return fmt.Errorf("failed to pull pre-built image: %v", err)
		}

		if err := util.TagImageForApp(spec.Image, appName); err != nil {
			return fmt.Errorf("failed to tag image: %v", err)
		}

		fmt.Fprintln(spec.out(), "-----> pre-built image ready for deployment!")

		return nil
	}

	if err := l.EnsureDockerfile(releaseDir, appName, spec); err != nil {
		return fmt.Errorf("failed to ensure Dockerfile: %v", err)
	}

	if err := runDockerBuild(appName, spec, releaseDir, spec.BuildArgs); err != nil {
		return err
	}

	fmt.Fprintln(spec.out(), "-----> ruby build complete!")

	return nil
}

func (l *Ruby) Deploy(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Fprintln(spec.out(), "-----> deploying ruby application...")
	return deployContainer(appName, spec, releaseDir)
}

func (l *Ruby) Restart(appName string, spec *BuildSpec) error {
	return restartContainer(spec.out(), appName)
}

func (l *Ruby) Cleanup(appName string, spec *BuildSpec) error {
	return cleanupReleases(spec.out(), appName)
}

func (l *Ruby) DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "Gemfile")); err == nil {
		return "ruby", nil
	}

	return "", fmt.Errorf("not a Ruby project")
}

func (l *Ruby) EnsureDockerfile(releaseDir string, appName string, spec *BuildSpec) error {
	found, err := ensureCustomDockerfile(releaseDir, spec)
	if err != nil {
		return err
	}

	if found {
		return nil
	}

	dockerfilePath := filepath.Join(releaseDir, "Dockerfile")

	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Fprintln(spec.out(), "-----> using existing dockerfile")
		return nil
	}

	fmt.Fprintln(spec.out(), "-----> generating dockerfile for ruby...")

	dockerfileContent := l.generateDockerfile(spec, appName)

	return os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644)
}

func (l *Ruby) generateDockerfile(spec *BuildSpec, appName string) string {
	block := l.block(spec)

	entrypoint := block.Entrypoint

	if entrypoint == "" {
		entrypoint = "app.rb"
	}

	baseImage := spec.Image

	if baseImage == "" {
		if block.Version != "" {
			baseImage = fmt.Sprintf("ruby:%s-alpine", block.Version)
		} else {
			baseImage = util.DetectRubyVersion(".")
			fmt.Fprintf(spec.out(), "-----> detected ruby version: %s\n", baseImage)
		}
	}

	return fmt.Sprintf(`# Generated Dockerfile for Ruby application
# App: %s
# Entrypoint: %s

FROM %s

WORKDIR /app

# Install system dependencies
RUN apk add --no-cache build-base sox lame

# Copy Gemfile
COPY Gemfile* ./
RUN bundle install --without development test

# Copy application code
COPY . .

# Run the application
CMD ["ruby", "%s"]
`, appName, entrypoint, baseImage, entrypoint)
}
