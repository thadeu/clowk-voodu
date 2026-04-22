// Package util provides cross-cutting helpers inherited from the Gokku
// codebase. Kept mostly mechanical during the Voodu port so behaviour is
// preserved while paths and env vars move to the Voodu scheme.
package util

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"go.voodu.clowk.in/internal/config"
	"go.voodu.clowk.in/internal/paths"
)

// RemoteInfo contains the parsed pieces of a Voodu git remote URL.
type RemoteInfo struct {
	Host    string
	BaseDir string
	App     string
}

// GetConfigPath returns the path to the client CLI configuration file.
func GetConfigPath() string {
	return paths.UserCfgPath()
}

// ExtractRemoteFlag extracts the --remote flag from arguments and returns the remote name and remaining args.
func ExtractRemoteFlag(args []string) (string, []string) {
	var remote string
	var remaining []string

	for i := 0; i < len(args); i++ {
		if args[i] == "--remote" && i+1 < len(args) {
			remote = args[i+1]
			i++
		} else {
			remaining = append(remaining, args[i])
		}
	}

	return remote, remaining
}

// ExtractAppFlag extracts the -a or --app flag from arguments and returns the app name and remaining args.
func ExtractAppFlag(args []string) (string, []string) {
	var app string
	var remaining []string

	for i := 0; i < len(args); i++ {
		if (args[i] == "-a" || args[i] == "--app") && i+1 < len(args) {
			app = args[i+1]
			i++
		} else {
			remaining = append(remaining, args[i])
		}
	}

	return app, remaining
}

// ExtractIdentityFlag extracts the --identity flag from arguments.
func ExtractIdentityFlag(args []string) (string, []string) {
	var identity string
	var remaining []string

	for i := 0; i < len(args); i++ {
		if args[i] == "--identity" && i+1 < len(args) {
			identity = args[i+1]
			i++
		} else {
			remaining = append(remaining, args[i])
		}
	}

	return identity, remaining
}

// GetRemoteInfoOrDefault extracts remote info using --remote flag or defaults to the Voodu remote.
// Returns nil if in server mode (local execution).
// Returns RemoteInfo if in client mode with valid remote.
func GetRemoteInfoOrDefault(args []string) (*RemoteInfo, []string, error) {
	if IsServerMode() {
		_, remainingArgs := ExtractRemoteFlag(args)

		return nil, remainingArgs, nil
	}

	remote, remainingArgs := ExtractRemoteFlag(args)

	if remote == "" {
		defaultRemote := paths.RemoteName

		info, err := GetRemoteInfo(defaultRemote)
		if err == nil {
			return info, remainingArgs, nil
		}

		return nil, remainingArgs, fmt.Errorf("no remote specified and default remote %q not found. Run 'voodu remote setup user@server_ip' first", defaultRemote)
	}

	remoteInfo, err := GetRemoteInfo(remote)
	if err != nil {
		return nil, remainingArgs, fmt.Errorf("remote '%s' not found: %v. Add it with: voodu remote add %s user@host", remote, err, remote)
	}

	return remoteInfo, remainingArgs, nil
}

// ExecuteRemoteCommand executes a command on the remote server via SSH.
// Automatically removes --remote flag from the command string.
func ExecuteRemoteCommand(remoteInfo *RemoteInfo, command string) error {
	if remoteInfo == nil {
		return fmt.Errorf("remoteInfo is nil")
	}

	cleanCommand := strings.Replace(command, " --remote", "", -1)
	cleanCommand = strings.Replace(cleanCommand, "--remote", "", -1)

	cmd := exec.Command("ssh", remoteInfo.Host, cleanCommand)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.Run()
}

// IsRunningOnServer returns true if running on the server environment.
func IsRunningOnServer() bool {
	return IsServerMode()
}

// GetVooduRcPath returns the path to the ~/.voodurc file.
func GetVooduRcPath() string {
	return paths.UserRCPath()
}

// ReadVooduRcMode reads the mode from ~/.voodurc file.
// Returns "client", "server", or empty string if file doesn't exist or is invalid.
func ReadVooduRcMode() string {
	rcPath := GetVooduRcPath()

	file, err := os.Open(rcPath)

	if err != nil {
		return ""
	}

	defer file.Close()

	scanner := bufio.NewScanner(file)

	var mode string

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		mode, _ = strings.CutPrefix(line, "mode=")
	}

	return mode
}

// IsClientMode returns true if running in client mode.
// Falls back to client mode if ~/.voodurc doesn't exist.
func IsClientMode() bool {
	mode := ReadVooduRcMode()

	if mode == "" {
		return true
	}

	return mode == "client"
}

// IsServerMode returns true if running in server mode.
// Falls back to client mode if ~/.voodurc doesn't exist.
func IsServerMode() bool {
	mode := ReadVooduRcMode()

	if mode == "" {
		return false
	}

	return mode == "server"
}

