package lang

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.voodu.clowk.in/internal/config"
	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/paths"
	"go.voodu.clowk.in/internal/util"
)

type Ruby struct {
	app *config.App
}

func (l *Ruby) Build(appName string, app *config.App, releaseDir string) error {
	fmt.Println("-----> Building Ruby application...")

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

		fmt.Printf("-----> Using custom Dockerfile: %s\n", dockerfilePath)
		cmd = exec.Command("docker", "build", "-f", dockerfilePath, "-t", imageTag, releaseDir)

		for _, label := range docker.GetVooduLabels() {
			cmd.Args = append(cmd.Args, "--label", label)
		}
	} else {
		cmd = exec.Command("docker", "build", "-t", imageTag, releaseDir)

		for _, label := range docker.GetVooduLabels() {
			cmd.Args = append(cmd.Args, "--label", label)
		}
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build failed: %v", err)
	}

	fmt.Println("-----> Ruby build complete!")

	return nil
}

func (l *Ruby) Deploy(appName string, app *config.App, releaseDir string) error {
	fmt.Println("-----> Deploying Ruby application...")

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

func (l *Ruby) Restart(appName string, app *config.App) error {
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

func (l *Ruby) Cleanup(appName string, app *config.App) error {
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

func (l *Ruby) DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "Gemfile")); err == nil {
		return "ruby", nil
	}

	return "", fmt.Errorf("not a Ruby project")
}

func (l *Ruby) EnsureDockerfile(releaseDir string, appName string, app *config.App) error {
	if app.Dockerfile != "" {
		customDockerfilePath := filepath.Join(releaseDir, app.Dockerfile)

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

	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Println("-----> Using existing Dockerfile")

		return nil
	}

	fmt.Println("-----> Generating Dockerfile for Ruby...")

	build := l.GetDefaultConfig()

	if app.Image != "" {
		build.Image = app.Image
	}

	if app.Entrypoint != "" {
		build.Entrypoint = app.Entrypoint
	}

	dockerfileContent := l.generateDockerfile(build, appName, app)

	return os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644)
}

func (l *Ruby) GetDefaultConfig() *config.App {
	return &config.App{
		Entrypoint: "app.rb",
		WorkDir:    ".",
	}
}

func (l *Ruby) generateDockerfile(build *config.App, appName string, app *config.App) string {
	entrypoint := build.Entrypoint

	if entrypoint == "" {
		entrypoint = "app.rb"
	}

	baseImage := build.Image

	if baseImage == "" {
		baseImage = util.DetectRubyVersion(".")
		fmt.Printf("-----> Detected Ruby version: %s\n", baseImage)
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
