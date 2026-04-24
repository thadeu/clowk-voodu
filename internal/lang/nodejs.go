package lang

import (
	"fmt"
	"os"
	"path/filepath"

	"go.voodu.clowk.in/internal/util"
)

type Nodejs struct{}

func (l *Nodejs) block(spec *BuildSpec) *LangBuildSpec {
	if spec != nil && spec.Lang != nil {
		return spec.Lang
	}

	return &LangBuildSpec{}
}

func (l *Nodejs) Build(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Println("-----> Building Node.js application...")

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

	fmt.Println("-----> Node.js build complete!")

	return nil
}

func (l *Nodejs) Deploy(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Println("-----> Deploying Node.js application...")
	return deployContainer(appName, spec, releaseDir)
}

func (l *Nodejs) Restart(appName string, spec *BuildSpec) error {
	return restartContainer(appName)
}

func (l *Nodejs) Cleanup(appName string, spec *BuildSpec) error {
	return cleanupReleases(appName)
}

func (l *Nodejs) DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "package.json")); err == nil {
		return "nodejs", nil
	}

	return "", fmt.Errorf("not a Node.js project")
}

func (l *Nodejs) EnsureDockerfile(releaseDir string, appName string, spec *BuildSpec) error {
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

	fmt.Println("-----> Generating Dockerfile for Node.js...")

	dockerfileContent := l.generateDockerfile(spec, appName)

	return os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644)
}

func (l *Nodejs) generateDockerfile(spec *BuildSpec, appName string) string {
	block := l.block(spec)

	entrypoint := block.Entrypoint

	if entrypoint == "" {
		entrypoint = "index.js"
	}

	baseImage := spec.Image

	if baseImage == "" {
		if block.Version != "" {
			baseImage = fmt.Sprintf("node:%s-alpine", block.Version)
		} else {
			baseImage = util.DetectNodeVersion(".")
			fmt.Printf("-----> Detected Node.js version: %s\n", baseImage)
		}
	}

	return fmt.Sprintf(`# Generated Dockerfile for Node.js application
# App: %s
# Entrypoint: %s

FROM %s

WORKDIR /app

# Copy package files
COPY package*.json ./
RUN npm ci --only=production

# Copy application code
COPY . .

# Run the application
CMD ["node", "%s"]
`, appName, entrypoint, baseImage, entrypoint)
}
