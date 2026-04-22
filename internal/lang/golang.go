package lang

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.voodu.clowk.in/internal/config"
	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/paths"
	"go.voodu.clowk.in/internal/util"
)

type Golang struct {
	app *config.App
}

func (l *Golang) Build(appName string, app *config.App, releaseDir string) error {
	fmt.Println("-----> Building Go application...")

	if app.Image != "" && util.IsRegistryImage(app.Image, util.GetCustomRegistries(appName)) {
		fmt.Println("-----> Using pre-built image from registry...")

		if err := util.PullRegistryImage(app.Image); err != nil {
			return fmt.Errorf("failed to pull pre-built image: %v", err)
		}

		if err := util.TagImageForApp(app.Image, appName); err != nil {
			return fmt.Errorf("failed to tag image: %v", err)
		}

		fmt.Println("-----> Pre-built image ready for deployment!")

		return nil
	}

	if err := l.EnsureDockerfile(releaseDir, appName, app); err != nil {
		return fmt.Errorf("failed to ensure Dockerfile: %v", err)
	}

	imageTag := fmt.Sprintf("%s:latest", appName)

	var cmd *exec.Cmd
	if app.Dockerfile != "" {
		dockerfilePath := filepath.Join(releaseDir, app.Dockerfile)

		if app.WorkDir != "" {
			workdirDockerfilePath := filepath.Join(releaseDir, app.WorkDir, app.Dockerfile)

			if _, err := os.Stat(workdirDockerfilePath); err == nil {
				dockerfilePath = workdirDockerfilePath
			}
		}

		buildArgs := l.getDockerBuildArgs(app)
		cmd = exec.Command("docker", "build", "--progress=plain", "-f", dockerfilePath, "-t", imageTag, releaseDir)

		for key, value := range buildArgs {
			cmd.Args = append(cmd.Args, "--build-arg", fmt.Sprintf("%s=%s", key, value))
		}

		for _, label := range docker.GetVooduLabels() {
			cmd.Args = append(cmd.Args, "--label", label)
		}

		fmt.Printf("-----> Using custom Dockerfile: %s\n", dockerfilePath)
	} else {
		cmd = exec.Command("docker", "build", "--progress=plain", "-t", imageTag, releaseDir)

		for _, label := range docker.GetVooduLabels() {
			cmd.Args = append(cmd.Args, "--label", label)
		}
	}

	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := util.RunDockerBuildWithTimeout(cmd, 60); err != nil {
		return err
	}

	fmt.Println("-----> Go build complete!")

	return nil
}

func (l *Golang) Deploy(appName string, app *config.App, releaseDir string) error {
	fmt.Println("-----> Deploying Go application...")

	envFile := paths.AppEnvFile(appName)

	networkMode := "bridge"

	if app.Network != nil && app.Network.Mode != "" {
		networkMode = app.Network.Mode
	}

	volumes := []string{}
	volumes = append(volumes, fmt.Sprintf("%s:/app/shared", paths.AppVolumeDir(appName)))

	if len(app.Volumes) > 0 {
		volumes = append(volumes, app.Volumes...)
	}

	return docker.DeployContainer(docker.DeploymentConfig{
		AppName:     appName,
		ImageTag:    "latest",
		EnvFile:     envFile,
		ReleaseDir:  releaseDir,
		NetworkMode: networkMode,
		DockerPorts: app.Ports,
		Volumes:     volumes,
	})
}

func (l *Golang) Restart(appName string, app *config.App) error {
	fmt.Printf("-----> Restarting %s...\n", appName)

	containerName := appName

	if !docker.ContainerExists(containerName) {
		containerName = appName + "-green"
	}

	if !docker.ContainerExists(containerName) {
		return fmt.Errorf("no active container found for %s", appName)
	}

	cmd := exec.Command("docker", "restart", containerName)

	return cmd.Run()
}

func (l *Golang) Cleanup(appName string, app *config.App) error {
	fmt.Printf("-----> Cleaning up old releases for %s...\n", appName)

	releasesDir := paths.AppReleasesDir(appName)

	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return err
	}

	keepReleases := 5
	if len(entries) <= keepReleases {
		return nil
	}

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

