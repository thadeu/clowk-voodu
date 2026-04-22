package v1

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"go.gokku-vm.com/pkg"
)

// ContainersCommand gerencia operações de containers
type ContainersCommand struct {
	output  Output
	baseDir string
}

// NewContainersCommand cria uma nova instância de ContainersCommand
func NewContainersCommand(output Output) *ContainersCommand {
	baseDir := os.Getenv("GOKKU_ROOT")
	if baseDir == "" {
		baseDir = "/opt/gokku"
	}

	return &ContainersCommand{
		output:  output,
		baseDir: baseDir,
	}
}

// ContainerFilter representa opções de filtro para listagem de containers
type ContainerFilter struct {
	AppName     string
	ProcessType string
	All         bool
}

// List retorna containers baseado no filtro (movido de internal/services/containers.go)
func (c *ContainersCommand) List(filter ContainerFilter) error {
	containers, err := c.listContainers(filter)
	if err != nil {
		c.output.Error(err.Error())
		return err
	}

	if len(containers) == 0 {
		c.output.Print("No containers found")
		return nil
	}

	// Para stdout, usar tabela
	if _, ok := c.output.(*StdoutOutput); ok {
		headers := []string{"Name", "Status", "Ports"}
		var rows [][]string
		for _, container := range containers {
			rows = append(rows, []string{
				container.Names,
				container.Status,
				container.Ports,
			})
		}
		c.output.Table(headers, rows)
	} else {
		// Para JSON, retornar array de objetos
		c.output.Data(containers)
	}

	return nil
}

// Restart reinicia um container (movido de internal/services/containers.go)
func (c *ContainersCommand) Restart(name string) error {
	cmd := exec.Command("docker", "restart", name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		c.output.Error(fmt.Sprintf("failed to restart container %s: %v, output: %s", name, err, string(output)))
		return err
	}

	c.output.Success(fmt.Sprintf("Container '%s' restarted successfully", name))
	return nil
}

// Stop para um container (movido de internal/services/containers.go)
func (c *ContainersCommand) Stop(name string) error {
	if err := pkg.StopContainer(name); err != nil {
		c.output.Error(err.Error())
		return err
	}

	c.output.Success(fmt.Sprintf("Container '%s' stopped successfully", name))
	return nil
}

// Start inicia um container (movido de internal/services/containers.go)
func (c *ContainersCommand) Start(name string) error {
	if !pkg.ContainerExists(name) {
		c.output.Error(fmt.Sprintf("Container '%s' not found", name))
		return fmt.Errorf("container not found")
	}

	cmd := exec.Command("docker", "start", name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		c.output.Error(fmt.Sprintf("failed to start container %s: %v, output: %s", name, err, string(output)))
		return err
	}

	c.output.Success(fmt.Sprintf("Container '%s' started successfully", name))
	return nil
}

// GetInfo obtém informações de um container (movido de internal/services/containers.go)
func (c *ContainersCommand) GetInfo(name string) error {
	containers, err := pkg.ListContainers(true)
	if err != nil {
		c.output.Error(err.Error())
		return err
	}

	for _, container := range containers {
		if strings.Contains(container.Names, name) {
			c.output.Data(container)
			return nil
		}
	}

	c.output.Error(fmt.Sprintf("Container '%s' not found", name))
	return fmt.Errorf("container not found")
}

// Métodos privados (movidos de internal/services/containers.go)

func (c *ContainersCommand) listContainers(filter ContainerFilter) ([]pkg.ContainerInfo, error) {
	allContainers, err := pkg.ListContainers(filter.All)
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	if filter.AppName == "" && filter.ProcessType == "" {
		return allContainers, nil
	}

	var filtered []pkg.ContainerInfo
	for _, container := range allContainers {
		match := true

		if filter.AppName != "" {
			if !c.containsContainerName(container.Names, filter.AppName) {
				match = false
			}
		}

		if filter.ProcessType != "" && match {
			if !c.containsProcessType(container.Names, filter.ProcessType) {
				match = false
			}
		}

		if match {
			filtered = append(filtered, container)
		}
	}

	return filtered, nil
}

func (c *ContainersCommand) containsContainerName(names, appName string) bool {
	return strings.Contains(names, appName)
}

func (c *ContainersCommand) containsProcessType(names, processType string) bool {
	return strings.Contains(names, processType)
}
