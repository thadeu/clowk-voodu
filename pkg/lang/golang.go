package lang

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.gokku-vm.com/pkg"
	"go.gokku-vm.com/pkg/util"
)

type Golang struct {
	app *pkg.App
}

func (l *Golang) Build(appName string, app *pkg.App, releaseDir string) error {
	fmt.Println("-----> Building Go application...")

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

		// Build Docker command with build args
		buildArgs := l.getDockerBuildArgs(app)
		cmd = exec.Command("docker", "build", "--progress=plain", "-f", dockerfilePath, "-t", imageTag, releaseDir)

		// Add build args to command
		for key, value := range buildArgs {
			cmd.Args = append(cmd.Args, "--build-arg", fmt.Sprintf("%s=%s", key, value))
		}

		// Add Gokku labels to image
		for _, label := range pkg.GetGokkuLabels() {
			cmd.Args = append(cmd.Args, "--label", label)
		}

		fmt.Printf("-----> Using custom Dockerfile: %s\n", dockerfilePath)
	} else {
		// Use default Dockerfile in release directory
		cmd = exec.Command("docker", "build", "--progress=plain", "-t", imageTag, releaseDir)
		// Add Gokku labels to image
		for _, label := range pkg.GetGokkuLabels() {
			cmd.Args = append(cmd.Args, "--label", label)
		}
	}

	// Enable BuildKit for cache mounts support
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Use timeout wrapper for build (default 60 minutes for Go builds)
	if err := util.RunDockerBuildWithTimeout(cmd, 60); err != nil {
		return err
	}

	fmt.Println("-----> Go build complete!")
	return nil
}

func (l *Golang) Deploy(appName string, app *pkg.App, releaseDir string) error {
	fmt.Println("-----> Deploying Go application...")

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

func (l *Golang) Restart(appName string, app *pkg.App) error {
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

func (l *Golang) Cleanup(appName string, app *pkg.App) error {
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

func (l *Golang) DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "go.mod")); err == nil {
		return "go", nil
	}
	return "", fmt.Errorf("not a Go project")
}

func (l *Golang) EnsureDockerfile(releaseDir string, appName string, app *pkg.App) error {
	fmt.Printf("-----> EnsureDockerfile called for app: %s\n", appName)

	// Check if custom Dockerfile is specified
	if app.Dockerfile != "" {
		customDockerfilePath := filepath.Join(releaseDir, app.Dockerfile)
		fmt.Printf("-----> Custom Dockerfile path: %s\n", customDockerfilePath)
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
	fmt.Printf("-----> Default Dockerfile path: %s\n", dockerfilePath)
	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Println("-----> Using existing Dockerfile")
		return nil
	}

	fmt.Println("-----> Generating Dockerfile for Go...")

	// Get build configuration
	build := l.GetDefaultConfig()
	if app.Image != "" {
		build.Image = app.Image
	}
	// Determine working directory
	workDir := "."
	if app.WorkDir != "" {
		workDir = app.WorkDir
	}
	fmt.Printf("-----> Working directory from config: '%s'\n", app.WorkDir)
	fmt.Printf("-----> Using workDir: '%s'\n", workDir)

	if app.Path != "" {
		// Since we COPY workdir ., the build path should be relative to workdir
		build.Path = "./" + strings.TrimPrefix(app.Path, "./")
		fmt.Printf("-----> Configured path: '%s'\n", app.Path)
	} else {
		// Default to working directory root
		build.Path = "."
	}
	fmt.Printf("-----> Final build path: '%s'\n", build.Path)

	// Generate Dockerfile content
	dockerfileContent := l.generateDockerfile(build, appName, app)

	// Write Dockerfile
	return os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644)
}

func (l *Golang) GetDefaultConfig() *pkg.App {
	return &pkg.App{
		// Default configuration for Go apps
		Path:    "",
		WorkDir: ".",
	}
}