func (l *Golang) EnsureDockerfile(releaseDir string, appName string, app *config.App) error {
	fmt.Printf("-----> EnsureDockerfile called for app: %s\n", appName)

	if app.Dockerfile != "" {
		customDockerfilePath := filepath.Join(releaseDir, app.Dockerfile)
		fmt.Printf("-----> Custom Dockerfile path: %s\n", customDockerfilePath)

		if _, err := os.Stat(customDockerfilePath); err == nil {
			fmt.Printf("-----> Using custom Dockerfile: %s\n", app.Dockerfile)

			return nil
		}

		if app.WorkDir != "" {
			workdirDockerfilePath := filepath.Join(releaseDir, app.WorkDir, app.Dockerfile)

			if _, err := os.Stat(workdirDockerfilePath); err == nil {
				fmt.Printf("-----> Using custom Dockerfile in workdir: %s/%s\n", app.WorkDir, app.Dockerfile)

				return nil
			}
		}

		return fmt.Errorf("custom Dockerfile not found: %s or %s", customDockerfilePath, filepath.Join(releaseDir, app.WorkDir, app.Dockerfile))
	}

	dockerfilePath := filepath.Join(releaseDir, "Dockerfile")
	fmt.Printf("-----> Default Dockerfile path: %s\n", dockerfilePath)

	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Println("-----> Using existing Dockerfile")

		return nil
	}

	fmt.Println("-----> Generating Dockerfile for Go...")

	build := l.GetDefaultConfig()

	if app.Image != "" {
		build.Image = app.Image
	}

	workDir := "."

	if app.WorkDir != "" {
		workDir = app.WorkDir
	}

	fmt.Printf("-----> Working directory from config: '%s'\n", app.WorkDir)
	fmt.Printf("-----> Using workDir: '%s'\n", workDir)

	if app.Path != "" {
		build.Path = "./" + strings.TrimPrefix(app.Path, "./")
		fmt.Printf("-----> Configured path: '%s'\n", app.Path)
	} else {
		build.Path = "."
	}

	fmt.Printf("-----> Final build path: '%s'\n", build.Path)

	dockerfileContent := l.generateDockerfile(build, appName, app)

	return os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644)
}

func (l *Golang) GetDefaultConfig() *config.App {
	return &config.App{
		Path:    "",
		WorkDir: ".",
	}
}

func (l *Golang) generateDockerfile(build *config.App, appName string, app *config.App) string {
	buildPath := build.Path

	if buildPath == "" {
		buildPath = "."
	}

	fmt.Printf("-----> Dockerfile build path: %s\n", buildPath)

	baseImage := build.Image

	if baseImage == "" {
		baseImage = util.DetectGoVersion(".")
		fmt.Printf("-----> Detected Go version: %s\n", baseImage)
	}

	workDir := "."

	if app.WorkDir != "" {
		workDir = app.WorkDir
	}

	fmt.Printf("-----> Using workdir: %s\n", workDir)

	detectedGoos, detectedGoarch := l.detectSystemArchitecture()
	fmt.Printf("-----> Detected system: %s/%s\n", detectedGoos, detectedGoarch)

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

func (l *Golang) detectSystemArchitecture() (goos, goarch string) {
	goos = runtime.GOOS
	goarch = runtime.GOARCH

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
		goarch = "amd64"
	}

	switch goos {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "linux"
	case "windows":
		goos = "linux"
	default:
		goos = "linux"
	}

	return goos, goarch
}

func (l *Golang) getDockerBuildArgs(app *config.App) map[string]string {
	detectedGoos, detectedGoarch := l.detectSystemArchitecture()
	fmt.Printf("-----> Detected system: %s/%s\n", detectedGoos, detectedGoarch)

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

	return map[string]string{
		"GOOS":        goos,
		"GOARCH":      goarch,
		"CGO_ENABLED": cgoEnabled,
		"GO_VERSION":  goVersion,
	}
}
