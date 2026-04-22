package main

import (
	"fmt"
	"os"
	"strings"

	"go.gokku-vm.com/pkg/context"
	"go.gokku-vm.com/pkg/util"

	v1 "go.gokku-vm.com/v1"
)

const version = "1.1.0"

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(0)
	}

	command := os.Args[1]
	args := os.Args[2:]

	// Check for --remote flag early, regardless of command position
	remoteInfo, remainingArgs, err := util.GetRemoteInfoOrDefault(args)

	if err == nil && remoteInfo != nil {
		remoteCmd := []string{"gokku", command}
		remoteCmd = append(remoteCmd, remainingArgs...)
		cmdStr := strings.Join(remoteCmd, " ")

		if err := util.ExecuteRemoteCommand(remoteInfo, cmdStr); err != nil {
			os.Exit(1)
		}

		return
	}

	args = remainingArgs

	cmd := v1.NewCommand(v1.OutputFormatStdout)

	// Create execution context only for 'run' command
	var ctx *context.ExecutionContext

	if command == "run" {
		appName, _ := util.ExtractAppFlag(args)

		var err error
		ctx, err = context.NewExecutionContext(appName)

		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
	}

	// Handle version/help first
	switch command {
	case "version", "--version", "-v":
		fmt.Printf("gokku version %s\n", version)
		return
	case "help", "--help", "-h":
		printHelp()
		return
	}

	// Handle prefixed commands (e.g., ps:list, config:set)
	if strings.Contains(command, ":") {
		parts := strings.SplitN(command, ":", 2)
		prefix := parts[0]

		subcommand := parts[1]

		switch prefix {
		case "apps":
			appsCommand(cmd, subcommand, args)
			return
		case "config":
			configCommand(cmd, subcommand, args)
			return
		case "ps":
			psCommand(cmd, subcommand, args)
			return
		case "services", "service":
			servicesCommand(cmd, subcommand, args)
			return
		case "plugins", "plugin":
			pluginsCommand(cmd, subcommand, args)
			return
		default:
			// Plugin command (e.g., nginx:reload)
			pluginsCommand(cmd, subcommand, args)
			return
		}
	}

	switch command {
	case "apps":
		appsCommand(cmd, "", args)
	case "config":
		configCommand(cmd, "", args)
	case "ps":
		psCommand(cmd, "", args)
	case "services":
		servicesCommand(cmd, "", args)
	case "plugins":
		pluginsCommand(cmd, "", args)
	case "logs":
		logsCommand(cmd, args)
	case "restart":
		restartCommand(cmd, args)
	case "deploy":
		deployCommand(cmd, args)
	case "remote":
		remoteCommand(cmd, args)
	case "run":
		runCommand(cmd, ctx, args)
	case "rollback":
		rollbackCommand(cmd, args)
	case "au", "update", "auto-update":
		autoUpdateCommand(cmd, args)
	case "uninstall":
		uninstallCommand(cmd, args)
	default:
		pluginsCommand(cmd, command, args)
	}
}

func remoteCommand(cmd *v1.Command, args []string) {
	cmd.Remote.Use(args)
}

func runCommand(cmd *v1.Command, ctx *context.ExecutionContext, args []string) {
	cmd.Run.UseWithContext(ctx, args)
}

func rollbackCommand(cmd *v1.Command, args []string) {
	appName, remainingArgs := util.ExtractAppFlag(args)

	if appName == "" {
		fmt.Println("Error: -a <app> is required")
		fmt.Println("Usage: gokku rollback -a <app> [release-id]")
		os.Exit(1)
	}

	// Check for remote execution
	if err := executeRemoteForApp("rollback", "", appName, remainingArgs); err == nil {
		return
	}

	// Local execution
	releaseID := ""
	if len(remainingArgs) > 0 {
		releaseID = remainingArgs[0]
	}

	if err := cmd.Rollback.Execute(appName, releaseID); err != nil {
		os.Exit(1)
	}
}

func autoUpdateCommand(cmd *v1.Command, args []string) {
	if err := cmd.AutoUpdate.Execute(args); err != nil {
		os.Exit(1)
	}
}

func uninstallCommand(cmd *v1.Command, args []string) {
	cmd.Uninstall.UseUninstall(args)
}

