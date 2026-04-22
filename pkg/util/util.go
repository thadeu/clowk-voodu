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

	"go.gokku-vm.com/pkg"
	"go.gokku-vm.com/pkg/git"
)

// GetConfigPath returns the path to the configuration file
func GetConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gokku", "config.yml")
}

// ExtractRemoteFlag extracts the --remote flag from arguments and returns the remote name and remaining args
func ExtractRemoteFlag(args []string) (string, []string) {
	var remote string
	var remaining []string

	for i := 0; i < len(args); i++ {
		if args[i] == "--remote" && i+1 < len(args) {
			remote = args[i+1]
			i++ // Skip next arg
		} else {
			remaining = append(remaining, args[i])
		}
	}

	return remote, remaining
}

// ExtractAppFlag extracts the -a or --app flag from arguments and returns the app name and remaining args
func ExtractAppFlag(args []string) (string, []string) {
	var app string
	var remaining []string

	for i := 0; i < len(args); i++ {
		if (args[i] == "-a" || args[i] == "--app") && i+1 < len(args) {
			app = args[i+1]
			i++ // Skip next arg
		} else {
			remaining = append(remaining, args[i])
		}
	}

	return app, remaining
}

// ExtractIdentityFlag extracts the --identity flag from arguments
func ExtractIdentityFlag(args []string) (string, []string) {
	var identity string
	var remaining []string

	for i := 0; i < len(args); i++ {
		if args[i] == "--identity" && i+1 < len(args) {
			identity = args[i+1]
			i++ // Skip next arg
		} else {
			remaining = append(remaining, args[i])
		}
	}

	return identity, remaining
}

// GetRemoteInfoOrDefault extracts remote info using --remote flag or defaults to "gokku"
// Returns nil if in server mode (local execution)
// Returns RemoteInfo if in client mode with valid remote
// Uses RemoteInfo from config.go
func GetRemoteInfoOrDefault(args []string) (*pkg.RemoteInfo, []string, error) {
	// If in server mode, return nil (execute locally)
	if IsServerMode() {
		// Remove --remote from args if present
		_, remainingArgs := ExtractRemoteFlag(args)
		return nil, remainingArgs, nil
	}

	// Client mode: extract --remote flag
	remote, remainingArgs := ExtractRemoteFlag(args)

	// If no remote specified, try to use "gokku" as default
	if remote == "" {
		// Try to get "gokku" remote first
		gokkuRemoteInfo, err := GetRemoteInfo("gokku")

		if err == nil {
			// Remote "gokku" exists, use it
			return gokkuRemoteInfo, remainingArgs, nil
		}

		// No "gokku" remote found, return error
		return nil, remainingArgs, fmt.Errorf("no remote specified and default remote 'gokku' not found. Run 'gokku remote setup user@server_ip' first")
	}

	// Remote specified, get its info
	remoteInfo, err := GetRemoteInfo(remote)
	if err != nil {
		return nil, remainingArgs, fmt.Errorf("remote '%s' not found: %v. Add it with: gokku remote add %s user@host", remote, err, remote)
	}

	return remoteInfo, remainingArgs, nil
}

// ExecuteRemoteCommand executes a command on remote server via SSH
// Automatically removes --remote flag from the command string
func ExecuteRemoteCommand(remoteInfo *pkg.RemoteInfo, command string) error {
	if remoteInfo == nil {
		return fmt.Errorf("remoteInfo is nil")
	}

	// Remove --remote flag from command string before executing
	cleanCommand := strings.Replace(command, " --remote", "", -1)
	cleanCommand = strings.Replace(cleanCommand, "--remote", "", -1)

	cmd := exec.Command("ssh", remoteInfo.Host, cleanCommand)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// IsRunningOnServer returns true if running on the server environment
// Uses ~/.gokkurc file to determine mode, with fallback to client mode if file doesn't exist
func IsRunningOnServer() bool {
	return IsServerMode()
}

// GetGokkuRcPath returns the path to the ~/.gokkurc file
func GetGokkuRcPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gokkurc")
}