// ExtractFlagValue extracts a flag value from arguments.
func ExtractFlagValue(args []string, flag string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1]
		}
	}

	return ""
}

// ExtractAppName extracts app name from arguments (no environment concept).
func ExtractAppName(args []string) string {
	app, _ := ExtractAppFlag(args)

	return app
}

// IsSignalInterruption checks if the error is due to signal interruption.
func IsSignalInterruption(err error) bool {
	if err == nil {
		return false
	}

	if exitError, ok := err.(*os.SyscallError); ok {
		if exitError.Err == syscall.EINTR {
			return true
		}
	}

	return false
}

// IsRegistryImage checks if an image is from a registry (not a local build).
func IsRegistryImage(image string, customRegistries ...[]string) bool {
	if image == "" {
		return false
	}

	registryPatterns := []string{
		"ghcr.io/",
		"docker.io/",
		"registry.hub.docker.com/",
		"quay.io/",
		"gcr.io/",
		"us.gcr.io/",
		"eu.gcr.io/",
		"asia.gcr.io/",
		"k8s.gcr.io/",
		"registry.k8s.io/",
		"amazonaws.com/",
		"public.ecr.aws/",
		"registry.ecr.",
		"azurecr.io/",
		"registry.azurecr.io/",
		"registry.redhat.io/",
		"registry.access.redhat.com/",
		"registry.connect.redhat.com/",
		"registry.developers.redhat.com/",
	}

	if len(customRegistries) > 0 && len(customRegistries[0]) > 0 {
		for _, customRegistry := range customRegistries[0] {
			if !strings.HasSuffix(customRegistry, "/") {
				customRegistry += "/"
			}

			registryPatterns = append(registryPatterns, customRegistry)
		}
	}

	for _, pattern := range registryPatterns {
		if strings.Contains(image, pattern) {
			return true
		}
	}

	if strings.Contains(image, ":") && !strings.Contains(image, "/") {
		return false
	}

	parts := strings.Split(image, "/")
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		return true
	}

	return false
}

// GetCustomRegistries returns custom registries configured for an app.
func GetCustomRegistries(appName string) []string {
	if cfg, err := config.LoadServerConfigByApp(appName); err == nil && cfg.Docker != nil && len(cfg.Docker.Registry) > 0 {
		return cfg.Docker.Registry
	}

	return []string{}
}

// PullRegistryImage pulls a pre-built image from a registry.
func PullRegistryImage(image string) error {
	fmt.Printf("-----> Pulling pre-built image: %s\n", image)

	cmd := exec.Command("docker", "pull", image)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to pull image %s: %v", image, err)
	}

	fmt.Printf("-----> Successfully pulled image: %s\n", image)

	return nil
}

// TagImageForApp tags a pulled image with the app name for deployment.
func TagImageForApp(image, appName string) error {
	tag := fmt.Sprintf("%s:latest", appName)
	fmt.Printf("-----> Tagging image %s as %s\n", image, tag)

	cmd := exec.Command("docker", "tag", image, tag)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to tag image %s as %s: %v", image, tag, err)
	}

	fmt.Printf("-----> Successfully tagged image as %s\n", tag)

	return nil
}

// RunDockerBuildWithTimeout runs a Docker build command with timeout monitoring.
func RunDockerBuildWithTimeout(cmd *exec.Cmd, timeoutMinutes int) error {
	if timeoutMinutes <= 0 {
		timeoutMinutes = 30
	}

	timeout := time.Duration(timeoutMinutes) * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	buildStartTime := time.Now()
	fmt.Printf("-----> Starting Docker build (timeout: %d minutes)...\n", timeoutMinutes)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start docker build: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			elapsed := time.Since(buildStartTime)
			fmt.Printf("-----> Build timeout reached after %s\n", elapsed.Round(time.Second))
			fmt.Println("-----> Terminating build process...")

			if cmd.Process != nil {
				cmd.Process.Kill()
			}

			buildContext := "."
			if len(cmd.Args) > 0 {
				buildContext = cmd.Args[len(cmd.Args)-1]
			}

			if cmd.Dir != "" {
				buildContext = cmd.Dir
			}

			return fmt.Errorf("docker build timed out after %d minutes. The build may be stuck or taking too long.\nTroubleshooting:\n  - Check Docker resources: docker system df\n  - Check Docker daemon logs: journalctl -u docker\n  - Verify available disk space: df -h\n  - Check if Go build is consuming resources: docker stats\n  - Try building manually: docker build -t <image> %s", timeoutMinutes, buildContext)
		case err := <-done:
			elapsed := time.Since(buildStartTime)

			if err != nil {
				return fmt.Errorf("docker build failed after %s: %v", elapsed.Round(time.Second), err)
			}

			fmt.Printf("-----> Build completed successfully in %s\n", elapsed.Round(time.Second))

			return nil
		case <-ticker.C:
			elapsed := time.Since(buildStartTime)
			remaining := timeout - elapsed

			if remaining > 0 {
				fmt.Printf("-----> Build still running... (elapsed: %s, remaining: %s)\n", elapsed.Round(time.Second), remaining.Round(time.Second))
			}
		}
	}
}

