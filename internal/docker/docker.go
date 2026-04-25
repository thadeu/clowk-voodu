// Package docker wraps the `docker` CLI invocations Voodu uses for build,
// deploy and container lifecycle management. Ported from the Gokku codebase
// with path/label rebranding.
package docker

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	VooduLabelKey   = "createdby"
	VooduLabelValue = "voodu"
)

// GetVooduLabels returns the docker --label arguments that mark a container
// as being managed by Voodu.
func GetVooduLabels() []string {
	return []string{fmt.Sprintf("%s=%s", VooduLabelKey, VooduLabelValue)}
}

// ContainerInfo represents information about a running container. Inherited
// from the Gokku codebase — combines docker ps output and the registry shape.
type ContainerInfo struct {
	ID      string `json:"ID"`
	Names   string `json:"Names"`
	Image   string `json:"Image"`
	Status  string `json:"Status"`
	Ports   string `json:"Ports"`
	Command string `json:"Command"`
	Created string `json:"CreatedAt"`

	Name         string `json:"name"`
	AppName      string `json:"app_name"`
	ProcessType  string `json:"process_type"`
	Number       int    `json:"number"`
	HostPort     int    `json:"host_port"`
	InternalPort int    `json:"internal_port"`
	CreatedAt    string `json:"created_at"`
}

type ContainerConfig struct {
	Name    string
	Image   string
	Ports   []string
	EnvFile string

	// NetworkMode is the legacy single-network knob — kept for the
	// build-driven deploy path (DeploymentConfig) which only ever joins
	// one network. Controller-managed deployments set Networks instead.
	NetworkMode string

	// Networks, when non-empty, wins over NetworkMode. The first entry
	// becomes --network at `docker run`; additional entries are attached
	// to the running container via `docker network connect`. This is the
	// only way to get a container onto more than one bridge (docker
	// doesn't accept --network multiple times).
	Networks []string

	RestartPolicy string
	Volumes       []string
	WorkingDir    string
	Command       []string

	// Labels are extra `--label k=v` flags appended after the umbrella
	// createdby=voodu marker. Controller-managed containers populate
	// this with the structured voodu.* identity (scope, name, kind,
	// replica_id) so the reconciler can list-by-labels instead of
	// parsing names. Build-mode and the legacy DeploymentConfig path
	// can leave it empty — the umbrella label still lands.
	Labels []string

	// AutoRemove sets `--rm` so the container is deleted as soon as the
	// process exits. Used by job-kind runs that should not leave
	// stopped artifacts behind. Defaults to false (long-running
	// deployments must persist across restarts).
	AutoRemove bool
}

// DeploymentConfig represents deployment configuration.
type DeploymentConfig struct {
	AppName       string
	ImageTag      string
	EnvFile       string
	ReleaseDir    string
	ZeroDowntime  bool
	HealthTimeout int
	NetworkMode   string
	DockerPorts   []string
	Volumes       []string
}

