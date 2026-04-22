package lang

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.voodu.clowk.in/internal/config"
	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/paths"
)

type Generic struct {
	app *config.App
}

func (l *Generic) Build(appName string, app *config.App, releaseDir string) error {
	fmt.Println("-----> Building generic application...")

	var dockerfilePath string

	if app.Dockerfile != "" {
		dockerfilePath = filepath.Join(releaseDir, app.Dockerfile)

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

	imageTag := fmt.Sprintf("%s:latest", appName)

	var cmd *exec.Cmd
	if app.Dockerfile != "" {
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

	fmt.Println("-----> Generic build complete!")

	return nil
}

func (l *Generic) Deploy(appName string, app *config.App, releaseDir string) error {
	fmt.Println("-----> Deploying generic application...")

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

func (l *Generic) Restart(appName string, app *config.App) error {
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

func (l *Generic) Cleanup(appName string, app *config.App) error {
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

func (l *Generic) DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "Dockerfile")); err == nil {
		return "docker", nil
	}

	return "generic", nil
}

func (l *Generic) EnsureDockerfile(releaseDir string, appName string, app *config.App) error {
	dockerfilePath := filepath.Join(releaseDir, "Dockerfile")

	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Println("-----> Using existing Dockerfile")

		return nil
	}

	return fmt.Errorf("no Dockerfile found and no language-specific strategy available")
}

func (l *Generic) GetDefaultConfig() *config.App {
	return &config.App{}
}
