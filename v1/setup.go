package v1

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"go.gokku-vm.com/pkg/git"
)

// SetupCommand gerencia configuração inicial do servidor
type SetupCommand struct {
	output       Output
	serverHost   string
	identityFile string
}

// NewSetupCommand cria uma nova instância de SetupCommand
func NewSetupCommand(output Output, serverHost, identityFile string) *SetupCommand {
	return &SetupCommand{
		output:       output,
		serverHost:   serverHost,
		identityFile: identityFile,
	}
}

// Execute executa a configuração completa do servidor (movido de internal/services/setup.go)
func (c *SetupCommand) Execute() error {
	c.output.Print(fmt.Sprintf("-----> Setting up server %s...", c.serverHost))
	c.output.Print("")

	// Phase 1: Check prerequisites
	c.output.Print("-----> Checking prerequisites...")

	if err := c.checkSSHConnection(); err != nil {
		c.output.Error(fmt.Sprintf("%v\n\nMake sure you can SSH to the server without password.\nYou can use: ssh-copy-id %s", err, c.serverHost))
		return err
	}

	// Check if Gokku is already installed
	c.output.Print("-----> Checking if Gokku is installed...")

	// Phase 2: Install Gokku
	c.output.Print("-----> Installing Gokku on server...")
	if err := c.installGokku(); err != nil {
		c.output.Error(fmt.Sprintf("error installing Gokku: %v", err))
		return err
	}
	c.output.Print("-----> Gokku installed successfully")

	// Phase 3: Install essential plugins
	c.output.Print("-----> Installing essential plugins...")
	essentialPlugins := []string{"nginx", "letsencrypt", "cron", "postgres", "redis"}
	installedCount := 0

	for _, pluginName := range essentialPlugins {
		c.output.Print(fmt.Sprintf("-----> Installing %s...", pluginName))

		if err := c.installPlugin(pluginName); err != nil {
			c.output.Print(fmt.Sprintf(" failed: %v", err))
		} else {
			c.output.Print(" ✓")
			installedCount++
		}
	}

	c.output.Print(fmt.Sprintf("-----> Installed %d/%d plugins", installedCount, len(essentialPlugins)))

	// Phase 4: Configure SSH keys (if needed)
	c.output.Print("-----> Configuring SSH keys...")

	if err := c.configureSSHKeys(); err != nil {
		c.output.Print(fmt.Sprintf("Warning: Could not configure SSH keys: %v", err))
	} else {
		c.output.Print("-----> SSH keys configured ✓")
	}

	// Phase 5: Final verification
	c.output.Print("-----> Verifying setup...")

	if err := c.verifySetup(); err != nil {
		c.output.Print(fmt.Sprintf("Warning: Verification failed: %v", err))
	} else {
		c.output.Print("-----> Setup verification complete ✓")
	}

	// Phase 6: Create default remote "gokku" locally
	c.output.Print("-----> Creating default remote 'gokku'...")

	if err := c.createDefaultRemote(); err != nil {
		c.output.Print(fmt.Sprintf("Note: Could not create default remote 'gokku': %v", err))
		c.output.Print("You can create it manually with:")
		c.output.Print(fmt.Sprintf("  gokku remote add gokku %s", c.serverHost))
	} else {
		c.output.Print("-----> Created default remote 'gokku' for future use")
	}

	c.output.Print("")
	c.output.Success("Setup complete!")
	c.output.Print("-----> Server is ready to receive deployments")
	c.output.Print("")
	c.output.Print("Next steps:")
	c.output.Print("  gokku apps create <app_name> [--remote]")
	c.output.Print("")
	c.output.Print("Example:")
	c.output.Print("  gokku apps create api-production")
	c.output.Print(fmt.Sprintf("  gokku remote add api-production %s", c.serverHost))
	c.output.Print("  git push api-production main")

	return nil
}

// Métodos privados (movidos de internal/services/setup.go)

func (c *SetupCommand) buildSSHArgs(extraArgs ...string) []string {
	args := []string{}

	if c.identityFile != "" {
		if _, err := os.Stat(c.identityFile); err == nil {
			args = append(args, "-i", c.identityFile)
		}
	}

	var sshOptions []string
	var commands []string

	i := 0

	for i < len(extraArgs) {
		arg := extraArgs[i]

		if arg == "-o" || arg == "-i" {
			sshOptions = append(sshOptions, arg)
			i++
			if i < len(extraArgs) {
				sshOptions = append(sshOptions, extraArgs[i])
			}
		} else if arg == "-t" || strings.HasPrefix(arg, "-") {
			sshOptions = append(sshOptions, arg)
		} else {
			commands = append(commands, arg)
		}
		i++
	}

	args = append(args, sshOptions...)
	args = append(args, c.serverHost)
	args = append(args, commands...)

	return args
}