func (l *Golang) generateDockerfile(build *pkg.App, appName string, app *pkg.App) string {
	// Determine build path
	buildPath := build.Path

	if buildPath == "" {
		buildPath = "."
	}

	fmt.Printf("-----> Dockerfile build path: %s\n", buildPath)

	// Determine base image
	baseImage := build.Image

	if baseImage == "" {
		// Try to detect Go version from go.mod
		baseImage = util.DetectGoVersion(".")
		fmt.Printf("-----> Detected Go version: %s\n", baseImage)
	}

	// Get the workdir for COPY
	workDir := "."

	if app.WorkDir != "" {
		workDir = app.WorkDir
	}

	fmt.Printf("-----> Using workdir: %s\n", workDir)

	// Detect system architecture
	detectedGoos, detectedGoarch := l.detectSystemArchitecture()
	fmt.Printf("-----> Detected system: %s/%s\n", detectedGoos, detectedGoarch)

	// Get build configuration with dynamic defaults
	goos := detectedGoos
	goarch := detectedGoarch
	cgoEnabled := "0"

	if build.Goos != "" {
		goos = build.Goos
		fmt.Printf("-----> Using configured GOOS: %s (overriding detected: %s)\n", goos, detectedGoos)
	}
	if build.Goarch != "" {
		goarch = build.Goarch
		fmt.Printf("-----> Using configured GOARCH: %s (overriding detected: %s)\n", goarch, detectedGoarch)
	}
	if build.CgoEnabled != nil {
		if *build.CgoEnabled {
			cgoEnabled = "1"
		}
	}

	fmt.Printf("-----> Final build config: GOOS=%s GOARCH=%s CGO_ENABLED=%s\n", goos, goarch, cgoEnabled)

	return fmt.Sprintf(`# Generated Dockerfile for Go application
FROM %s AS builder

WORKDIR /app

# Copy only go.mod and go.sum first (for better Docker layer caching)
COPY %s/go.mod %s/go.sum* ./

# Download dependencies with cache mount (this layer will be cached if go.mod/go.sum don't change)
RUN --mount=type=cache,target=/go/pkg/mod \
  go mod download

# Copy the rest of the application code
COPY %s .

# Build the application with cache mounts for faster builds
RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  CGO_ENABLED=%s GOOS=%s GOARCH=%s \
  go build -ldflags="-w -s" -o app %s

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates \
  tzdata sox lame \
  && rm -rf /var/cache/apk/*

WORKDIR /root/

# Copy binary from builder
COPY --from=builder /app/app .

# Run the application
CMD ["/root/app"]
`, baseImage, workDir, workDir, workDir, cgoEnabled, goos, goarch, buildPath)
}

// detectSystemArchitecture detects the current system architecture
func (l *Golang) detectSystemArchitecture() (goos, goarch string) {
	// Use runtime package to detect current system
	goos = runtime.GOOS
	goarch = runtime.GOARCH

	// Map some common architectures to Docker-compatible names
	switch goarch {
	case "amd64":
		goarch = "amd64"
	case "arm64":
		goarch = "arm64"
	case "386":
		goarch = "386"
	case "arm":
		goarch = "arm"
	default:
		// Default to amd64 for unknown architectures
		goarch = "amd64"
	}

	// Map OS names to Docker-compatible names
	switch goos {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "linux" // Docker containers run on Linux
	case "windows":
		goos = "linux" // Docker containers run on Linux
	default:
		goos = "linux" // Default to Linux for containers
	}

	return goos, goarch
}

// getDockerBuildArgs returns build arguments for Docker build command
func (l *Golang) getDockerBuildArgs(app *pkg.App) map[string]string {
	// Detect system architecture
	detectedGoos, detectedGoarch := l.detectSystemArchitecture()
	fmt.Printf("-----> Detected system: %s/%s\n", detectedGoos, detectedGoarch)

	// Get build configuration with dynamic defaults
	goos := detectedGoos
	goarch := detectedGoarch
	cgoEnabled := "0"
	goVersion := "1.25"

	if app.Goos != "" {
		goos = app.Goos
		fmt.Printf("-----> Using configured GOOS: %s (overriding detected: %s)\n", goos, detectedGoos)
	}
	if app.Goarch != "" {
		goarch = app.Goarch
		fmt.Printf("-----> Using configured GOARCH: %s (overriding detected: %s)\n", goarch, detectedGoarch)
	}
	if app.CgoEnabled != nil {
		if *app.CgoEnabled {
			cgoEnabled = "1"
		}
	}
	if app.GoVersion != "" {
		goVersion = app.GoVersion
	}

	fmt.Printf("-----> Build args: GOOS=%s GOARCH=%s CGO_ENABLED=%s GO_VERSION=%s\n", goos, goarch, cgoEnabled, goVersion)

	// Return build args for Docker
	return map[string]string{
		"GOOS":        goos,
		"GOARCH":      goarch,
		"CGO_ENABLED": cgoEnabled,
		"GO_VERSION":  goVersion,
	}
}
