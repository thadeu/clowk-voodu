package lang

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.gokku-vm.com/pkg"
)

type Generic struct {
	app *pkg.App
}

func (l *Generic) Build(appName string, app *pkg.App, releaseDir string) error {
	fmt.Println("-----> Building generic application...")

	// Check if custom Dockerfile is specified
	var dockerfilePath string
	if app.Dockerfile != "" {
		dockerfilePath = filepath.Join(releaseDir, app.Dockerfile)
		// Check if Dockerfile exists in workdir
		if app.WorkDir != "" {
			workdirDockerfilePath := filepath.Join(releaseDir, app.WorkDir, app.Dockerfile)
			if _, err := os.Stat(workdirDockerfilePath); err == nil {
				dockerfilePath = workdirDockerfilePath
				fmt.Printf("-----> Using custom Dockerfile in workdir: %s/%s\n", app.WorkDir, app.Dockerfile)
			} else {
				fmt.Printf("-----> Using custom Dockerfile: %s\n", app.Dockerfile)
			}
		} else {
			fmt.Printf("-----> Using custom Dockerfile: %s\n", app.Dockerfile)
		}
	} else {
		dockerfilePath = filepath.Join(releaseDir, "Dockerfile")
		fmt.Println("-----> Using default Dockerfile")
	}

	if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
		return fmt.Errorf("no Dockerfile found and no language-specific strategy available")
	}

	// Build Docker image
	imageTag := fmt.Sprintf("%s:latest", appName)

	// Build Docker image with the determined Dockerfile path
	var cmd *exec.Cmd
	if app.Dockerfile != "" {
		// Use custom Dockerfile path
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

	fmt.Println("-----> Generic build complete!")
	return nil
}

func (l *Generic) Deploy(appName string, app *pkg.App, releaseDir string) error {
	fmt.Println("-----> Deploying generic application...")

	// Get environment file
	envFile := filepath.Join("/opt/gokku/apps", appName, "shared", ".env")

	networkMode := "bridge"

	if app.Network != nil && app.Network.Mode != "" {
		networkMode = app.Network.Mode
	}

	// Deploy using Docker client
	volumes := []string{}
	volumes = append(volumes, fmt.Sprintf("/opt/gokku/volumes/%s:/app/shared", appName))

	if len(app.Volumes) > 0 {
		volumes = append(volumes, app.Volumes...)
	}

	return pkg.DeployContainer(pkg.DeploymentConfig{
		AppName:       appName,
		ImageTag:      "latest",
		EnvFile:       envFile,
		ReleaseDir:    releaseDir,
		ZeroDowntime:  true,
		HealthTimeout: 60,
		NetworkMode:   networkMode,
		DockerPorts:   app.Ports,
		Volumes:       volumes,
	})
}

func (l *Generic) Restart(appName string, app *pkg.App) error {
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

func (l *Generic) Cleanup(appName string, app *pkg.App) error {
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

func (l *Generic) DetectLanguage(releaseDir string) (string, error) {
	// Check for existing Dockerfile
	if _, err := os.Stat(filepath.Join(releaseDir, "Dockerfile")); err == nil {
		return "docker", nil
	}
	return "generic", nil
}

func (l *Generic) EnsureDockerfile(releaseDir string, appName string, app *pkg.App) error {
	dockerfilePath := filepath.Join(releaseDir, "Dockerfile")

	// Check if Dockerfile already exists
	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Println("-----> Using existing Dockerfile")
		return nil
	}

	return fmt.Errorf("no Dockerfile found and no language-specific strategy available")
}

func (l *Generic) GetDefaultConfig() *pkg.App {
	return &pkg.App{
		// Default configuration for generic apps
	}
}