// CreateContainer creates a new container with the given configuration.
func CreateContainer(cfg ContainerConfig) error {
	args := []string{"run", "-d", "--name", cfg.Name}

	if cfg.AutoRemove {
		args = append(args, "--rm")
	}

	for _, label := range GetVooduLabels() {
		args = append(args, "--label", label)
	}

	// Caller-supplied labels (typically voodu.scope=…, voodu.name=…,
	// voodu.kind=…). The umbrella createdby=voodu above stays for the
	// existing ListContainers filter; structured labels ride alongside.
	for _, label := range cfg.Labels {
		args = append(args, "--label", label)
	}

	if cfg.RestartPolicy != "" {
		args = append(args, "--restart", cfg.RestartPolicy)
	}

	// NetworkMode (host/none) wins when explicitly set — it bypasses
	// docker's bridge stack entirely, so Networks is irrelevant. The
	// handler validates mutual exclusivity before we get here; this
	// branch just picks which field to trust. Otherwise fall through
	// to the bridge path: first Networks entry becomes --network, and
	// extras are attached post-run via docker network connect.
	primaryNet := ""
	if cfg.NetworkMode != "" {
		primaryNet = cfg.NetworkMode
	} else if len(cfg.Networks) > 0 {
		primaryNet = cfg.Networks[0]
	}

	if primaryNet != "" {
		args = append(args, "--network", primaryNet)
	}

	// Port publishing is meaningless (and rejected with a warning by
	// modern docker) when the container shares the host's net stack:
	// in host mode the container's listening ports ARE the host's
	// ports, so there's no NAT rule to install. Same for `none` mode
	// where the container has no network namespace at all. Gokku had
	// this same guard (legacy/pkg/docker.go) — keep it to avoid the
	// "Published ports are discarded" warning + confused operators.
	if cfg.NetworkMode != "host" && cfg.NetworkMode != "none" {
		for _, port := range cfg.Ports {
			args = append(args, "-p", port)
		}
	}

	if cfg.EnvFile != "" {
		args = append(args, "--env-file", cfg.EnvFile)
	}

	for _, volume := range cfg.Volumes {
		args = append(args, "-v", volume)
	}

	if cfg.WorkingDir != "" {
		args = append(args, "-w", cfg.WorkingDir)
	}

	args = append(args, "--ulimit", "nofile=65536:65536")
	args = append(args, "--ulimit", "nproc=4096:4096")

	args = append(args, cfg.Image)

	if len(cfg.Command) > 0 {
		args = append(args, cfg.Command...)
	}

	cmd := exec.Command("docker", args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("failed to create container %s: %v, output: %s", cfg.Name, err, string(output))
	}

	// Attach any secondary networks. Docker only accepts one --network
	// at create time, so fanning out here is the only path to multi-
	// homed containers. If a connect fails we roll the container back —
	// a half-joined container is worse than no container because it
	// silently misses whichever network the operator declared.
	//
	// Skip entirely in host/none mode: attaching a bridge to a
	// host-networked container is either rejected by docker or silently
	// ignored, and either way it's operator confusion we don't want.
	if cfg.NetworkMode == "" && len(cfg.Networks) > 1 {
		for _, net := range cfg.Networks[1:] {
			if net == "" || net == primaryNet {
				continue
			}

			if err := ConnectNetwork(cfg.Name, net); err != nil {
				_ = RemoveContainer(cfg.Name, true)
				return fmt.Errorf("failed to connect container %s to network %s: %w", cfg.Name, net, err)
			}
		}
	}

	return nil
}

// ConnectNetwork attaches an existing container to an additional docker
// network. Used by CreateContainer to fan out multi-network specs, but
// exported so the reconciler (or future drift logic) can rewire
// membership without recreating the container.
func ConnectNetwork(container, network string) error {
	cmd := exec.Command("docker", "network", "connect", network, container)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker network connect %s %s: %v, output: %s", network, container, err, string(output))
	}

	return nil
}

// ListContainers returns list of containers in JSON format.
// By default, only lists containers with Voodu labels to avoid conflicts.
func ListContainers(all bool) ([]ContainerInfo, error) {
	return ListContainersFiltered(all, nil)
}

// ListContainersFiltered runs `docker ps` scoped to voodu-managed
// containers (via createdby=voodu) plus any extra label filters the
// caller supplies.
//
// Each entry in extraLabels is a `key=value` pair appended as
// `--filter label=…`. Docker AND-combines multiple label filters, so
// passing `[voodu.kind=deployment, voodu.scope=softphone]` returns
// only the deployment containers in that scope. Used by the
// reconciler (ListByIdentity) and `voodu get pods`.
func ListContainersFiltered(all bool, extraLabels []string) ([]ContainerInfo, error) {
	args := []string{"ps"}

	if all {
		args = append(args, "-a")
	}

	args = append(args, "--format", "json", "--filter", fmt.Sprintf("label=%s=%s", VooduLabelKey, VooduLabelValue))

	for _, lbl := range extraLabels {
		if lbl == "" {
			continue
		}

		args = append(args, "--filter", "label="+lbl)
	}

	cmd := exec.Command("docker", args...)
	output, err := cmd.Output()

	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %v", err)
	}

	var containers []ContainerInfo
	lines := strings.Split(string(output), "\n")

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var container ContainerInfo

		if err := json.Unmarshal([]byte(line), &container); err != nil {
			continue
		}

		containers = append(containers, container)
	}

	return containers, nil
}