// ReadGokkuRcMode reads the mode from ~/.gokkurc file
// Returns "client", "server", or empty string if file doesn't exist or is invalid
func ReadGokkuRcMode() string {
	rcPath := GetGokkuRcPath()

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

// IsClientMode returns true if running in client mode
// Falls back to client mode if ~/.gokkurc doesn't exist
func IsClientMode() bool {
	mode := ReadGokkuRcMode()

	if mode == "" {
		// If file doesn't exist, assume client mode (fallback)
		return true
	}

	return mode == "client"
}

// IsServerMode returns true if running in server mode
// Falls back to client mode if ~/.gokkurc doesn't exist
func IsServerMode() bool {
	mode := ReadGokkuRcMode()

	if mode == "" {
		// If file doesn't exist, assume client mode (fallback)
		return false
	}

	return mode == "server"
}

// ExtractFlagValue extracts a flag value from arguments
func ExtractFlagValue(args []string, flag string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// ExtractAppName extracts app name from arguments (no environment concept)
func ExtractAppName(args []string) string {
	app, _ := ExtractAppFlag(args)
	return app
}

// IsSignalInterruption checks if the error is due to signal interruption
func IsSignalInterruption(err error) bool {
	if err == nil {
		return false
	}

	// Check if it's a signal error
	if exitError, ok := err.(*os.SyscallError); ok {
		if exitError.Err == syscall.EINTR {
			return true
		}
	}

	return false
}

// IsRegistryImage checks if an image is from a registry (not a local build)
func IsRegistryImage(image string, customRegistries ...[]string) bool {
	if image == "" {
		return false
	}

	// Default registry patterns
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

	// Add custom registries if provided
	if len(customRegistries) > 0 && len(customRegistries[0]) > 0 {
		for _, customRegistry := range customRegistries[0] {
			// Ensure custom registry ends with / for pattern matching
			if !strings.HasSuffix(customRegistry, "/") {
				customRegistry += "/"
			}
			registryPatterns = append(registryPatterns, customRegistry)
		}
	}

	// Check if image contains any registry pattern
	for _, pattern := range registryPatterns {
		if strings.Contains(image, pattern) {
			return true
		}
	}

	// Check if image contains a port (e.g., registry.example.com:5000)
	if strings.Contains(image, ":") && !strings.Contains(image, "/") {
		// This might be a local registry with port
		return false
	}

	// Check if image contains a domain-like pattern (contains dots and slashes)
	parts := strings.Split(image, "/")
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		return true
	}

	return false
}

// GetCustomRegistries returns custom registries configured for an app
func GetCustomRegistries(appName string) []string {
	// Load server config for the specific app
	if config, err := pkg.LoadServerConfigByApp(appName); err == nil && config.Docker != nil && len(config.Docker.Registry) > 0 {
		// Return the list of registries from docker.registry config
		return config.Docker.Registry
	}

	// Fallback to empty list if no config found
	return []string{}
}

// PullRegistryImage pulls a pre-built image from a registry
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

// TagImageForApp tags a pulled image with the app name for deployment
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

// RunDockerBuildWithTimeout runs a Docker build command with timeout monitoring
func RunDockerBuildWithTimeout(cmd *exec.Cmd, timeoutMinutes int) error {
	if timeoutMinutes <= 0 {
		timeoutMinutes = 30 // Default 30 minutes
	}

	timeout := time.Duration(timeoutMinutes) * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	buildStartTime := time.Now()
	fmt.Printf("-----> Starting Docker build (timeout: %d minutes)...\n", timeoutMinutes)

	// Start command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start docker build: %v", err)
	}

	// Monitor progress
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	// Progress ticker - log every 30 seconds to show we're still working
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Timeout reached
			elapsed := time.Since(buildStartTime)
			fmt.Printf("-----> Build timeout reached after %s\n", elapsed.Round(time.Second))
			fmt.Println("-----> Terminating build process...")
			if cmd.Process != nil {
				cmd.Process.Kill()
			}

			// Extract build context from command args
			buildContext := "."
			if len(cmd.Args) > 0 {
				// Last argument is usually the build context
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
			// Log progress every 30 seconds
			elapsed := time.Since(buildStartTime)
			remaining := timeout - elapsed

			if remaining > 0 {
				fmt.Printf("-----> Build still running... (elapsed: %s, remaining: %s)\n", elapsed.Round(time.Second), remaining.Round(time.Second))
			}
		}
	}
}

