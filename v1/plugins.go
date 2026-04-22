package v1

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.gokku-vm.com/pkg"
	"go.gokku-vm.com/pkg/util"
)

// PluginsCommand gerencia plugins
type PluginsCommand struct {
	output     Output
	pluginsDir string
}

// NewPluginsCommand cria uma nova instância de PluginsCommand
func NewPluginsCommand(output Output) *PluginsCommand {
	baseDir := os.Getenv("GOKKU_ROOT")
	if baseDir == "" {
		baseDir = "/opt/gokku"
	}
	return &PluginsCommand{
		output:     output,
		pluginsDir: filepath.Join(baseDir, "plugins"),
	}
}

// PluginInfo representa informações de um plugin
type PluginInfo struct {
	Name     string   `json:"name"`
	Commands []string `json:"commands,omitempty"`
}

// List lista todos os plugins instalados
func (c *PluginsCommand) List() error {
	if _, err := os.Stat(c.pluginsDir); os.IsNotExist(err) {
		c.output.Print("No plugins installed")
		return nil
	}

	entries, err := os.ReadDir(c.pluginsDir)
	if err != nil {
		c.output.Error(fmt.Sprintf("Error reading plugins directory: %v", err))
		return err
	}

	var plugins []PluginInfo
	for _, entry := range entries {
		if entry.IsDir() {
			commands, _ := c.getPluginCommands(entry.Name())
			plugins = append(plugins, PluginInfo{
				Name:     entry.Name(),
				Commands: commands,
			})
		}
	}

	if len(plugins) == 0 {
		c.output.Print("No plugins installed")
		return nil
	}

	// Para stdout, usar tabela
	if _, ok := c.output.(*StdoutOutput); ok {
		headers := []string{"Plugin Name", "Commands"}
		var rows [][]string
		for _, plugin := range plugins {
			rows = append(rows, []string{
				plugin.Name,
				strings.Join(plugin.Commands, ", "),
			})
		}
		c.output.Table(headers, rows)
	} else {
		// Para JSON, retornar array de objetos
		c.output.Data(plugins)
	}

	return nil
}

// Install instala um plugin (oficial ou via URL git)
func (c *PluginsCommand) Install(pluginName, pluginURL string) error {
	if c.pluginExists(pluginName) {
		c.output.Error(fmt.Sprintf("Plugin '%s' already exists", pluginName))
		return fmt.Errorf("plugin already exists")
	}

	// Se URL não for fornecida, tentar instalar plugin oficial
	if pluginURL == "" {
		return c.installOfficialPlugin(pluginName)
	}

	return c.installPluginFromGit(pluginURL, pluginName)
}

// Uninstall desinstala um plugin
func (c *PluginsCommand) Uninstall(pluginName string) error {
	if !c.pluginExists(pluginName) {
		c.output.Error(fmt.Sprintf("Plugin '%s' not found", pluginName))
		return fmt.Errorf("plugin not found")
	}

	pluginDir := filepath.Join(c.pluginsDir, pluginName)

	// Executar script de desinstalação se existir
	uninstallScript := filepath.Join(pluginDir, "bin", "uninstall")
	if _, err := os.Stat(uninstallScript); err == nil {
		c.output.Print(fmt.Sprintf("Running uninstall script for %s...", pluginName))
		cmd := exec.Command(uninstallScript)
		cmd.Dir = pluginDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			c.output.Print(fmt.Sprintf("Warning: uninstall script failed: %v", err))
		}
	}

	if err := os.RemoveAll(pluginDir); err != nil {
		c.output.Error(fmt.Sprintf("Failed to uninstall plugin: %v", err))
		return err
	}

	c.output.Success(fmt.Sprintf("Plugin '%s' uninstalled successfully", pluginName))
	return nil
}