// InspectLabels returns the labels of a single container as a flat
// map. Used by the reconciler / `voodu describe` to recover the
// structured voodu.* identity. `docker ps --format json` does not
// emit labels, so this is the second hop.
//
// Returns (nil, nil) when the container does not exist — distinct
// from (nil, err) which is a real failure to inspect.
func InspectLabels(name string) (map[string]string, error) {
	if !ContainerExists(name) {
		return nil, nil
	}

	cmd := exec.Command("docker", "inspect", name, "--format", "{{json .Config.Labels}}")

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect %s labels: %w", name, err)
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return map[string]string{}, nil
	}

	var labels map[string]string

	if err := json.Unmarshal([]byte(trimmed), &labels); err != nil {
		return nil, fmt.Errorf("parse labels for %s: %w", name, err)
	}

	return labels, nil
}

// ContainerExists checks if a container exists.
// First checks containers with Voodu labels, then falls back to direct name check for backwards compatibility.
func ContainerExists(name string) bool {
	containers, err := ListContainers(true)
	if err == nil {
		for _, container := range containers {
			if strings.Contains(container.Name, name) {
				return true
			}
		}
	}

	cmd := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}", "--filter", fmt.Sprintf("name=%s", name))
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == name {
			return true
		}
	}

	return false
}

// GetContainerImage returns the image tag of the named container via
// `docker inspect`. Empty string + nil error means no such container;
// callers distinguish "not running" from "unreadable" by checking the
// error.
func GetContainerImage(name string) (string, error) {
	if !ContainerExists(name) {
		return "", nil
	}

	cmd := exec.Command("docker", "inspect", name, "--format", "{{.Config.Image}}")

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", name, err)
	}

	return strings.TrimSpace(string(out)), nil
}

// GetContainerImageID returns the image ID (sha256) the container was
// created from. Distinct from GetContainerImage (which returns the tag)
// because tags are mutable: `vd-web:latest` today is a different image
// than `vd-web:latest` after a rebuild. The ID is the stable identity
// and the only reliable way to detect "tag got rewritten, container
// needs to restart from the new image".
//
// Empty string + nil error means no such container.
func GetContainerImageID(name string) (string, error) {
	if !ContainerExists(name) {
		return "", nil
	}

	cmd := exec.Command("docker", "inspect", name, "--format", "{{.Image}}")

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", name, err)
	}

	return strings.TrimSpace(string(out)), nil
}

// GetImageID resolves a tag (or any image reference) to its current
// sha256 ID. Pair with GetContainerImageID to detect build-mode drift:
// after `docker build -t vd-web:latest`, the tag points at a new ID
// but existing containers still reference the old one.
//
// Empty string + nil error means the image doesn't exist locally.
func GetImageID(ref string) (string, error) {
	cmd := exec.Command("docker", "inspect", ref, "--format", "{{.Id}}")

	out, err := cmd.Output()
	if err != nil {
		// Exit code 1 with "No such object" on stderr is the common
		// "image not pulled/built yet" case — treat it as non-fatal so
		// the caller can transient-retry.
		if strings.Contains(string(out), "No such") {
			return "", nil
		}

		return "", nil
	}

	return strings.TrimSpace(string(out)), nil
}

// ContainerIsRunning checks if a container is currently running.
func ContainerIsRunning(name string) bool {
	cmd := exec.Command("docker", "ps", "--format", "{{.Names}}", "--filter", fmt.Sprintf("name=%s", name))
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == name {
			return true
		}
	}

	return false
}

// StopContainer stops a running container.
func StopContainer(name string) error {
	cmd := exec.Command("docker", "stop", name)

	return cmd.Run()
}

// IsRunning reports whether the container is currently in the running
// state. Distinct from ContainerExists: a stopped container exists but
// is not running, and the reconciler treats that as "needs a start".
// Returns (false, nil) when the container doesn't exist — the caller
// can decide whether that's an error in its context.
func IsRunning(name string) (bool, error) {
	cmd := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", name)

	out, err := cmd.Output()
	if err != nil {
		// `docker inspect` exits non-zero when the container is missing.
		// Treat "missing" as "not running" and let the caller distinguish
		// via ContainerExists if it cares.
		return false, nil
	}

	return strings.TrimSpace(string(out)) == "true", nil
}

