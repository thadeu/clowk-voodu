package v1

import (
	"fmt"
	"os"
	"os/exec"

	"go.gokku-vm.com/pkg"
)

// LogsCommand gerencia logs de aplicações
type LogsCommand struct {
	output  Output
	baseDir string
}

// NewLogsCommand cria uma nova instância de LogsCommand
func NewLogsCommand(output Output) *LogsCommand {
	return &LogsCommand{
		output:  output,
		baseDir: "/opt/gokku",
	}
}

// Show exibe os logs de uma aplicação
func (c *LogsCommand) Show(appName string, follow bool, tail int) error {
	// Verificar se o container existe
	if !pkg.ContainerExists(appName) {
		c.output.Error(fmt.Sprintf("Container '%s' not found", appName))
		return fmt.Errorf("container not found")
	}

	// Construir comando docker logs
	args := []string{"logs"}

	if follow {
		args = append(args, "-f")
	}

	if tail > 0 {
		args = append(args, "--tail", fmt.Sprintf("%d", tail))
	} else {
		args = append(args, "--tail", "500")
	}

	args = append(args, appName)

	cmd := exec.Command("docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		// Check if it's a signal interruption (Ctrl+C), which is normal
		if exitErr, ok := err.(*exec.ExitError); ok {
			// Exit code 130 = SIGINT (Ctrl+C)
			// Exit code 143 = SIGTERM
			if exitErr.ExitCode() == 130 || exitErr.ExitCode() == 143 {
				return nil
			}
		}

		c.output.Error(fmt.Sprintf("Error executing docker logs: %v", err))
		return err
	}

	return nil
}
