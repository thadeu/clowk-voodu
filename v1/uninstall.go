package v1

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"go.gokku-vm.com/pkg/util"
)

type RunUninstallCommand struct {
	output  Output
	baseDir string
}

func NewRunUninstallCommand(output Output) *RunUninstallCommand {
	baseDir := os.Getenv("GOKKU_ROOT")
	if baseDir == "" {
		baseDir = "/opt/gokku"
	}

	return &RunUninstallCommand{
		output:  output,
		baseDir: baseDir,
	}
}

// handleUninstall removes Gokku installation
func (c *RunUninstallCommand) UseUninstall(args []string) {
	// Check if running on server or client
	isServerMode := util.IsServerMode()

	fmt.Println("-----> Uninstalling Gokku...")
	fmt.Println("")

	if isServerMode {
		c.handleServerUninstall()
	} else {
		c.handleClientUninstall()
	}
}

// handleServerUninstall removes Gokku from server
func (c *RunUninstallCommand) handleServerUninstall() {
	fmt.Println("This will remove:")
	fmt.Println("  - Gokku binary from /usr/local/bin/gokku")
	fmt.Println("  - All applications in /opt/gokku/apps/")
	fmt.Println("  - All repositories in /opt/gokku/repos/")
	fmt.Println("  - All plugins in /opt/gokku/plugins/")
	fmt.Println("  - All services in /opt/gokku/services/")
	fmt.Println("  - All scripts in /opt/gokku/scripts/")
	fmt.Println("  - Configuration file ~/.gokkurc")
	fmt.Println("")
	fmt.Print("Are you sure? Type 'yes' to continue: ")

	var response string
	fmt.Scanln(&response)
	if response != "yes" {
		fmt.Println("Aborted.")
		return
	}

	baseDir := "/opt/gokku"

	// Stop and remove all containers with Gokku label
	fmt.Println("-----> Stopping and removing Gokku containers...")
	stopContainersCmd := exec.Command("docker", "ps", "-aq", "--filter", "label=createdby=gokku")
	output, err := stopContainersCmd.Output()
	if err == nil && len(output) > 0 {
		containerIDs := strings.Fields(strings.TrimSpace(string(output)))
		for _, containerID := range containerIDs {
			// Stop container
			exec.Command("docker", "stop", containerID).Run()
			// Remove container
			exec.Command("docker", "rm", "-f", containerID).Run()
		}
	}

	// Also remove containers from /opt/gokku/apps (by name)
	fmt.Println("-----> Stopping and removing application containers...")
	if appsDir := filepath.Join(baseDir, "apps"); c.filepathExists(appsDir) {
		apps, _ := filepath.Glob(filepath.Join(appsDir, "*"))
		for _, appDir := range apps {
			appName := filepath.Base(appDir)
			// Stop containers (app and app-green for blue/green)
			for _, containerName := range []string{appName, appName + "-green", appName + "-blue"} {
				exec.Command("docker", "stop", containerName).Run()
				exec.Command("docker", "rm", "-f", containerName).Run()
				exec.Command("docker", "rmi", "-f", containerName).Run()
			}
		}
	}

	// Remove service containers from /opt/gokku/services
	fmt.Println("-----> Stopping and removing service containers...")
	if servicesDir := filepath.Join(baseDir, "services"); c.filepathExists(servicesDir) {
		services, _ := filepath.Glob(filepath.Join(servicesDir, "*"))
		for _, serviceDir := range services {
			serviceName := filepath.Base(serviceDir)
			exec.Command("docker", "stop", serviceName).Run()
			exec.Command("docker", "rm", "-f", serviceName).Run()
		}
	}

	// Remove base directory
	fmt.Println("-----> Removing /opt/gokku directory...")
	if _, err := os.Stat(baseDir); err == nil {
		removeDirCmd := exec.Command("sudo", "rm", "-rf", baseDir)
		removeDirCmd.Stdout = os.Stdout
		removeDirCmd.Stderr = os.Stderr
		if err := removeDirCmd.Run(); err != nil {
			fmt.Printf("Warning: Failed to remove %s: %v\n", baseDir, err)
		} else {
			fmt.Printf("-----> Removed %s\n", baseDir)
		}
	}

	// Remove binary
	fmt.Println("-----> Removing gokku binary...")
	binaryPath := "/usr/local/bin/gokku"
	if _, err := os.Stat(binaryPath); err == nil {
		removeBinaryCmd := exec.Command("sudo", "rm", "-f", binaryPath)
		removeBinaryCmd.Stdout = os.Stdout
		removeBinaryCmd.Stderr = os.Stderr
		if err := removeBinaryCmd.Run(); err != nil {
			fmt.Printf("Warning: Failed to remove binary: %v\n", err)
		} else {
			fmt.Printf("-----> Removed %s\n", binaryPath)
		}
	}

	// Remove config file
	fmt.Println("-----> Removing configuration file...")
	localUser, err := user.Current()
	if err == nil {
		rcPath := filepath.Join(localUser.HomeDir, ".gokkurc")
		if err := os.Remove(rcPath); err == nil {
			fmt.Printf("-----> Removed %s\n", rcPath)
		}
	}

	fmt.Println("")
	fmt.Println("-----> Gokku uninstalled successfully!")
	fmt.Println("")
	fmt.Println("Note: Docker and Docker images are not removed.")
	fmt.Println("Note: User was added to docker group (not removed).")
}

// handleClientUninstall removes Gokku from client machine
func (c *RunUninstallCommand) handleClientUninstall() {
	fmt.Println("This will remove:")
	fmt.Println("  - Gokku binary from /usr/local/bin/gokku")
	fmt.Println("  - Configuration directory ~/.gokku/")
	fmt.Println("  - Configuration file ~/.gokkurc")
	fmt.Println("")
	fmt.Print("Are you sure? Type 'yes' to continue: ")

	var response string
	fmt.Scanln(&response)
	if response != "yes" {
		fmt.Println("Aborted.")
		return
	}

	localUser, err := user.Current()
	if err != nil {
		fmt.Printf("Error: Could not get current user: %v\n", err)
		os.Exit(1)
	}

	// Remove binary
	fmt.Println("-----> Removing gokku binary...")
	binaryPath := "/usr/local/bin/gokku"
	if _, err := os.Stat(binaryPath); err == nil {
		removeBinaryCmd := exec.Command("sudo", "rm", "-f", binaryPath)
		removeBinaryCmd.Stdout = os.Stdout
		removeBinaryCmd.Stderr = os.Stderr
		if err := removeBinaryCmd.Run(); err != nil {
			fmt.Printf("Warning: Failed to remove binary: %v\n", err)
		} else {
			fmt.Printf("-----> Removed %s\n", binaryPath)
		}
	}

	// Remove config directory
	fmt.Println("-----> Removing configuration directory...")
	configDir := filepath.Join(localUser.HomeDir, ".gokku")
	if _, err := os.Stat(configDir); err == nil {
		if err := os.RemoveAll(configDir); err != nil {
			fmt.Printf("Warning: Failed to remove config directory: %v\n", err)
		} else {
			fmt.Printf("-----> Removed %s\n", configDir)
		}
	}

	// Remove config file
	fmt.Println("-----> Removing configuration file...")
	rcPath := filepath.Join(localUser.HomeDir, ".gokkurc")
	if err := os.Remove(rcPath); err == nil {
		fmt.Printf("-----> Removed %s\n", rcPath)
	}

	fmt.Println("")
	fmt.Println("-----> Gokku uninstalled successfully!")
}

// filepathExists checks if a file path exists
func (c *RunUninstallCommand) filepathExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}
