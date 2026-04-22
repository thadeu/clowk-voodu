package v1

import (
	"fmt"
	"os"
	"strings"

	"go.gokku-vm.com/pkg/git"
	"go.gokku-vm.com/pkg/util"
)

type RunRemoteCommand struct {
	output  Output
	baseDir string
}

func NewRunRemoteCommand(output Output) *RunRemoteCommand {
	baseDir := os.Getenv("GOKKU_ROOT")

	if baseDir == "" {
		baseDir = "/opt/gokku"
	}

	return &RunRemoteCommand{
		output:  output,
		baseDir: baseDir,
	}
}

func (c *RunRemoteCommand) Use(args []string) {
	if len(args) < 1 {
		c.printRemoteHelp()
		os.Exit(1)
	}

	subcommand := args[0]

	if strings.Contains(subcommand, ":") {
		parts := strings.Split(subcommand, ":")

		if len(parts) == 2 && parts[0] == "remote" {
			subcommand = parts[1]
		}
	}

	switch subcommand {
	case "add":
		c.handleRemoteAdd(args[1:])
	case "list", "ls":
		c.handleRemoteList()
	case "remove", "rm":
		c.handleRemoteRemove(args[1:])
	case "setup":
		c.handleRemoteSetup(args[1:])
	case "help", "--help", "-h":
		c.printRemoteHelp()
	default:
		fmt.Printf("Unknown remote command: %s\n", subcommand)
		c.printRemoteHelp()
		os.Exit(1)
	}
}

func (c *RunRemoteCommand) handleRemoteAdd(args []string) {
	if len(args) < 2 {
		fmt.Println("Usage: gokku remote add <label> <remote_app_name[optional]> <user@server_ip>")
		fmt.Println("Example: gokku remote add api user@hostname")
		os.Exit(1)
	}

	var remoteAppName, serverHost, label string

	label = args[0]
	remoteAppName = label

	if len(args) == 2 {
		serverHost = args[1]
	} else if len(args) == 3 {
		remoteAppName = args[1]
		serverHost = args[2]
	}

	// Validate app name
	if remoteAppName == "" {
		fmt.Println("Error: app name cannot be empty")
		os.Exit(1)
	}

	// Validate server host format
	if !strings.Contains(serverHost, "@") {
		fmt.Println("Error: server host must be in format user@host")
		fmt.Println("Example: user@hostname")
		os.Exit(1)
	}

	gitc := &git.GitClient{}

	// Create git remote URL
	remoteURL := fmt.Sprintf("%s:%s", serverHost, remoteAppName)

	// Check if remote already exists
	output, _ := gitc.GetRemoteURL(remoteAppName)

	if output != "" {
		fmt.Printf("Error: remote '%s' already exists -> %s\n", remoteAppName, strings.TrimSpace(output))
		fmt.Printf("Remove it first with: gokku remote remove %s\n", remoteAppName)
		os.Exit(1)
	}

	// Add git remote
	_, err := gitc.AddRemote(label, remoteURL)

	if err != nil {
		fmt.Printf("Error adding remote '%s' -> %s\n", label, remoteURL)
		os.Exit(1)
	}

	fmt.Printf("Added remote '%s' -> %s\n", label, remoteURL)
}

// handleRemoteList lists all configured git remotes
func (c *RunRemoteCommand) handleRemoteList() {
	gitc := &git.GitClient{}
	outputBytes, err := gitc.ExecuteCommand("remote", "-v")

	if err != nil {
		fmt.Println("Error listing remotes")
		os.Exit(1)
	}

	output := string(outputBytes)

	if strings.TrimSpace(output) == "" {
		fmt.Println("No remotes configured")
		fmt.Println("Add one with: gokku remote add <app_name> <user@server_ip>")
		return
	}

	fmt.Println("Configured remotes:")
	fmt.Println(output)
}

// handleRemoteRemove removes a git remote
func (c *RunRemoteCommand) handleRemoteRemove(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: gokku remote remove <remote_name>")
		fmt.Println("Example: gokku remote remove api")
		os.Exit(1)
	}

	gitc := &git.GitClient{}

	remoteName := args[0]

	// Check if remote exists
	output, _ := gitc.GetRemoteURL(remoteName)

	if output == "" {
		fmt.Printf("Error: remote '%s' not found\n", remoteName)
		os.Exit(1)
	}

	_, err := gitc.RemoveRemote(remoteName)

	if err != nil {
		fmt.Printf("Error removing remote '%s'\n", remoteName)
		os.Exit(1)
	}

	fmt.Printf("Removed remote '%s'\n", remoteName)
}

// handleRemoteSetup performs one-time server setup
func (c *RunRemoteCommand) handleRemoteSetup(args []string) {
	// Extract identity flag (-i or --identity)
	identityFile, remainingArgs := util.ExtractIdentityFlag(args)

	// Get server host from remaining args
	if len(remainingArgs) < 1 {
		fmt.Println("Usage: gokku remote setup <user@host> [-i|--identity <pem_file>]")
		fmt.Println("Example: gokku remote setup ubuntu@192.168.105.3")
		fmt.Println("Example: gokku remote setup ubuntu@ec2.example.com -i ~/.ssh/my-key.pem")
		os.Exit(1)
	}

	serverHost := remainingArgs[0]

	// Validate server host format
	if !strings.Contains(serverHost, "@") {
		fmt.Println("Error: server host must be in format user@host")
		fmt.Println("Example: ubuntu@192.168.105.3")
		os.Exit(1)
	}

	// Validate identity file if provided
	if identityFile != "" {
		if _, err := os.Stat(identityFile); os.IsNotExist(err) {
			fmt.Printf("Error: identity file not found: %s\n", identityFile)
			os.Exit(1)
		}
	}

	// Create setup service and execute
	setup := NewSetupCommand(c.output, serverHost, identityFile)

	if err := setup.Execute(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

func (c *RunRemoteCommand) printRemoteHelp() {
	fmt.Println(`Remote Management Commands:

Usage:
  gokku remote <command> [options]

Commands:
  add <app_name> <user@server_ip>    Add a new git remote
  list, ls                           List all configured remotes
  remove, rm <remote_name>           Remove a git remote
  setup <user@host> [-i|--identity <pem_file>]    Perform one-time server setup
  help                               Show this help

Examples:
  gokku remote add api ubuntu@192.168.105.3
  gokku remote list
  gokku remote remove api
  gokku remote setup ubuntu@192.168.105.3
  gokku remote setup ubuntu@ec2.example.com -i ~/.ssh/my-key.pem

The setup command will:
  - Install Gokku on the server
  - Install essential plugins (nginx, letsencrypt, cron, postgres, redis)
  - Configure SSH keys
  - Verify the installation

After adding a remote, you can:
  git push <remote_name> main`)
}