func appsCommand(cmd *v1.Command, subcommand string, args []string) {
	if err := executeRemote("apps", subcommand, args); err == nil {
		return
	}

	// Local execution
	if subcommand == "" && len(args) > 0 {
		subcommand = args[0]
		args = args[1:]
	}

	switch subcommand {
	case "", "list", "ls":
		if err := cmd.Apps.List(); err != nil {
			os.Exit(1)
		}
	case "create":
		if len(args) < 1 {
			fmt.Println("Error: app name is required")
			fmt.Println("Usage: gokku apps create <app>")
			os.Exit(1)
		}
		if err := cmd.Apps.Create(args[0], "deploy"); err != nil {
			os.Exit(1)
		}
	case "destroy", "rm":
		if len(args) < 1 {
			fmt.Println("Error: app name is required")
			fmt.Println("Usage: gokku apps destroy <app>")
			os.Exit(1)
		}
		if err := cmd.Apps.Destroy(args[0]); err != nil {
			os.Exit(1)
		}
	case "info":
		if len(args) < 1 {
			fmt.Println("Error: app name is required")
			fmt.Println("Usage: gokku apps info <app>")
			os.Exit(1)
		}
		if err := cmd.Apps.Get(args[0]); err != nil {
			os.Exit(1)
		}
	default:
		fmt.Printf("Unknown apps subcommand: %s\n", subcommand)
		os.Exit(1)
	}
}

func configCommand(cmd *v1.Command, subcommand string, args []string) {
	appName, remainingArgs := util.ExtractAppFlag(args)

	if appName == "" {
		fmt.Println("Error: -a <app> is required")
		fmt.Println("Usage: gokku config <command> -a <app>")
		os.Exit(1)
	}

	// Check for remote execution
	if err := executeRemoteForApp("config", subcommand, appName, remainingArgs); err == nil {
		return
	}

	// Local execution
	if subcommand == "" && len(remainingArgs) > 0 {
		subcommand = remainingArgs[0]
		remainingArgs = remainingArgs[1:]
	}

	switch subcommand {
	case "", "list":
		if err := cmd.Config.List(appName); err != nil {
			os.Exit(1)
		}
	case "set":
		if len(remainingArgs) < 1 {
			fmt.Println("Error: KEY=VALUE is required")
			fmt.Println("Usage: gokku config set KEY=VALUE [KEY2=VALUE2...] -a <app>")
			os.Exit(1)
		}
		if err := cmd.Config.Set(appName, remainingArgs); err != nil {
			os.Exit(1)
		}
	case "get":
		if len(remainingArgs) < 1 {
			fmt.Println("Error: KEY is required")
			fmt.Println("Usage: gokku config get KEY -a <app>")
			os.Exit(1)
		}
		if err := cmd.Config.Get(appName, remainingArgs[0]); err != nil {
			os.Exit(1)
		}
	case "unset":
		if len(remainingArgs) < 1 {
			fmt.Println("Error: KEY is required")
			fmt.Println("Usage: gokku config unset KEY [KEY2...] -a <app>")
			os.Exit(1)
		}
		if err := cmd.Config.Unset(appName, remainingArgs); err != nil {
			os.Exit(1)
		}
	default:
		fmt.Printf("Unknown config subcommand: %s\n", subcommand)
		os.Exit(1)
	}
}

func psCommand(cmd *v1.Command, subcommand string, args []string) {
	appName, remainingArgs := util.ExtractAppFlag(args)

	// Check for remote execution
	if appName != "" {
		if err := executeRemoteForApp("ps", subcommand, appName, remainingArgs); err == nil {
			return
		}
	}

	// Local execution
	if subcommand == "" && len(remainingArgs) > 0 {
		subcommand = remainingArgs[0]
		remainingArgs = remainingArgs[1:]
	}

	switch subcommand {
	case "", "list", "ls":
		if err := cmd.Processes.List(appName); err != nil {
			os.Exit(1)
		}
	case "restart":
		if appName == "" {
			fmt.Println("Error: -a <app> is required")
			fmt.Println("Usage: gokku ps restart -a <app>")
			os.Exit(1)
		}
		if err := cmd.Processes.Restart(appName); err != nil {
			os.Exit(1)
		}
	case "stop":
		if appName == "" {
			fmt.Println("Error: -a <app> is required")
			fmt.Println("Usage: gokku ps stop -a <app> [process-type]")
			os.Exit(1)
		}
		processType := ""
		if len(remainingArgs) > 0 {
			processType = remainingArgs[0]
		}
		if err := cmd.Processes.Stop(appName, processType); err != nil {
			os.Exit(1)
		}
	case "start":
		if appName == "" {
			fmt.Println("Error: -a <app> is required")
			fmt.Println("Usage: gokku ps start -a <app>")
			os.Exit(1)
		}
		if err := cmd.Processes.Start(appName); err != nil {
			os.Exit(1)
		}
	default:
		fmt.Printf("Unknown ps subcommand: %s\n", subcommand)
		os.Exit(1)
	}
}

