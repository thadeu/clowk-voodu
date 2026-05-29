package lang

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/paths"
)

type Generic struct{}

func (l *Generic) Build(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Println("-----> building generic application...")

	var dockerfilePath string

	if spec.Dockerfile != "" {
		dockerfilePath = filepath.Join(releaseDir, spec.Dockerfile)

		if spec.Context != "" {
			workdirDockerfilePath := filepath.Join(releaseDir, spec.Context, spec.Dockerfile)

			if _, err := os.Stat(workdirDockerfilePath); err == nil {
				dockerfilePath = workdirDockerfilePath
				fmt.Printf("-----> using custom dockerfile in context: %s/%s\n", spec.Context, spec.Dockerfile)
			} else {
				fmt.Printf("-----> using custom dockerfile: %s\n", spec.Dockerfile)
			}
		} else {
			fmt.Printf("-----> using custom dockerfile: %s\n", spec.Dockerfile)
		}
	} else {
		dockerfilePath = filepath.Join(releaseDir, "Dockerfile")
		fmt.Println("-----> using default dockerfile")
	}

	if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
		return fmt.Errorf("no Dockerfile found and no language-specific strategy available")
	}

	// Two tags: floating :latest for the manifest's pull target,
	// and immutable :<buildID> so rollback can re-point :latest at
	// older content without rebuilding. See common.go's
	// runDockerBuild for the full rationale.
	latestTag := fmt.Sprintf("%s:latest", appName)
	buildID := filepath.Base(releaseDir)
	immutableTag := fmt.Sprintf("%s:%s", appName, buildID)

	var cmd *exec.Cmd
	if spec.Dockerfile != "" {
		cmd = exec.Command("docker", "build", "-f", dockerfilePath, "-t", latestTag, "-t", immutableTag, releaseDir)
	} else {
		cmd = exec.Command("docker", "build", "-t", latestTag, "-t", immutableTag, releaseDir)
	}

	// Build args from the manifest's `build.args = {...}` block.
	// docker-compose's `build.args` semantics — parameterise a generic
	// Dockerfile (SERVICE=api / VERSION=1.2.3 / etc) without committing
	// the value to the Dockerfile itself.
	for key, value := range spec.BuildArgs {
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

	fmt.Println("-----> generic build complete!")

	return nil
}

func (l *Generic) Deploy(appName string, spec *BuildSpec, releaseDir string) error {
	fmt.Println("-----> deploying generic application...")

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
		AppName:       appName,
		ImageTag:      "latest",
		EnvFile:       envFile,
		ReleaseDir:    releaseDir,
		ZeroDowntime:  true,
		HealthTimeout: 60,
		NetworkMode:   networkMode,
		DockerPorts:   spec.Ports,
		Volumes:       volumes,
	})
}

func (l *Generic) Restart(appName string, spec *BuildSpec) error {
	return restartContainer(appName)
}

func (l *Generic) Cleanup(appName string, spec *BuildSpec) error {
	return cleanupReleases(appName)
}

func (l *Generic) DetectLanguage(releaseDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(releaseDir, "Dockerfile")); err == nil {
		return "docker", nil
	}

	return "generic", nil
}

func (l *Generic) EnsureDockerfile(releaseDir string, appName string, spec *BuildSpec) error {
	dockerfilePath := filepath.Join(releaseDir, "Dockerfile")

	if _, err := os.Stat(dockerfilePath); err == nil {
		fmt.Println("-----> using existing dockerfile")
		return nil
	}

	return fmt.Errorf("no Dockerfile found and no language-specific strategy available")
}
