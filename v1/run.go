package v1

import (
	"fmt"
	"os"
	"strings"

	"go.gokku-vm.com/pkg/context"
	"go.gokku-vm.com/pkg/util"
)

// RunCommand gerencia operações de aplicações
type RunCommand struct {
	output  Output
	baseDir string
}

// NewRunCommand cria uma nova instância de RunCommand
func NewRunCommand(output Output) *RunCommand {
	baseDir := os.Getenv("GOKKU_ROOT")

	if baseDir == "" {
		baseDir = "/opt/gokku"
	}

	return &RunCommand{
		output:  output,
		baseDir: baseDir,
	}
}

func (c *RunCommand) UseWithContext(ctx *context.ExecutionContext, args []string) {
	if err := ctx.ValidateAppRequired(); err != nil {
		ctx.PrintUsageError("run", err.Error())
	}

	_, remainingArgs := util.ExtractAppFlag(args)

	if len(remainingArgs) < 1 {
		fmt.Println("Error: command is required")
		fmt.Println("Usage: gokku run <command> -a <app>")
		os.Exit(1)
	}

	command := strings.Join(remainingArgs, " ")

	containerName := ctx.GetAppName()
	dockerCommand := fmt.Sprintf("docker exec -it %s %s", containerName, command)

	ctx.PrintConnectionInfo()
	fmt.Printf("$ %s\n\n", command)

	// Execute command
	if err := ctx.ExecuteCommand(dockerCommand); err != nil {
		os.Exit(1)
	}
}