// StartContainer runs `docker start` on an existing, stopped container.
// Used by the reconciler's replay path to recover containers whose
// restart policy was "no" and which therefore stayed dead after a host
// reboot.
func StartContainer(name string) error {
	cmd := exec.Command("docker", "start", name)

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker start %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}

	return nil
}

// UpdateRestartPolicy sets the restart policy on an existing container.
// Used to repair containers that were created before voodu defaulted
// to "unless-stopped" — those containers wouldn't survive a reboot,
// and we want the next reconcile to fix them in place without a
// destructive recreate.
func UpdateRestartPolicy(name, policy string) error {
	cmd := exec.Command("docker", "update", "--restart", policy, name)

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker update --restart %s %s: %v: %s", policy, name, err, strings.TrimSpace(string(out)))
	}

	return nil
}

// WaitContainer blocks until the named container exits and returns the
// process exit code. Backed by `docker wait`, which prints the integer
// exit code on stdout when the container terminates. Used by the job
// runner: jobs are containers we spawn synchronously and observe to
// completion.
//
// Returns an error when docker can't wait on the container (already
// removed, daemon down) or when the output isn't a parseable integer
// — the caller treats that as a failed run distinct from a non-zero
// exit, since we can't trust any value.
func WaitContainer(name string) (int, error) {
	cmd := exec.Command("docker", "wait", name)

	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("docker wait %s: %w", name, err)
	}

	trimmed := strings.TrimSpace(string(out))

	code, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("parse exit code from docker wait %s: %q: %w", name, trimmed, err)
	}

	return code, nil
}

// RemoveContainer removes a container.
func RemoveContainer(name string, force bool) error {
	args := []string{"rm"}

	if force {
		args = append(args, "-f")
	}

	args = append(args, name)

	cmd := exec.Command("docker", args...)

	return cmd.Run()
}

// GetContainerPort extracts port from environment file.
func GetContainerPort(envFile string, defaultPort int) int {
	if !fileExists(envFile) {
		return defaultPort
	}

	content, err := os.ReadFile(envFile)

	if err != nil {
		return defaultPort
	}

	lines := strings.Split(string(content), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "PORT=") {
			portStr := strings.TrimSpace(strings.TrimPrefix(line, "PORT="))

			if port, err := strconv.Atoi(portStr); err == nil {
				return port
			}
		}
	}

	return defaultPort
}

// IsZeroDowntimeEnabled checks if zero downtime deployment is enabled.
func IsZeroDowntimeEnabled(envFile string) bool {
	if !fileExists(envFile) {
		return true
	}

	content, err := os.ReadFile(envFile)

	if err != nil {
		return true
	}

	lines := strings.Split(string(content), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "ZERO_DOWNTIME=") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "ZERO_DOWNTIME="))

			switch strings.ToLower(value) {
			case "0", "false", "no", "off", "n":
				return false
			case "1", "true", "yes", "on", "y":
				return true
			default:
				return true
			}
		}
	}

	return true
}

// WaitForContainerHealth waits for container to be healthy.
func WaitForContainerHealth(name string, timeout int) error {
	startTime := time.Now()
	maxWait := time.Duration(timeout) * time.Second

	fmt.Printf("-----> Waiting for container to be healthy (max %ds)...\n", timeout)

	for {
		if time.Since(startTime) > maxWait {
			return fmt.Errorf("container failed to become healthy within %ds", timeout)
		}

		cmd := exec.Command("docker", "inspect", name, "--format", "{{.State.Health.Status}}")
		output, err := cmd.Output()
		if err != nil {
			time.Sleep(3 * time.Second)
			fmt.Println("-----> Container ready (no health check configured)")

			return nil
		}

		status := strings.TrimSpace(string(output))
		elapsed := int(time.Since(startTime).Seconds())

		switch status {
		case "healthy":
			fmt.Println("-----> Container is healthy!")

			return nil
		case "starting":
			fmt.Printf("       Starting... (%d/%ds)\n", elapsed, timeout)
			time.Sleep(2 * time.Second)
		case "unhealthy":
			logCmd := exec.Command("docker", "logs", name)
			logOutput, _ := logCmd.Output()

			return fmt.Errorf("container is unhealthy, logs: %s", string(logOutput))
		default:
			time.Sleep(2 * time.Second)
		}
	}
}