func servicesCommand(cmd *v1.Command, subcommand string, args []string) {
	if err := executeRemote("services", subcommand, args); err == nil {
		return
	}

	// Local execution
	if subcommand == "" && len(args) > 0 {
		subcommand = args[0]
		args = args[1:]
	}

	switch subcommand {
	case "", "list", "ls":
		if err := cmd.Services.List(); err != nil {
			os.Exit(1)
		}
	case "create":
		if len(args) < 2 {
			fmt.Println("Error: plugin and service name are required")
			fmt.Println("Usage: gokku services create <plugin> <service> [version]")
			os.Exit(1)
		}
		version := ""
		if len(args) > 2 {
			version = args[2]
		}
		if err := cmd.Services.Create(args[0], args[1], version); err != nil {
			os.Exit(1)
		}
	case "destroy", "rm":
		if len(args) < 1 {
			fmt.Println("Error: service name is required")
			fmt.Println("Usage: gokku services destroy <service>")
			os.Exit(1)
		}
		if err := cmd.Services.Destroy(args[0]); err != nil {
			os.Exit(1)
		}
	case "link":
		if len(args) < 2 {
			fmt.Println("Error: service and app names are required")
			fmt.Println("Usage: gokku services link <service> <app>")
			os.Exit(1)
		}
		if err := cmd.Services.Link(args[0], args[1]); err != nil {
			os.Exit(1)
		}
	case "unlink":
		if len(args) < 2 {
			fmt.Println("Error: service and app names are required")
			fmt.Println("Usage: gokku services unlink <service> <app>")
			os.Exit(1)
		}
		if err := cmd.Services.Unlink(args[0], args[1]); err != nil {
			os.Exit(1)
		}
	case "info":
		if len(args) < 1 {
			fmt.Println("Error: service name is required")
			fmt.Println("Usage: gokku services info <service>")
			os.Exit(1)
		}
		if err := cmd.Services.Info(args[0]); err != nil {
			os.Exit(1)
		}
	case "logs":
		if len(args) < 1 {
			fmt.Println("Error: service name is required")
			fmt.Println("Usage: gokku services logs <service> [-f]")
			os.Exit(1)
		}
		follow := len(args) > 1 && (args[1] == "-f" || args[1] == "--follow")
		if err := cmd.Services.Logs(args[0], follow); err != nil {
			os.Exit(1)
		}
	default:
		fmt.Printf("Unknown services subcommand: %s\n", subcommand)
		os.Exit(1)
	}
}

func pluginsCommand(cmd *v1.Command, subcommand string, args []string) {
	if err := executeRemote("plugins", subcommand, args); err == nil {
		return
	}

	// Local execution
	if subcommand == "" && len(args) > 0 {
		subcommand = args[0]
		args = args[1:]
	}

	switch subcommand {
	case "", "list", "ls":
		if err := cmd.Plugins.List(); err != nil {
			os.Exit(1)
		}
	case "install", "add":
		if len(args) < 1 {
			fmt.Println("Error: plugin name is required")
			fmt.Println("Usage: gokku plugins install <plugin> [url]")
			os.Exit(1)
		}
		pluginURL := ""
		if len(args) > 1 {
			pluginURL = args[1]
		}
		if err := cmd.Plugins.Install(args[0], pluginURL); err != nil {
			os.Exit(1)
		}
	case "uninstall", "remove", "rm":
		if len(args) < 1 {
			fmt.Println("Error: plugin name is required")
			fmt.Println("Usage: gokku plugins uninstall <plugin>")
			os.Exit(1)
		}
		if err := cmd.Plugins.Uninstall(args[0]); err != nil {
			os.Exit(1)
		}
	case "update":
		if len(args) < 1 {
			fmt.Println("Error: plugin name is required")
			fmt.Println("Usage: gokku plugins update <plugin>")
			os.Exit(1)
		}
		if err := cmd.Plugins.Update(args[0]); err != nil {
			os.Exit(1)
		}
	default:
		// Fallback to plugin-specific commands (e.g., nginx:reload)
		if err := cmd.Plugins.Wildcard(append([]string{subcommand}, args...)); err != nil {
			os.Exit(1)
		}
	}
}

func logsCommand(cmd *v1.Command, args []string) {
	appName, remainingArgs := util.ExtractAppFlag(args)

	if appName == "" {
		fmt.Println("Error: -a <app> is required")
		fmt.Println("Usage: gokku logs -a <app> [-f]")
		os.Exit(1)
	}

	// Check for remote execution
	if err := executeRemoteForApp("logs", "", appName, remainingArgs); err == nil {
		return
	}

	// Local execution
	follow := false
	tail := 500

	for _, arg := range remainingArgs {
		if arg == "-f" || arg == "--follow" {
			follow = true
		}
	}

	if err := cmd.Logs.Show(appName, follow, tail); err != nil {
		os.Exit(1)
	}
}