// DetectRubyVersion detects Ruby version from .ruby-version or Gemfile.
func DetectRubyVersion(releaseDir string) string {
	rubyVersionPath := filepath.Join(releaseDir, ".ruby-version")

	if data, err := os.ReadFile(rubyVersionPath); err == nil {
		version := strings.TrimSpace(string(data))

		if version != "" {
			return fmt.Sprintf("ruby:%s", version)
		}
	}

	gemfilePath := filepath.Join(releaseDir, "Gemfile")

	if data, err := os.ReadFile(gemfilePath); err == nil {
		content := string(data)

		re := regexp.MustCompile(`ruby\s+["']([^"']+)["']`)
		matches := re.FindStringSubmatch(content)

		if len(matches) > 1 {
			version := matches[1]

			return fmt.Sprintf("ruby:%s", version)
		}
	}

	return "ruby:latest"
}

// DetectGoVersion detects Go version from go.mod.
func DetectGoVersion(releaseDir string) string {
	goModPath := filepath.Join(releaseDir, "go.mod")

	if data, err := os.ReadFile(goModPath); err == nil {
		content := string(data)

		re := regexp.MustCompile(`go\s+(\d+\.\d+)`)
		matches := re.FindStringSubmatch(content)

		if len(matches) > 1 {
			version := matches[1]

			return fmt.Sprintf("golang:%s-alpine", version)
		}
	}

	return "golang:latest"
}

// DetectNodeVersion detects Node.js version from .nvmrc or package.json.
func DetectNodeVersion(releaseDir string) string {
	nvmrcPath := filepath.Join(releaseDir, ".nvmrc")

	if data, err := os.ReadFile(nvmrcPath); err == nil {
		version := strings.TrimSpace(string(data))

		if version != "" {
			version = strings.TrimPrefix(version, "v")

			return fmt.Sprintf("node:%s", version)
		}
	}

	packageJsonPath := filepath.Join(releaseDir, "package.json")

	if data, err := os.ReadFile(packageJsonPath); err == nil {
		content := string(data)

		re := regexp.MustCompile(`"engines"\s*:\s*{[^}]*"node"\s*:\s*"([^"]+)"`)
		matches := re.FindStringSubmatch(content)

		if len(matches) > 1 {
			version := matches[1]

			re2 := regexp.MustCompile(`(\d+)`)

			if versionMatch := re2.FindStringSubmatch(version); len(versionMatch) > 1 {
				return fmt.Sprintf("node:%s", versionMatch[1])
			}
		}
	}

	return "node:latest"
}

// DetectPythonVersion returns latest Python version.
func DetectPythonVersion(releaseDir string) string {
	return "python:latest"
}

// LoadEnvFile loads environment variables from a file.
func LoadEnvFile(envFile string) map[string]string {
	envVars := make(map[string]string)

	content, err := os.ReadFile(envFile)

	if err != nil {
		if os.IsNotExist(err) {
			return envVars
		}

		fmt.Printf("Error reading file: %v\n", err)
		os.Exit(1)
	}

	lines := strings.Split(string(content), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)

		if len(parts) == 2 {
			envVars[parts[0]] = parts[1]
		}
	}

	return envVars
}

// SaveEnvFile saves environment variables to a file.
func SaveEnvFile(envFile string, envVars map[string]string) error {
	keys := make([]string, 0, len(envVars))

	for k := range envVars {
		keys = append(keys, k)
	}

	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}

	var content strings.Builder

	for _, key := range keys {
		content.WriteString(fmt.Sprintf("%s=%s\n", key, envVars[key]))
	}

	return os.WriteFile(envFile, []byte(content.String()), 0600)
}

// GetRemoteInfo extracts info from a git remote by name.
// Example: ubuntu@server:api
// Returns: RemoteInfo{Host: "ubuntu@server", BaseDir: <voodu root>, App: "api"}
func GetRemoteInfo(remoteName string) (*RemoteInfo, error) {
	cmd := exec.Command("git", "remote", "get-url", remoteName)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("remote '%s' not found: %w", remoteName, err)
	}

	remoteURL := strings.TrimSpace(string(output))

	parts := strings.Split(remoteURL, ":")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid remote URL format: %s", remoteURL)
	}

	return &RemoteInfo{
		Host:    parts[0],
		BaseDir: paths.Root(),
		App:     parts[1],
	}, nil
}