// StandardDeploy performs standard deployment (kill and restart).
func StandardDeploy(cfg DeploymentConfig) error {
	fmt.Println("=====> Starting Standard Deployment")

	containerName := cfg.AppName

	if ContainerExists(containerName) {
		fmt.Printf("-----> Stopping old container: %s\n", containerName)

		if err := StopContainer(containerName); err != nil {
			fmt.Printf("Warning: Failed to stop container: %v\n", err)
		}

		if err := RemoveContainer(containerName, true); err != nil {
			fmt.Printf("Warning: Failed to remove container: %v\n", err)
		}

		time.Sleep(2 * time.Second)
	}

	containerPort := GetContainerPort(cfg.EnvFile, 0)

	containerConfig := ContainerConfig{
		Name:          containerName,
		Image:         fmt.Sprintf("%s:%s", cfg.AppName, cfg.ImageTag),
		NetworkMode:   cfg.NetworkMode,
		RestartPolicy: "no",
		WorkingDir:    "/app",
		Volumes:       []string{fmt.Sprintf("%s:/app", cfg.ReleaseDir)},
	}

	if len(cfg.Volumes) > 0 {
		containerConfig.Volumes = append(containerConfig.Volumes, cfg.Volumes...)
		fmt.Printf("-----> Adding %d custom volumes\n", len(cfg.Volumes))
	}

	if cfg.NetworkMode != "host" {
		if len(cfg.DockerPorts) > 0 {
			containerConfig.Ports = cfg.DockerPorts
			fmt.Println("-----> Using ports from voodu.yml")
		} else if containerPort > 0 {
			containerConfig.Ports = []string{fmt.Sprintf("%d:%d", containerPort, containerPort)}
		}
	} else {
		fmt.Println("-----> Using host network (all ports exposed)")
	}

	if fileExists(cfg.EnvFile) {
		containerConfig.EnvFile = cfg.EnvFile
	}

	fmt.Printf("-----> Starting new container: %s\n", containerName)

	if err := CreateContainer(containerConfig); err != nil {
		return fmt.Errorf("failed to start container: %v", err)
	}

	fmt.Println("-----> Waiting for container to be ready...")
	time.Sleep(5 * time.Second)

	if !ContainerIsRunning(containerName) {
		logCmd := exec.Command("docker", "logs", containerName)
		logOutput, _ := logCmd.Output()

		return fmt.Errorf("container failed to start, logs: %s", string(logOutput))
	}

	fmt.Println("=====> Standard Deployment Complete!")
	fmt.Printf("-----> Active container: %s\n", containerName)
	fmt.Printf("-----> Running image: %s\n", cfg.ImageTag)

	return nil
}

// BlueGreenDeploy performs blue/green deployment.
func BlueGreenDeploy(cfg DeploymentConfig) error {
	fmt.Println("=====> Starting Blue/Green Deployment")

	containerPort := GetContainerPort(cfg.EnvFile, 0)

	if err := startGreenContainer(cfg, containerPort); err != nil {
		return fmt.Errorf("failed to start green container: %v", err)
	}

	if err := WaitForContainerHealth(cfg.AppName+"-green", cfg.HealthTimeout); err != nil {
		StopContainer(cfg.AppName + "-green")
		RemoveContainer(cfg.AppName+"-green", true)

		return fmt.Errorf("green container failed health check: %v", err)
	}

	activeContainerName := cfg.AppName

	if ContainerExists(activeContainerName) {
		if err := switchTrafficBlueToGreen(cfg.AppName, containerPort); err != nil {
			StopContainer(cfg.AppName + "-green")
			RemoveContainer(cfg.AppName+"-green", true)

			return fmt.Errorf("failed to switch traffic: %v", err)
		}

		cleanupOldBlueContainer(cfg.AppName)
	} else {
		fmt.Println("-----> First deployment, activating green")

		if err := renameContainer(cfg.AppName+"-green", activeContainerName); err != nil {
			return fmt.Errorf("failed to rename green to active: %v", err)
		}

		updateContainerRestartPolicy(activeContainerName, "always")
	}

	fmt.Println("=====> Blue/Green Deployment Complete!")
	fmt.Printf("-----> Active container: %s\n", activeContainerName)
	fmt.Printf("-----> Running image: %s\n", cfg.ImageTag)

	return nil
}