// Update atualiza um plugin
func (c *PluginsCommand) Update(pluginName string) error {
	if !c.pluginExists(pluginName) {
		c.output.Error(fmt.Sprintf("Plugin '%s' not found", pluginName))
		return fmt.Errorf("plugin not found")
	}

	c.output.Print(fmt.Sprintf("Updating plugin '%s'...", pluginName))

	pluginDir := filepath.Join(c.pluginsDir, pluginName)
	gitURL, err := c.getPluginGitURL(pluginDir)
	if err != nil {
		c.output.Error(fmt.Sprintf("Failed to read plugin config: %v", err))
		return err
	}

	if gitURL == "" {
		c.output.Error("Plugin source URL not found in config.json")
		return fmt.Errorf("url not found")
	}

	// Clone para diretório temporário
	tempDir := pluginDir + ".tmp"
	defer os.RemoveAll(tempDir)

	if err := c.cloneRepository(gitURL, tempDir); err != nil {
		c.output.Error(fmt.Sprintf("Failed to clone repository: %v", err))
		return err
	}

	// Remover .git do temp
	os.RemoveAll(filepath.Join(tempDir, ".git"))

	// Overlay files
	if err := c.overlayDirectory(tempDir, pluginDir); err != nil {
		c.output.Error(fmt.Sprintf("Failed to update files: %v", err))
		return err
	}

	// Recriar config e permissões
	c.createPluginConfig(pluginDir, pluginName, gitURL)
	c.makeScriptsExecutable(pluginDir)
	c.afterInstallation(pluginDir)

	c.output.Success(fmt.Sprintf("Plugin '%s' updated successfully", pluginName))
	return nil
}

// ExecuteCommand executa um comando de plugin
func (c *PluginsCommand) ExecuteCommand(pluginName, command string, args []string) error {
	if !c.pluginExists(pluginName) {
		c.output.Error(fmt.Sprintf("Plugin '%s' not found", pluginName))
		return fmt.Errorf("plugin not found")
	}

	pluginDir := filepath.Join(c.pluginsDir, pluginName)
	var commandPath string

	// Try bin/ first, then commands/
	binPath := filepath.Join(pluginDir, "bin", command)
	if _, err := os.Stat(binPath); err == nil {
		commandPath = binPath
	} else {
		commandsPath := filepath.Join(pluginDir, "commands", command)
		if _, err := os.Stat(commandsPath); err == nil {
			commandPath = commandsPath
		}
	}

	if commandPath == "" {
		c.output.Error(fmt.Sprintf("Command '%s' not found for plugin '%s'", command, pluginName))
		return fmt.Errorf("command not found")
	}

	// Execute the plugin command
	cmd := exec.Command("bash", commandPath)
	cmd.Args = append(cmd.Args, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command failed: %w", err)
	}

	return nil
}

// Wildcard trata comandos dinâmicos de plugins (ex: postgres:export)
func (c *PluginsCommand) Wildcard(args []string) error {
	if len(args) == 0 {
		c.showHelp()
		return nil
	}

	// Extract --remote flag first (if present)
	remoteInfo, remainingArgs, err := util.GetRemoteInfoOrDefault(args)
	if err != nil {
		c.output.Error(fmt.Sprintf("Error: %v", err))
		return err
	}

	if len(remainingArgs) == 0 {
		if remoteInfo != nil {
			return c.executeRemote("gokku plugins list", remoteInfo)
		}
		c.showHelp()
		return nil
	}

	// Handle plugin:command format
	subcommand := remainingArgs[0]
	if strings.Contains(subcommand, ":") {
		parts := strings.Split(subcommand, ":")
		if len(parts) == 2 && parts[0] == "plugins" {
			subcommand = parts[1]
		}
	}

	// Check if subcommand is a flag
	if strings.HasPrefix(subcommand, "--") && subcommand != "--help" && subcommand != "--remote" {
		c.output.Error(fmt.Sprintf("Unknown plugin command: %s", subcommand))
		c.showHelp()
		return fmt.Errorf("unknown command")
	}

	switch subcommand {
	case "list", "ls":
		if remoteInfo != nil {
			return c.executeRemote("gokku plugins list", remoteInfo)
		}
		return c.List()
	case "add", "install":
		return c.handleInstall(remainingArgs[1:], remoteInfo)
	case "update":
		return c.handleUpdate(remainingArgs[1:], remoteInfo)
	case "remove", "uninstall":
		return c.handleUninstall(remainingArgs[1:], remoteInfo)
	default:
		// Try as plugin:command format (e.g., postgres:export)
		if remoteInfo != nil {
			cmd := fmt.Sprintf("gokku plugins %s", strings.Join(remainingArgs, " "))
			return c.executeRemote(cmd, remoteInfo)
		}
		return c.handlePluginCommand(remainingArgs)
	}
}