func restartCommand(cmd *v1.Command, args []string) {
	appName, remainingArgs := util.ExtractAppFlag(args)

	if appName == "" {
		fmt.Println("Error: -a <app> is required")
		fmt.Println("Usage: gokku restart -a <app>")
		os.Exit(1)
	}

	// Check for remote execution
	if err := executeRemoteForApp("restart", "", appName, remainingArgs); err == nil {
		return
	}

	// Local execution
	if err := cmd.Restart.Execute(appName); err != nil {
		os.Exit(1)
	}
}

func deployCommand(cmd *v1.Command, args []string) {
	appName, _ := util.ExtractAppFlag(args)

	if appName == "" {
		fmt.Println("Error: -a <app> is required")
		fmt.Println("Usage: gokku deploy -a <app>")
		os.Exit(1)
	}

	// Check for remote execution
	remoteInfo, err := util.GetRemoteInfo(appName)
	if err == nil && remoteInfo != nil {
		// Deploy via git push
		fmt.Printf("Deploying to %s...\n", appName)
		fmt.Println("Use: git push <remote> <branch>")
		return
	}

	// Local execution (server-side)
	if err := cmd.Deploy.Execute(appName); err != nil {
		os.Exit(1)
	}
}

func executeRemote(command, subcommand string, args []string) error {
	remoteInfo, remainingArgs, err := util.GetRemoteInfoOrDefault(args)

	if err != nil || remoteInfo == nil {
		return err
	}

	cmdParts := []string{"gokku", command}

	if subcommand != "" {
		cmdParts = append(cmdParts, subcommand)
	}

	cmdParts = append(cmdParts, remainingArgs...)
	cmdStr := strings.Join(cmdParts, " ")

	return util.ExecuteRemoteCommand(remoteInfo, cmdStr)
}

func executeRemoteForApp(command, subcommand, appName string, remainingArgs []string) error {
	remoteInfo, err := util.GetRemoteInfo(appName)
	if err != nil || remoteInfo == nil {
		return err
	}

	cmdParts := []string{"gokku", command}
	if subcommand != "" {
		cmdParts = append(cmdParts, subcommand)
	}
	cmdParts = append(cmdParts, remainingArgs...)
	cmdParts = append(cmdParts, "--app", remoteInfo.App)
	cmdStr := strings.Join(cmdParts, " ")

	return util.ExecuteRemoteCommand(remoteInfo, cmdStr)
}

func printHelp() {
	fmt.Println(`gokku - Deployment management CLI

Usage:
  gokku <command> [options]

CLIENT COMMANDS (run from local machine):
  remote        Manage git remotes (add, list, remove, setup)
  apps          List applications on remote server
  config        Manage environment variables (use -a with git remote)
  run           Run arbitrary commands (use -a)
  logs          View application logs (use -a)
  restart       Restart services (use -a)
  deploy        Deploy applications
  rollback      Rollback to previous release
  tool          Utility commands for scripts
  plugins       Manage plugins
  services      Manage services
  ps            Process management (list, restart, stop)
  uninstall     Remove Gokku installation
  version       Show version
  help          Show this help

SERVER COMMANDS (run directly on server):
  config        Manage environment variables locally (use -a with app name)
  run           Run arbitrary commands locally
  logs          View application logs locally
  restart       Restart services locally
  rollback      Rollback to previous release locally

Remote Management:
  gokku remote add <app_name> <user@host>                      Add a git remote
  gokku remote list                                             List git remotes
  gokku remote remove <name>                                    Remove a git remote
  gokku remote setup <user@host> [-i|--identity <pem_file>]    One-time server setup

Client Commands (always use -a with git remote):
  gokku config set KEY=VALUE -a <git-remote>
  gokku config get KEY -a <git-remote>
  gokku config list -a <git-remote>
  gokku config unset KEY -a <git-remote>

  gokku run <command> -a <git-remote>

  gokku logs -a <git-remote> [-f]
  gokku restart -a <git-remote>

  gokku deploy -a <git-remote>
  gokku rollback -a <git-remote>

  gokku ps:list -a <git-remote>
  gokku ps:restart -a <git-remote>
  gokku ps:stop -a <git-remote>

Server Commands (run on server only, use -a with app name):
  gokku run <command>                                (run locally)
  gokku logs <app> <env> [-f]                        (view logs locally)
  gokku restart <app>                                (restart locally)
  gokku rollback <app> <env> [release-id]            (rollback locally)

  Server Mode (-a with app name):
  -a, --app <app-name>`)
}
