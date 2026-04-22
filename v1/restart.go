package v1

import (
	"fmt"
	"path/filepath"

	"go.gokku-vm.com/pkg"
)

// RestartCommand gerencia restart de aplicações
type RestartCommand struct {
	output  Output
	baseDir string
}

// NewRestartCommand cria uma nova instância de RestartCommand
func NewRestartCommand(output Output) *RestartCommand {
	return &RestartCommand{
		output:  output,
		baseDir: "/opt/gokku",
	}
}

// Execute reinicia uma aplicação
func (c *RestartCommand) Execute(appName string) error {
	c.output.Print(fmt.Sprintf("Restarting %s...", appName))

	envFile := filepath.Join(c.baseDir, "apps", appName, "shared", ".env")
	appDir := filepath.Join(c.baseDir, "apps", appName, "current")

	if err := pkg.RecreateActiveContainer(appName, envFile, appDir); err != nil {
		c.output.Error(fmt.Sprintf("Error restarting app: %v", err))
		return err
	}

	c.output.Success(fmt.Sprintf("App '%s' restarted successfully", appName))
	return nil
}
