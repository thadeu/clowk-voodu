package lang

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.gokku-vm.com/pkg"
	"go.gokku-vm.com/pkg/util"
)

type Rails struct {
	app *pkg.App
}

func (l *Rails) Build(appName string, app *pkg.App, releaseDir string) error {
	fmt.Println("-----> Building Rails application...")

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
		dockerfilePath := filepath.Join(releaseDir, app.Dockerfile)

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

	fmt.Println("-----> Rails build complete!")
	return nil
}

func (l *Rails) Deploy(appName string, app *pkg.App, releaseDir string) error {
	fmt.Println("-----> Deploying Rails application...")

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

func (l *Rails) Restart(appName string, app *pkg.App) error {
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

func (l *Rails) Cleanup(appName string, app *pkg.App) error {
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

func (l *Rails) DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "Gemfile")); err == nil {
		return "ruby", nil
	}
	return "", fmt.Errorf("not a Rails project")
}

func (l *Rails) EnsureDockerfile(releaseDir string, appName string, app *pkg.App) error {
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

	fmt.Println("-----> Generating Dockerfile for Ruby...")

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

func (l *Rails) GetDefaultConfig() *pkg.App {
	return &pkg.App{
		Entrypoint: "app.rb",
		WorkDir:    ".",
	}
}

func (l *Rails) generateDockerfile(build *pkg.App, appName string, app *pkg.App) string {
	// Determine entrypoint
	entrypoint := build.Entrypoint

	if entrypoint == "" {
		entrypoint = "app.rb"
	}

	// Determine base image
	baseImage := build.Image

	if baseImage == "" {
		baseImage = util.DetectRubyVersion(".")
		fmt.Printf("-----> Detected Ruby version: %s\n", baseImage)
	}

	return fmt.Sprintf(`# Generated Dockerfile for Ruby application
# App: %s
# Entrypoint: %s

FROM %s as builder

WORKDIR /app

# Install dependencies for building native extensions
RUN apt-get update -qq && apt-get install -y build-essential libpq-dev nodejs npm yarn sox libsox-fmt-all lame

COPY Gemfile Gemfile.lock ./
RUN bundle install --jobs 4 --retry 3

COPY . .

# Precompile assets (if using Sprockets or similar)
# For Rails 8 with Propshaft, this step might be different or handled automatically
RUN bin/rails assets:precompile || true

# Stage 2: Production Stage
FROM %s as production

WORKDIR /app

# Install only runtime dependencies
RUN apt-get update -qq && apt-get install -y libpq-dev && rm -rf /var/lib/apt/lists/*

# Copy built application from the builder stage
COPY --from=builder /app /app

# Set environment variables for production
ENV RAILS_ENV=production
ENV BUNDLE_WITHOUT="development test"

# Expose the port your Rails app will listen on
EXPOSE 3000

# Set a non-root user for security
RUN useradd -ms /bin/bash rails
USER rails

# Command to run the Rails server
CMD ["bundle", "exec", "puma", "-C", "config/puma.rb"]
`, appName, entrypoint, baseImage, baseImage)
}
