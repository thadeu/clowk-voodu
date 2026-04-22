package v1

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"go.gokku-vm.com/pkg/util"
)

// AutoUpdateCommand gerencia auto-atualização do gokku
type AutoUpdateCommand struct {
	output Output
}

// NewAutoUpdateCommand cria uma nova instância de AutoUpdateCommand
func NewAutoUpdateCommand(output Output) *AutoUpdateCommand {
	return &AutoUpdateCommand{
		output: output,
	}
}

// Execute executa a atualização automática
func (c *AutoUpdateCommand) Execute(args []string) error {
	command := "curl -fsSL https://gokku-vm.com/install | bash -s --"

	remoteInfo, remainingArgs, err := util.GetRemoteInfoOrDefault(args)
	if err != nil {
		c.output.Error(fmt.Sprintf("Error: %v", err))
		return err
	}

	hasRemoteFlag := strings.Contains(strings.Join(remainingArgs, " "), "--remote")

	if util.IsClientMode() && hasRemoteFlag {
		command += " --server"

		cmd := fmt.Sprintf("bash -c '%s'", command)
		c.output.Print(fmt.Sprintf("Executing command auto-update on server: %s", cmd))

		if err := util.ExecuteRemoteCommand(remoteInfo, cmd); err != nil {
			c.output.Error(fmt.Sprintf("Error: %v", err))
			return err
		}

		return nil
	}

	if util.IsServerMode() {
		command += " --server"
	} else {
		command += " --client"
	}

	c.output.Print(fmt.Sprintf("Executing command auto-update: %s", command))

	cmd := exec.Command("bash", "-c", command)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		c.output.Error(fmt.Sprintf("Error: %v", err))
		return err
	}

	return nil
}