func (c *SetupCommand) checkSSHConnection() error {
	args := c.buildSSHArgs("-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "echo OK")
	cmd := exec.Command("ssh", args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		outputStr := strings.TrimSpace(string(output))

		if outputStr != "" {
			return fmt.Errorf("SSH connection failed: %v\nOutput: %s", err, outputStr)
		}
		return fmt.Errorf("SSH connection failed: %v", err)
	}

	outputStr := strings.TrimSpace(string(output))
	if !strings.Contains(outputStr, "OK") {
		return fmt.Errorf("SSH connection test failed (received: %s)", outputStr)
	}

	return nil
}

func (c *SetupCommand) installGokku() error {
	installCmd := `bash -c 'curl -fsSL https://gokku-vm.com/install | bash -s -- --server'`

	args := c.buildSSHArgs("-t", installCmd)
	cmd := exec.Command("ssh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.Run()
}

func (c *SetupCommand) installPlugin(pluginName string) error {
	checkCmd := fmt.Sprintf(`test -d /opt/gokku/plugins/%s && echo 'exists' || echo 'missing'`, pluginName)

	args := c.buildSSHArgs(checkCmd)
	cmd := exec.Command("ssh", args...)
	output, err := cmd.Output()

	if err == nil && strings.Contains(string(output), "exists") {
		return nil
	}

	installCmd := fmt.Sprintf(`gokku plugins:add %s 2>&1`, pluginName)

	args = c.buildSSHArgs(installCmd)
	sshCmd := exec.Command("ssh", args...)
	output, err = sshCmd.CombinedOutput()

	if err != nil {
		outputStr := string(output)
		if strings.Contains(outputStr, "already exists") {
			return nil
		}
		return fmt.Errorf("%s", strings.TrimSpace(outputStr))
	}

	return nil
}

func (c *SetupCommand) configureSSHKeys() error {
	if c.identityFile != "" {
		return nil
	}

	localUser, err := user.Current()

	if err != nil {
		return fmt.Errorf("could not get current user: %v", err)
	}

	keyPaths := []string{
		filepath.Join(localUser.HomeDir, ".ssh", "id_ed25519.pub"),
		filepath.Join(localUser.HomeDir, ".ssh", "id_rsa.pub"),
		filepath.Join(localUser.HomeDir, ".ssh", "id_ecdsa.pub"),
	}

	var publicKeyPath string
	for _, path := range keyPaths {
		if _, err := os.Stat(path); err == nil {
			publicKeyPath = path
			break
		}
	}

	if publicKeyPath == "" {
		return fmt.Errorf("no SSH public key found")
	}

	publicKeyData, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return fmt.Errorf("could not read public key: %v", err)
	}

	publicKey := strings.TrimSpace(string(publicKeyData))

	checkCmd := fmt.Sprintf(`grep -Fx "%s" ~/.ssh/authorized_keys >/dev/null 2>&1 && echo 'exists' || echo 'missing'`, publicKey)

	args := c.buildSSHArgs(checkCmd)
	cmd := exec.Command("ssh", args...)
	output, err := cmd.Output()

	if err == nil && strings.Contains(string(output), "exists") {
		return nil
	}

	addKeyCmd := fmt.Sprintf(`mkdir -p ~/.ssh && echo "%s" >> ~/.ssh/authorized_keys && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys`, publicKey)
	args = c.buildSSHArgs(addKeyCmd)
	addCmd := exec.Command("ssh", args...)

	if err := addCmd.Run(); err != nil {
		return fmt.Errorf("could not add SSH key: %v", err)
	}

	return nil
}

func (c *SetupCommand) verifySetup() error {
	c.output.Print("-----> Checking Docker...")

	args := c.buildSSHArgs("docker ps >/dev/null 2>&1 && echo 'OK' || echo 'FAIL'")
	dockerCmd := exec.Command("ssh", args...)
	dockerOutput, err := dockerCmd.Output()

	if err == nil {
		dockerStatus := strings.TrimSpace(string(dockerOutput))
		if dockerStatus != "OK" {
			c.output.Print("-----> Docker: Not running (this is OK if Docker was just installed)")
		}
	}

	c.output.Print("-----> Checking plugins...")

	args = c.buildSSHArgs("ls -1 /opt/gokku/plugins 2>/dev/null | wc -l")
	pluginsCmd := exec.Command("ssh", args...)
	pluginsOutput, err := pluginsCmd.Output()

	if err == nil {
		pluginCount := strings.TrimSpace(string(pluginsOutput))
		c.output.Print(fmt.Sprintf("-----> Plugins installed: %s", pluginCount))
	}

	c.output.Print("-----> Checking directories...")

	args = c.buildSSHArgs(`test -d /opt/gokku && echo 'OK' || echo 'FAIL'`)
	dirsCmd := exec.Command("ssh", args...)
	dirsOutput, err := dirsCmd.Output()

	if err == nil {
		dirsStatus := strings.TrimSpace(string(dirsOutput))
		if dirsStatus != "OK" {
			c.output.Print("-----> Directories: Missing /opt/gokku")
			return fmt.Errorf("base directory not found")
		}
	}

	return nil
}

func (c *SetupCommand) createDefaultRemote() error {
	client := &git.GitClient{}

	if _, err := client.GetRemoteURL("gokku"); err == nil {
		return nil
	}

	remoteURL := fmt.Sprintf("%s:gokku.git", c.serverHost)

	_, err := client.AddRemote("gokku", remoteURL)

	if err != nil {
		return fmt.Errorf("failed to add remote: %v", err)
	}

	return nil
}