// Métodos privados para handlers CLI (com parsing de args e remote execution)

func (c *PluginsCommand) handleInstall(args []string, remoteInfo *pkg.RemoteInfo) error {
	if len(args) < 1 {
		c.showInstallHelp()
		return fmt.Errorf("plugin name required")
	}

	pluginName := args[0]
	var gitURL string
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		gitURL = args[1]
	}

	if remoteInfo != nil {
		cmdParts := []string{"gokku plugins:add", pluginName}
		if gitURL != "" {
			cmdParts = append(cmdParts, gitURL)
		}
		return c.executeRemote(strings.Join(cmdParts, " "), remoteInfo)
	}

	return c.Install(pluginName, gitURL)
}

func (c *PluginsCommand) handleUpdate(args []string, remoteInfo *pkg.RemoteInfo) error {
	if len(args) < 1 {
		c.showUpdateHelp()
		return fmt.Errorf("plugin name required")
	}

	pluginName := args[0]

	if remoteInfo != nil {
		cmd := fmt.Sprintf("gokku plugins:update %s", pluginName)
		return c.executeRemote(cmd, remoteInfo)
	}

	return c.Update(pluginName)
}

func (c *PluginsCommand) handleUninstall(args []string, remoteInfo *pkg.RemoteInfo) error {
	if len(args) < 1 {
		c.showUninstallHelp()
		return fmt.Errorf("plugin name required")
	}

	pluginName := args[0]

	if remoteInfo != nil {
		cmd := fmt.Sprintf("gokku plugins:remove %s", pluginName)
		return c.executeRemote(cmd, remoteInfo)
	}

	return c.Uninstall(pluginName)
}

func (c *PluginsCommand) handlePluginCommand(args []string) error {
	if len(args) == 0 {
		c.output.Error("Plugin command required")
		c.showHelp()
		return fmt.Errorf("command required")
	}

	// Parse: postgres:export postgres-api
	parts := strings.Split(args[0], ":")
	if len(parts) != 2 {
		c.output.Error(fmt.Sprintf("Unknown command: %s", args[0]))
		c.showHelp()
		return fmt.Errorf("invalid command format")
	}

	pluginName := parts[0]
	command := parts[1]
	commandArgs := args[1:]

	return c.ExecuteCommand(pluginName, command, commandArgs)
}

// Métodos auxiliares para execução remota

func (c *PluginsCommand) executeRemote(cmd string, remoteInfo *pkg.RemoteInfo) error {
	if err := util.ExecuteRemoteCommand(remoteInfo, cmd); err != nil {
		c.output.Error(fmt.Sprintf("Remote execution failed: %v", err))
		return err
	}
	return nil
}

// Métodos privados para instalação