// DeployContainer determines and executes deployment strategy.
func DeployContainer(cfg DeploymentConfig) error {
	if IsZeroDowntimeEnabled(cfg.EnvFile) {
		fmt.Println("=====> ZERO_DOWNTIME deployment enabled")

		return BlueGreenDeploy(cfg)
	}

	fmt.Println("=====> ZERO_DOWNTIME deployment disabled")

	return StandardDeploy(cfg)
}

func fileExists(filename string) bool {
	_, err := os.Stat(filename)

	return !os.IsNotExist(err)
}

func startGreenContainer(cfg DeploymentConfig, containerPort int) error {
	greenName := cfg.AppName + "-green"
	fmt.Printf("-----> Starting green container: %s\n", greenName)

	if ContainerExists(greenName) {
		fmt.Println("       Removing old green container...")
		StopContainer(greenName)
		RemoveContainer(greenName, true)
	}

	containerConfig := ContainerConfig{
		Name:          greenName,
		Image:         fmt.Sprintf("%s:%s", cfg.AppName, cfg.ImageTag),
		NetworkMode:   cfg.NetworkMode,
		RestartPolicy: "unless-stopped",
		WorkingDir:    "/app",
		Volumes:       []string{fmt.Sprintf("%s:/app", cfg.ReleaseDir)},
	}

	if len(cfg.Volumes) > 0 {
		containerConfig.Volumes = append(containerConfig.Volumes, cfg.Volumes...)
		fmt.Printf("-----> Adding %d custom volumes to green container\n", len(cfg.Volumes))
	}

	if cfg.NetworkMode != "host" {
		if len(cfg.DockerPorts) > 0 {
			containerConfig.Ports = cfg.DockerPorts
		} else {
			containerConfig.Ports = []string{fmt.Sprintf("%d:%d", containerPort, containerPort)}
		}
	}

	if fileExists(cfg.EnvFile) {
		containerConfig.EnvFile = cfg.EnvFile
	}

	if err := CreateContainer(containerConfig); err != nil {
		return fmt.Errorf("failed to start green container: %v", err)
	}

	fmt.Printf("-----> Green container started (%s)\n", greenName)

	return nil
}

func switchTrafficBlueToGreen(appName string, containerPort int) error {
	activeName := appName
	greenName := appName + "-green"

	fmt.Println("-----> Switching traffic: active → green")

	if ContainerIsRunning(activeName) {
		fmt.Println("       Pausing active container...")
		cmd := exec.Command("docker", "pause", activeName)
		cmd.Run()
		time.Sleep(2 * time.Second)
	}

	fmt.Println("       Swapping container names...")

	if ContainerExists(activeName) {
		renameContainer(activeName, activeName+"-old")
	}

	if err := renameContainer(greenName, activeName); err != nil {
		return fmt.Errorf("failed to rename green container to active: %v", err)
	}

	updateContainerRestartPolicy(activeName, "always")

	fmt.Println("-----> Traffic switch complete (green → active)")

	return nil
}

func cleanupOldBlueContainer(appName string) {
	oldActiveName := appName + "-old"

	fmt.Println("-----> Cleaning up old active container...")

	if ContainerExists(oldActiveName) {
		fmt.Println("       Waiting 5s before removing old container...")
		time.Sleep(5 * time.Second)

		fmt.Println("       Removing old active container...")
		StopContainer(oldActiveName)
		RemoveContainer(oldActiveName, true)

		fmt.Println("-----> Old container cleaned up")
	}
}

func renameContainer(oldName, newName string) error {
	cmd := exec.Command("docker", "rename", oldName, newName)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("failed to rename container %s to %s: %v, output: %s", oldName, newName, err, string(output))
	}

	return nil
}

func updateContainerRestartPolicy(containerName, policy string) {
	cmd := exec.Command("docker", "update", "--restart", policy, containerName)
	cmd.Run()
}

