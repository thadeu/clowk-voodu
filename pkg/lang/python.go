package lang

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.gokku-vm.com/pkg"

	"go.gokku-vm.com/pkg/util"
)

type Python struct {
	app *pkg.App
}

func (l *Python) Build(appName string, app *pkg.App, releaseDir string) error {
	fmt.Println("-----> Building Python application...")

	// Check if using pre-built image from registry
	if app.Image != "" && util.IsRegistryImage(app.Image, util.GetCustomRegistries(appName)) {
		fmt.Println("-----> Using pre-built image from registry...")

		// Pull the pre-built image
		if err := util.PullRegistryImage(app.Image); err != nil {
			return fmt.Errorf("failed to pull pre-built image: %v", err)
		}

		// Tag the image for the app
		if err := util.TagImageForApp(app.Image, appName); err != nil {
			return fmt.Errorf("failed to tag image: %v", err)
		}

		fmt.Println("-----> Pre-built image ready for deployment!")
		return nil
	}

	// Ensure Dockerfile exists
	if err := l.EnsureDockerfile(releaseDir, appName, app); err != nil {
		return fmt.Errorf("failed to ensure Dockerfile: %v", err)
	}

	// Build Docker image
	imageTag := fmt.Sprintf("%s:latest", appName)

	// Check if custom Dockerfile path is specified
	var cmd *exec.Cmd
	if app.Dockerfile != "" {
		// Use custom Dockerfile path
		dockerfilePath := filepath.Join(releaseDir, app.Dockerfile)
		// Check if Dockerfile exists in workdir
		if app.WorkDir != "" {
			workdirDockerfilePath := filepath.Join(releaseDir, app.WorkDir, app.Dockerfile)
			if _, err := os.Stat(workdirDockerfilePath); err == nil {
				dockerfilePath = workdirDockerfilePath
			}
		}
		fmt.Printf("-----> Using custom Dockerfile: %s\n", dockerfilePath)
		cmd = exec.Command("docker", "build", "-f", dockerfilePath, "-t", imageTag, releaseDir)
		// Add Gokku labels to image
		for _, label := range pkg.GetGokkuLabels() {
			cmd.Args = append(cmd.Args, "--label", label)
		}
	} else {
		// Use default Dockerfile in release directory
		cmd = exec.Command("docker", "build", "-t", imageTag, releaseDir)
		// Add Gokku labels to image
		for _, label := range pkg.GetGokkuLabels() {
			cmd.Args = append(cmd.Args, "--label", label)
		}
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build failed: %v", err)
	}

	fmt.Println("-----> Python build complete!")
	return nil
}

func (l *Python) Deploy(appName string, app *pkg.App, releaseDir string) error {
	fmt.Println("-----> Deploying Python application...")

	// Get environment file
	envFile := filepath.Join("/opt/gokku/apps", appName, "shared", ".env")

	networkMode := "bridge"

	if app.Network != nil && app.Network.Mode != "" {
		networkMode = app.Network.Mode
	}

	// Create deployment config
	volumes := []string{}
	volumes = append(volumes, fmt.Sprintf("/opt/gokku/volumes/%s:/app/shared", appName))

	if len(app.Volumes) > 0 {
		volumes = append(volumes, app.Volumes...)
	}

	return pkg.DeployContainer(pkg.DeploymentConfig{
		AppName:     appName,
		ImageTag:    "latest",
		EnvFile:     envFile,
		ReleaseDir:  releaseDir,
		NetworkMode: networkMode,
		DockerPorts: app.Ports,
		Volumes:     volumes,
	})
}

func (l *Python) Restart(appName string, app *pkg.App) error {
	fmt.Printf("-----> Restarting %s...\n", appName)

	// Find active container
	containerName := appName
	if !pkg.ContainerExists(containerName) {
		containerName = appName + "-green"
	}

	if !pkg.ContainerExists(containerName) {
		return fmt.Errorf("no active container found for %s", appName)
	}

	// Restart container
	cmd := exec.Command("docker", "restart", containerName)
	return cmd.Run()
}

func (l *Python) Cleanup(appName string, app *pkg.App) error {
	fmt.Printf("-----> Cleaning up old releases for %s...\n", appName)

	appDir := filepath.Join("/opt/gokku/apps", appName)
	releasesDir := filepath.Join(appDir, "releases")

	// Read all release directories
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return err
	}

	// Keep only the last 5 releases
	keepReleases := 5
	if len(entries) <= keepReleases {
		return nil
	}

	// Remove old releases
	toRemove := len(entries) - keepReleases
	for i := 0; i < toRemove; i++ {
		entry := entries[i]
		releasePath := filepath.Join(releasesDir, entry.Name())
		if err := os.RemoveAll(releasePath); err != nil {
			fmt.Printf("Warning: Failed to remove old release %s: %v\n", entry.Name(), err)
		} else {
			fmt.Printf("-----> Removed old release: %s\n", entry.Name())
		}
	}

	return nil
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

func (l *Python) EnsureDockerfile(releaseDir string, appName string, app *pkg.App) error {
	// Check if custom Dockerfile is specified
	if app.Dockerfile != "" {
		customDockerfilePath := filepath.Join(releaseDir, app.Dockerfile)
		if _, err := os.Stat(customDockerfilePath); err == nil {
			fmt.Printf("-----> Using custom Dockerfile: %s\n", app.Dockerfile)
			return nil
		}
		// If custom Dockerfile not found, try relative to workdir
		if app.WorkDir != "" {
			workdirDockerfilePath := filepath.Join(releaseDir, app.WorkDir, app.Dockerfile)
			if _, err := os.Stat(workdirDockerfilePath); err == nil {
				fmt.Printf("-----> Using custom Dockerfile in workdir: %s/%s\n", app.WorkDir, app.Dockerfile)
				return nil
			}
		}
		return fmt.Errorf("custom Dockerfile not found: %s or %s", customDockerfilePath, filepath.Join(releaseDir, app.WorkDir, app.Dockerfile))
	}

	// Check if default Dockerfile exists
	dockerfilePath := filepath.Join(releaseDir, "Dockerfile")
	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Println("-----> Using existing Dockerfile")
		return nil
	}

	fmt.Println("-----> Generating Dockerfile for Python...")

	// Get build configuration
	build := l.GetDefaultConfig()
	if app.Image != "" {
		build.Image = app.Image
	}
	if app.Entrypoint != "" {
		build.Entrypoint = app.Entrypoint
	}

	// Generate Dockerfile content
	dockerfileContent := l.generateDockerfile(build, appName, app)

	// Write Dockerfile
	return os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644)
}

func (l *Python) GetDefaultConfig() *pkg.App {
	return &pkg.App{
		// Default configuration for Python apps
		Entrypoint: "main.py",
		WorkDir:    ".",
	}
}

func (l *Python) generateDockerfile(build *pkg.App, appName string, app *pkg.App) string {
	// Determine entrypoint
	entrypoint := build.Entrypoint
	if entrypoint == "" {
		entrypoint = "main.py"
	}

	// Determine base image
	baseImage := build.Image
	if baseImage == "" {
		// Use latest Python as fallback
		baseImage = util.DetectPythonVersion(".")
		fmt.Printf("-----> Using Python fallback: %s\n", baseImage)
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