func (c *PluginsCommand) installOfficialPlugin(pluginName string) error {
	c.output.Print(fmt.Sprintf("Installing official plugin '%s'...", pluginName))

	pluginDir := filepath.Join(c.pluginsDir, pluginName)
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		c.output.Error(fmt.Sprintf("Failed to create plugin directory: %v", err))
		return err
	}

	repoName := fmt.Sprintf("gokku-%s", pluginName)
	gitURL := fmt.Sprintf("https://github.com/gokku-vm/%s", repoName)

	if err := c.cloneRepository(gitURL, pluginDir); err != nil {
		os.RemoveAll(pluginDir)
		c.output.Error(fmt.Sprintf("Failed to clone official plugin: %v", err))
		return err
	}

	if err := c.finalizeInstallation(pluginDir, pluginName, gitURL); err != nil {
		return err
	}

	c.output.Success(fmt.Sprintf("Plugin '%s' installed successfully", pluginName))
	return nil
}

func (c *PluginsCommand) installPluginFromGit(gitURL, pluginName string) error {
	c.output.Print(fmt.Sprintf("Installing plugin '%s' from %s...", pluginName, gitURL))

	pluginDir := filepath.Join(c.pluginsDir, pluginName)
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		c.output.Error(fmt.Sprintf("Failed to create plugin directory: %v", err))
		return err
	}

	if err := c.cloneRepository(gitURL, pluginDir); err != nil {
		os.RemoveAll(pluginDir)
		c.output.Error(fmt.Sprintf("Failed to clone repository: %v", err))
		return err
	}

	if err := c.finalizeInstallation(pluginDir, pluginName, gitURL); err != nil {
		return err
	}

	c.output.Success(fmt.Sprintf("Plugin '%s' installed successfully", pluginName))
	return nil
}

func (c *PluginsCommand) finalizeInstallation(pluginDir, pluginName, gitURL string) error {
	if err := c.createPluginConfig(pluginDir, pluginName, gitURL); err != nil {
		c.output.Error(fmt.Sprintf("Failed to create config: %v", err))
		return err
	}

	if err := c.makeScriptsExecutable(pluginDir); err != nil {
		c.output.Error(fmt.Sprintf("Failed to make scripts executable: %v", err))
		return err
	}

	if err := c.afterInstallation(pluginDir); err != nil {
		c.output.Error(fmt.Sprintf("Post-installation script failed: %v", err))
		return err
	}

	return nil
}

func (c *PluginsCommand) cloneRepository(gitURL, targetDir string) error {
	cmd := exec.Command("git", "clone", "--depth", "1", gitURL, targetDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %v\nOutput: %s", err, string(output))
	}
	return nil
}

func (c *PluginsCommand) createPluginConfig(pluginDir, pluginName, gitURL string) error {
	configJSON := fmt.Sprintf(`{
  "name": "%s",
  "url": "%s"
}`, pluginName, gitURL)

	configPath := filepath.Join(pluginDir, "config.json")
	return os.WriteFile(configPath, []byte(configJSON), 0644)
}

func (c *PluginsCommand) makeScriptsExecutable(pluginDir string) error {
	// Make bin/install executable
	installPath := filepath.Join(pluginDir, "bin", "install")
	if _, err := os.Stat(installPath); err == nil {
		os.Chmod(installPath, 0755)
	}

	// Make bin/uninstall executable
	uninstallPath := filepath.Join(pluginDir, "bin", "uninstall")
	if _, err := os.Stat(uninstallPath); err == nil {
		os.Chmod(uninstallPath, 0755)
	}

	// Make commands executable
	commandsDir := filepath.Join(pluginDir, "commands")
	if _, err := os.Stat(commandsDir); err == nil {
		entries, err := os.ReadDir(commandsDir)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					os.Chmod(filepath.Join(commandsDir, entry.Name()), 0755)
				}
			}
		}
	}
	return nil
}