// RecreateActiveContainer recreates the active container with a new env
// file. Used when secrets rotate — docker reads --env-file at container
// create time only, so a plain `docker restart` would not pick up the
// change; we have to destroy and recreate.
//
// Image, network mode, volumes, and port mappings are preserved from the
// running container via `docker inspect`. That's deliberate: the live
// container is the source of truth for "what's currently serving", and
// re-reading a declarative spec here risks diverging from what actually
// got deployed.
func RecreateActiveContainer(appName, envFile, appDir string) error {
	var activeContainer string

	if ContainerExists(appName) {
		activeContainer = appName
	} else if ContainerExists(appName + "-green") {
		activeContainer = appName + "-green"
	} else {
		return fmt.Errorf("no active container found for %s", appName)
	}

	fmt.Printf("-----> Recreating container: %s\n", activeContainer)

	inspected, err := inspectContainerShape(activeContainer)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", activeContainer, err)
	}

	fmt.Printf("       Using image: %s\n", inspected.Image)
	fmt.Printf("       Network mode: %s\n", inspected.NetworkMode)

	fmt.Println("       Stopping old container...")

	StopContainer(activeContainer)
	RemoveContainer(activeContainer, true)

	containerConfig := ContainerConfig{
		Name:          activeContainer,
		Image:         inspected.Image,
		NetworkMode:   inspected.NetworkMode,
		RestartPolicy: "always",
		WorkingDir:    "/app",
		Volumes:       inspected.Volumes,
		Ports:         inspected.Ports,
	}

	if len(containerConfig.Volumes) == 0 {
		containerConfig.Volumes = []string{fmt.Sprintf("%s:/app", appDir)}
	}

	if fileExists(envFile) {
		containerConfig.EnvFile = envFile
	}

	fmt.Println("       Starting new container with updated configuration...")

	if err := CreateContainer(containerConfig); err != nil {
		return fmt.Errorf("failed to recreate container: %v", err)
	}

	fmt.Println("Container recreated successfully with new environment")

	return nil
}

// containerShape captures the bits of a live container we need to
// recreate it identically minus its env.
type containerShape struct {
	Image       string
	NetworkMode string
	Ports       []string
	Volumes     []string
}

// inspectContainerShape pulls image, network mode, port bindings and
// bind-mount volumes out of a running container via `docker inspect`.
func inspectContainerShape(name string) (*containerShape, error) {
	cmd := exec.Command("docker", "inspect", name)

	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var raw []struct {
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`

		HostConfig struct {
			NetworkMode  string                           `json:"NetworkMode"`
			Binds        []string                         `json:"Binds"`
			PortBindings map[string][]struct{ HostIP, HostPort string } `json:"PortBindings"`
		} `json:"HostConfig"`
	}

	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, fmt.Errorf("parse docker inspect: %w", err)
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("no container data for %s", name)
	}

	entry := raw[0]

	shape := &containerShape{
		Image:       entry.Config.Image,
		NetworkMode: entry.HostConfig.NetworkMode,
		Volumes:     entry.HostConfig.Binds,
	}

	if shape.NetworkMode == "" {
		shape.NetworkMode = "bridge"
	}

	for containerSpec, bindings := range entry.HostConfig.PortBindings {
		containerPort := strings.SplitN(containerSpec, "/", 2)[0]

		for _, b := range bindings {
			if b.HostPort == "" {
				continue
			}

			if b.HostIP != "" {
				shape.Ports = append(shape.Ports, fmt.Sprintf("%s:%s:%s", b.HostIP, b.HostPort, containerPort))
			} else {
				shape.Ports = append(shape.Ports, fmt.Sprintf("%s:%s", b.HostPort, containerPort))
			}
		}
	}

	return shape, nil
}

// BlueGreenRollback performs rollback to previous blue container.
func BlueGreenRollback(appName string) error {
	blueName := appName + "-blue"
	oldBlueName := appName + "-blue-old"

	fmt.Println("=====> Starting Blue/Green Rollback")

	if !ContainerExists(oldBlueName) {
		return fmt.Errorf("no previous blue container found for rollback")
	}

	fmt.Println("-----> Stopping current blue container...")
	StopContainer(blueName)

	fmt.Println("-----> Restoring previous blue container...")

	if err := renameContainer(oldBlueName, blueName); err != nil {
		return fmt.Errorf("failed to restore previous blue container: %v", err)
	}

	fmt.Println("-----> Starting previous blue container...")
	cmd := exec.Command("docker", "start", blueName)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("failed to start previous blue container: %v, output: %s", err, string(output))
	}

	time.Sleep(5 * time.Second)

	fmt.Println("=====> Blue/Green Rollback Complete!")
	fmt.Printf("-----> Active container: %s\n", blueName)

	return nil
}
