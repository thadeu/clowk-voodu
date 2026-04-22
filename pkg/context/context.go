package context

import (
	"fmt"
	"os"
	"os/exec"

	"go.gokku-vm.com/pkg"

	"go.gokku-vm.com/pkg/util"
)

// ExecutionContext represents the execution context for a command
type ExecutionContext struct {
	Mode            string // "client" or "server"
	AppName         string
	RemoteInfo      *pkg.RemoteInfo
	ServerExecution bool
	Host            string
	BaseDir         string
}

// NewExecutionContext creates a new execution context based on mode and app flag
func NewExecutionContext(appName string) (*ExecutionContext, error) {
	ctx := &ExecutionContext{
		AppName: appName,
	}

	if util.IsClientMode() {
		ctx.Mode = "client"
		ctx.ServerExecution = false
	} else {
		ctx.Mode = "server"
		ctx.ServerExecution = true
	}

	// Determine mode
	if !ctx.ServerExecution {
		if appName != "" {
			// Client mode with app - use git remote
			remoteInfo, err := util.GetRemoteInfo(appName)

			if err != nil {
				return nil, fmt.Errorf("failed to get remote info: %v", err)
			}

			ctx.RemoteInfo = remoteInfo
			ctx.Host = remoteInfo.Host
			ctx.BaseDir = remoteInfo.BaseDir
		}
	} else {
		ctx.Host = ""
		ctx.BaseDir = "/opt/gokku"

		if appName != "" {
			// Server mode with app - use app name directly
			ctx.AppName = appName
		}
	}

	return ctx, nil
}

// ValidateAppRequired validates that an app is required for the current context
func (ctx *ExecutionContext) ValidateAppRequired() error {
	if ctx.AppName == "" {
		if ctx.Mode == "client" {
			return fmt.Errorf("client mode requires -a flag to specify app")
		} else {
			return fmt.Errorf("server mode requires -a flag to specify app")
		}
	}

	return nil
}

// GetAppName returns the actual app name to use for execution
func (ctx *ExecutionContext) GetAppName() string {
	if ctx.Mode == "client" && ctx.RemoteInfo != nil {
		return ctx.RemoteInfo.App
	}

	return ctx.AppName
}

// ExecuteCommand executes a command based on the context
func (ctx *ExecutionContext) ExecuteCommand(command string) error {
	if ctx.ServerExecution {
		// Local execution on server
		cmd := exec.Command("bash", "-c", command)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		return cmd.Run()
	} else {
		// Remote execution via SSH
		cmd := exec.Command("ssh", ctx.Host, command)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		return cmd.Run()
	}
}

// ExecuteCommandWithOutput executes a command and returns output
func (ctx *ExecutionContext) ExecuteCommandWithOutput(command string) (string, error) {
	if ctx.ServerExecution {
		// Local execution on server
		cmd := exec.Command("bash", "-c", command)
		output, err := cmd.Output()
		return string(output), err
	} else {
		// Remote execution via SSH
		cmd := exec.Command("ssh", ctx.Host, command)
		output, err := cmd.Output()
		return string(output), err
	}
}

// ExecuteCommandWithSignalHandling executes a command with proper signal handling
func (ctx *ExecutionContext) ExecuteCommandWithSignalHandling(command string) error {
	cmd := exec.Command("bash", "-c", command)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	// Set up signal handling for graceful shutdown
	return cmd.Run()
}

// PrintConnectionInfo prints connection information for remote execution
func (ctx *ExecutionContext) PrintConnectionInfo() {
	if !ctx.ServerExecution && ctx.RemoteInfo != nil {
		fmt.Printf("→ %s (%s)\n", ctx.RemoteInfo.App, ctx.RemoteInfo.Host)
	}
}

// GetUsageExamples returns usage examples based on the context
func (ctx *ExecutionContext) GetUsageExamples(command string) []string {
	if ctx.Mode == "client" {
		return []string{
			fmt.Sprintf("gokku %s -a api-production", command),
			fmt.Sprintf("gokku %s -a worker-staging", command),
		}
	} else {
		return []string{
			fmt.Sprintf("gokku %s -a api", command),
			fmt.Sprintf("gokku %s -a worker", command),
		}
	}
}

// PrintUsageError prints a usage error with context-specific examples
func (ctx *ExecutionContext) PrintUsageError(command string, message string) {
	fmt.Printf("Error: %s\n", message)
	fmt.Println("")
	fmt.Printf("Usage: gokku %s -a <app>\n", command)
	fmt.Println("")
	fmt.Println("Examples:")
	for _, example := range ctx.GetUsageExamples(command) {
		fmt.Printf("  %s\n", example)
	}
	os.Exit(1)
}
