package lang

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/paths"
)

// restartContainer cycles the app's active container. Falls back to the
// `-green` suffix used by the blue/green swap in docker.DeployContainer.
// Shared across every language handler — the restart flow is identical.
func restartContainer(appName string) error {
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

// cleanupReleases prunes old release directories, keeping the N newest.
// Shared across handlers — GC is identical regardless of language.
const defaultKeepReleases = 5

func cleanupReleases(appName string) error {
	fmt.Printf("-----> Cleaning up old releases for %s...\n", appName)

	releasesDir := paths.AppReleasesDir(appName)

	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return err
	}

	if len(entries) <= defaultKeepReleases {
		return nil
	}

	toRemove := len(entries) - defaultKeepReleases
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

// deployContainer is the shared docker-run dispatch used by every
// non-Go language handler. Go has its own copy (identical body) because
// a future refactor will let it grow extra build-arg wiring.
func deployContainer(appName string, spec *BuildSpec, releaseDir string) error {
	envFile := paths.AppEnvFile(appName)

	networkMode := "bridge"

	if spec.NetworkMode != "" {
		networkMode = spec.NetworkMode
	}

	volumes := []string{}
	volumes = append(volumes, fmt.Sprintf("%s:/app/shared", paths.AppVolumeDir(appName)))

	if len(spec.Volumes) > 0 {
		volumes = append(volumes, spec.Volumes...)
	}

	return docker.DeployContainer(docker.DeploymentConfig{
		AppName:     appName,
		ImageTag:    "latest",
		EnvFile:     envFile,
		ReleaseDir:  releaseDir,
		NetworkMode: networkMode,
		DockerPorts: spec.Ports,
		Volumes:     volumes,
	})
}

// runDockerBuild invokes `docker build` honouring spec.Dockerfile /
// spec.Workdir, tagged as <appName>:latest. buildArgs is merged into
// --build-arg flags. Shared by Python/Ruby/Rails/Nodejs handlers which
// all have the same shape.
func runDockerBuild(appName string, spec *BuildSpec, releaseDir string, buildArgs map[string]string) error {
	imageTag := fmt.Sprintf("%s:latest", appName)

	var cmd *exec.Cmd

	if spec.Dockerfile != "" {
		dockerfilePath := filepath.Join(releaseDir, spec.Dockerfile)

		if spec.Workdir != "" {
			workdirDockerfilePath := filepath.Join(releaseDir, spec.Workdir, spec.Dockerfile)

			if _, err := os.Stat(workdirDockerfilePath); err == nil {
				dockerfilePath = workdirDockerfilePath
			}
		}

		fmt.Printf("-----> Using custom Dockerfile: %s\n", dockerfilePath)
		cmd = exec.Command("docker", "build", "-f", dockerfilePath, "-t", imageTag, releaseDir)
	} else {
		cmd = exec.Command("docker", "build", "-t", imageTag, releaseDir)
	}

	for key, value := range buildArgs {
		cmd.Args = append(cmd.Args, "--build-arg", fmt.Sprintf("%s=%s", key, value))
	}

	for _, label := range docker.GetVooduLabels() {
		cmd.Args = append(cmd.Args, "--label", label)
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build failed: %v", err)
	}

	return nil
}

// ensureCustomDockerfile resolves spec.Dockerfile against releaseDir and
// spec.Workdir. Returns nil if a usable Dockerfile is found, an error if
// spec.Dockerfile was set but unreachable.
func ensureCustomDockerfile(releaseDir string, spec *BuildSpec) (found bool, err error) {
	if spec.Dockerfile == "" {
		return false, nil
	}

	customDockerfilePath := filepath.Join(releaseDir, spec.Dockerfile)

	if _, err := os.Stat(customDockerfilePath); err == nil {
		fmt.Printf("-----> Using custom Dockerfile: %s\n", spec.Dockerfile)
		return true, nil
	}

	if spec.Workdir != "" {
		workdirDockerfilePath := filepath.Join(releaseDir, spec.Workdir, spec.Dockerfile)

		if _, err := os.Stat(workdirDockerfilePath); err == nil {
			fmt.Printf("-----> Using custom Dockerfile in workdir: %s/%s\n", spec.Workdir, spec.Dockerfile)
			return true, nil
		}
	}

	return false, fmt.Errorf("custom Dockerfile not found: %s or %s",
		customDockerfilePath,
		filepath.Join(releaseDir, spec.Workdir, spec.Dockerfile),
	)
}