func (c *PluginsCommand) afterInstallation(pluginDir string) error {
	binInstallPath := filepath.Join(pluginDir, "bin", "install")
	if fi, err := os.Stat(binInstallPath); err == nil && !fi.IsDir() && fi.Mode()&0111 != 0 {
		c.output.Print("Running post-installation script...")
		cmd := exec.Command(binInstallPath)
		cmd.Dir = pluginDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return nil
}

func (c *PluginsCommand) pluginExists(pluginName string) bool {
	pluginPath := filepath.Join(c.pluginsDir, pluginName)
	_, err := os.Stat(pluginPath)
	return !os.IsNotExist(err)
}

func (c *PluginsCommand) getPluginCommands(pluginName string) ([]string, error) {
	commandsDir := filepath.Join(c.pluginsDir, pluginName, "commands")
	var commands []string

	entries, err := os.ReadDir(commandsDir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			commands = append(commands, entry.Name())
		}
	}
	return commands, nil
}

func (c *PluginsCommand) getPluginGitURL(pluginDir string) (string, error) {
	configPath := filepath.Join(pluginDir, "config.json")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}

	// Simple JSON parsing for "url" field
	configStr := string(configData)
	lines := strings.Split(configStr, "\n")
	for _, line := range lines {
		if strings.Contains(line, `"url":`) {
			parts := strings.Split(line, `"url": "`)
			if len(parts) > 1 {
				url := strings.TrimSuffix(strings.TrimSpace(parts[1]), `"`)
				return url, nil
			}
		}
	}

	return "", nil
}

func (c *PluginsCommand) overlayDirectory(srcDir, dstDir string) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		dstPath := filepath.Join(dstDir, relPath)
		if info.IsDir() {
			return os.MkdirAll(dstPath, info.Mode())
		}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return err
		}
		srcData, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dstPath, srcData, info.Mode())
	})
}

// Métodos de ajuda

func (c *PluginsCommand) showHelp() {
	c.output.Print("Plugin management commands:")
	c.output.Print("")
	c.output.Print("  gokku plugins:list                    List all installed plugins")
	c.output.Print("  gokku plugins:add <name>              Add official plugin")
	c.output.Print("  gokku plugins:add <name> <git-url>    Add community plugin")
	c.output.Print("  gokku plugins:update <plugin>        Update plugin from source")
	c.output.Print("  gokku plugins:remove <plugin>         Remove plugin")
	c.output.Print("")
	c.output.Print("Plugin commands:")
	c.output.Print("  gokku <plugin>:<command> <service>     Execute plugin command")
	c.output.Print("")
	c.output.Print("Examples:")
	c.output.Print("  gokku plugins:add nginx                              # Official plugin")
	c.output.Print("  gokku plugins:add aws https://github.com/user/gokku-aws  # Community plugin")
	c.output.Print("  gokku plugins:update redis                           # Update plugin")
}

func (c *PluginsCommand) showInstallHelp() {
	c.output.Print("Usage: gokku plugins:add <plugin-name> [<git-url>] [--remote]")
	c.output.Print("")
	c.output.Print("Examples:")
	c.output.Print("  gokku plugins:add nginx                              # Official plugin")
	c.output.Print("  gokku plugins:add myplugin https://github.com/user/gokku-myplugin  # Community plugin")
	c.output.Print("  gokku plugins:add nginx --remote                    # Install on remote server")
	c.output.Print("")
	c.output.Print("Official plugins are automatically fetched from gokku-vm organization")
	c.output.Print("Community plugins require a git URL")
}

func (c *PluginsCommand) showUpdateHelp() {
	c.output.Print("Usage: gokku plugins:update <plugin-name> [--remote]")
	c.output.Print("")
	c.output.Print("Examples:")
	c.output.Print("  gokku plugins:update redis")
	c.output.Print("  gokku plugins:update redis --remote")
}

func (c *PluginsCommand) showUninstallHelp() {
	c.output.Print("Usage: gokku plugins:remove <plugin-name> [--remote]")
	c.output.Print("")
	c.output.Print("Examples:")
	c.output.Print("  gokku plugins:remove nginx")
	c.output.Print("  gokku plugins:remove nginx --remote")
}