// DetectRubyVersion detects Ruby version from .ruby-version or Gemfile
func DetectRubyVersion(releaseDir string) string {
	// Try .ruby-version first
	rubyVersionPath := filepath.Join(releaseDir, ".ruby-version")

	if data, err := os.ReadFile(rubyVersionPath); err == nil {
		version := strings.TrimSpace(string(data))
		if version != "" {
			return fmt.Sprintf("ruby:%s", version)
		}
	}

	// Try Gemfile
	gemfilePath := filepath.Join(releaseDir, "Gemfile")

	if data, err := os.ReadFile(gemfilePath); err == nil {
		content := string(data)

		// Look for ruby version in Gemfile
		re := regexp.MustCompile(`ruby\s+["']([^"']+)["']`)
		matches := re.FindStringSubmatch(content)

		if len(matches) > 1 {
			version := matches[1]
			// Convert version format (e.g., "3.1" -> "3.1")
			return fmt.Sprintf("ruby:%s", version)
		}
	}

	// Fallback to latest Ruby
	return "ruby:latest"
}

// DetectGoVersion detects Go version from go.mod
func DetectGoVersion(releaseDir string) string {
	goModPath := filepath.Join(releaseDir, "go.mod")

	if data, err := os.ReadFile(goModPath); err == nil {
		content := string(data)

		// Look for go version in go.mod
		re := regexp.MustCompile(`go\s+(\d+\.\d+)`)
		matches := re.FindStringSubmatch(content)

		if len(matches) > 1 {
			version := matches[1]
			return fmt.Sprintf("golang:%s-alpine", version)
		}
	}

	// Fallback to latest Go
	return "golang:latest"
}

// DetectNodeVersion detects Node.js version from .nvmrc or package.json
func DetectNodeVersion(releaseDir string) string {
	// Try .nvmrc first
	nvmrcPath := filepath.Join(releaseDir, ".nvmrc")

	if data, err := os.ReadFile(nvmrcPath); err == nil {
		version := strings.TrimSpace(string(data))

		if version != "" {
			// Remove 'v' prefix if present
			version = strings.TrimPrefix(version, "v")
			return fmt.Sprintf("node:%s", version)
		}
	}

	// Try package.json
	packageJsonPath := filepath.Join(releaseDir, "package.json")

	if data, err := os.ReadFile(packageJsonPath); err == nil {
		content := string(data)

		// Look for engines.node in package.json
		re := regexp.MustCompile(`"engines"\s*:\s*{[^}]*"node"\s*:\s*"([^"]+)"`)
		matches := re.FindStringSubmatch(content)

		if len(matches) > 1 {
			version := matches[1]
			// Remove version constraints (e.g., ">=18.0.0" -> "18")
			re2 := regexp.MustCompile(`(\d+)`)

			if versionMatch := re2.FindStringSubmatch(version); len(versionMatch) > 1 {
				return fmt.Sprintf("node:%s", versionMatch[1])
			}
		}
	}

	// Fallback to latest Node.js
	return "node:latest"
}

// DetectPythonVersion returns latest Python version (as requested)
func DetectPythonVersion(releaseDir string) string {
	// Always use latest Python as fallback
	return "python:latest"
}

// LoadEnvFile loads environment variables from a file
func LoadEnvFile(envFile string) map[string]string {
	envVars := make(map[string]string)

	content, err := os.ReadFile(envFile)

	if err != nil {
		if os.IsNotExist(err) {
			return envVars // Return empty map if file doesn't exist
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

// SaveEnvFile saves environment variables to a file
func SaveEnvFile(envFile string, envVars map[string]string) error {
	// Sort keys
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

// GetRemoteInfo extracts info from git remote
// Example: ubuntu@server:api
// Returns: RemoteInfo{Host: "ubuntu@server", BaseDir: "/opt/gokku", App: "api"}
func GetRemoteInfo(remoteName string) (*pkg.RemoteInfo, error) {
	remoteInfo, err := git.GetRemoteInfoWithClient(&git.GitClient{}, remoteName)
	if err != nil {
		return nil, err
	}
	return &pkg.RemoteInfo{
		Host:    remoteInfo.Host,
		BaseDir: remoteInfo.BaseDir,
		App:     remoteInfo.App,
	}, nil
}
