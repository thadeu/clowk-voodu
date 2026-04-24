package lang

import (
	"fmt"
	"os"
	"path/filepath"

	"go.voodu.clowk.in/internal/util"
)

type Python struct{}

func (l *Python) block(spec *BuildSpec) *LangBuildSpec {
	if spec != nil && spec.Lang != nil {
		return spec.Lang
	}

	return &LangBuildSpec{}
}

func (l *Python) Build(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Println("-----> Building Python application...")

	if spec.Image != "" && util.IsRegistryImage(spec.Image, util.GetCustomRegistries(appName)) {
		fmt.Println("-----> Using pre-built image from registry...")

		if err := util.PullRegistryImage(spec.Image); err != nil {
			return fmt.Errorf("failed to pull pre-built image: %v", err)
		}

		if err := util.TagImageForApp(spec.Image, appName); err != nil {
			return fmt.Errorf("failed to tag image: %v", err)
		}

		fmt.Println("-----> Pre-built image ready for deployment!")

		return nil
	}

	if err := l.EnsureDockerfile(releaseDir, appName, spec); err != nil {
		return fmt.Errorf("failed to ensure Dockerfile: %v", err)
	}

	if err := runDockerBuild(appName, spec, releaseDir, l.block(spec).BuildArgs); err != nil {
		return err
	}

	fmt.Println("-----> Python build complete!")

	return nil
}

func (l *Python) Deploy(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Println("-----> Deploying Python application...")
	return deployContainer(appName, spec, releaseDir)
}

func (l *Python) Restart(appName string, spec *BuildSpec) error {
	return restartContainer(appName)
}

func (l *Python) Cleanup(appName string, spec *BuildSpec) error {
	return cleanupReleases(appName)
}

func (l *Python) DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "requirements.txt")); err == nil {
		return "python", nil
	}

	if _, err := os.Stat(filepath.Join(releaseDir, "pyproject.toml")); err == nil {
		return "python", nil
	}

	return "", fmt.Errorf("not a Python project")
}

func (l *Python) EnsureDockerfile(releaseDir string, appName string, spec *BuildSpec) error {
	found, err := ensureCustomDockerfile(releaseDir, spec)
	if err != nil {
		return err
	}

	if found {
		return nil
	}

	dockerfilePath := filepath.Join(releaseDir, "Dockerfile")

	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Println("-----> Using existing Dockerfile")
		return nil
	}

	fmt.Println("-----> Generating Dockerfile for Python...")

	dockerfileContent := l.generateDockerfile(spec, appName)

	return os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644)
}

func (l *Python) generateDockerfile(spec *BuildSpec, appName string) string {
	block := l.block(spec)

	entrypoint := block.Entrypoint

	if entrypoint == "" {
		entrypoint = "main.py"
	}

	baseImage := spec.Image

	if baseImage == "" {
		if block.Version != "" {
			baseImage = fmt.Sprintf("python:%s-slim", block.Version)
		} else {
			baseImage = util.DetectPythonVersion(".")
			fmt.Printf("-----> Using Python fallback: %s\n", baseImage)
		}
	}

	return fmt.Sprintf(`# Generated Dockerfile for Python application
# App: %s
# Entrypoint: %s

FROM %s

WORKDIR /app

# Install system dependencies if needed
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc sox libsox-fmt-all lame \
    && rm -rf /var/lib/apt/lists/*

# Copy requirements
COPY requirements.txt* ./
RUN if [ -f requirements.txt ]; then pip install --no-cache-dir -r requirements.txt; fi

# Copy application code
COPY . .

# Run the application
CMD ["python", "%s"]
`, appName, entrypoint, baseImage, entrypoint)
}
