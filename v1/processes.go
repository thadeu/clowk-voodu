package v1

import (
	"fmt"
	"os"

	"go.gokku-vm.com/pkg"

	"go.gokku-vm.com/pkg/containers"
)

// ProcessesCommand gerencia processos/containers
type ProcessesCommand struct {
	output        Output
	baseDir       string
	containersCmd *ContainersCommand
}

// NewProcessesCommand cria uma nova instância de ProcessesCommand
func NewProcessesCommand(output Output) *ProcessesCommand {
	baseDir := os.Getenv("GOKKU_ROOT")
	if baseDir == "" {
		baseDir = "/opt/gokku"
	}

	return &ProcessesCommand{
		output:        output,
		baseDir:       baseDir,
		containersCmd: NewContainersCommand(output),
	}
}

// ContainerInfo representa informações de um container
type ContainerInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Ports  string `json:"ports"`
}

// List lista todos os containers ou containers de uma app específica
func (c *ProcessesCommand) List(appName string) error {
	var filter ContainerFilter

	if appName != "" {
		filter = ContainerFilter{
			AppName: appName,
			All:     false,
		}
	} else {
		filter = ContainerFilter{
			All: false,
		}
	}

	return c.containersCmd.List(filter)
}

// Restart reinicia todos os containers de uma app
func (c *ProcessesCommand) Restart(appName string) error {
	registry := containers.NewContainerRegistry()
	allContainers, err := registry.ListContainers(appName)

	if err != nil {
		c.output.Error(fmt.Sprintf("Error getting containers: %v", err))
		return err
	}

	if len(allContainers) == 0 {
		// Fallback: try to find containers directly by name
		filter := ContainerFilter{
			AppName: appName,
			All:     true,
		}

		containers, err := c.containersCmd.listContainers(filter)
		if err == nil && len(containers) > 0 {
			c.output.Print(fmt.Sprintf("Restarting processes for app '%s' (found %d container(s))...", appName, len(containers)))

			for _, container := range containers {
				c.output.Print(fmt.Sprintf("-----> Restarting %s", container.Names))
				c.containersCmd.Restart(container.Names)
			}

			c.output.Success(fmt.Sprintf("Restart complete for app '%s'", appName))
			return nil
		}

		c.output.Print(fmt.Sprintf("No processes running for app '%s'", appName))
		return nil
	}

	c.output.Print(fmt.Sprintf("Restarting processes for app '%s'...", appName))

	for _, container := range allContainers {
		c.output.Print(fmt.Sprintf("-----> Restarting %s", container.Name))

		if err := c.containersCmd.Restart(container.Name); err != nil {
			c.output.Print(fmt.Sprintf("       Error restarting container %s: %v", container.Name, err))
			continue
		}

		registry.UpdateContainerStatus(appName, container.ProcessType, container.Number, "running")
	}

	c.output.Success(fmt.Sprintf("Restart complete for app '%s'", appName))
	return nil
}

// Stop para containers de uma app (todos ou de um tipo específico)
func (c *ProcessesCommand) Stop(appName, processType string) error {
	registry := containers.NewContainerRegistry()

	if processType != "" {
		// Stop specific process type
		allContainers, err := registry.ListContainers(appName)
		if err != nil {
			c.output.Error(fmt.Sprintf("Error getting containers: %v", err))
			return err
		}

		// Filter by process type
		containers := make([]pkg.ContainerInfo, 0)
		for _, container := range allContainers {
			if container.ProcessType == processType {
				containers = append(containers, container)
			}
		}
		if err != nil {
			c.output.Error(fmt.Sprintf("Error getting containers: %v", err))
			return err
		}

		if len(containers) == 0 {
			c.output.Print(fmt.Sprintf("No %s processes running for app '%s'", processType, appName))
			return nil
		}

		c.output.Print(fmt.Sprintf("Stopping %s processes for app '%s'...", processType, appName))

		for _, container := range containers {
			c.output.Print(fmt.Sprintf("-----> Stopping %s", container.Name))

			if err := c.containersCmd.Stop(container.Name); err != nil {
				c.output.Print(fmt.Sprintf("       Error stopping container %s: %v", container.Name, err))
				continue
			}

			registry.UpdateContainerStatus(appName, container.ProcessType, container.Number, "stopped")
		}
	} else {
		// Stop all processes
		allContainers, err := registry.ListContainers(appName)
		if err != nil {
			c.output.Error(fmt.Sprintf("Error getting containers: %v", err))
			return err
		}

		if len(allContainers) == 0 {
			// Fallback: try to find containers directly by name
			filter := ContainerFilter{
				AppName: appName,
				All:     true,
			}

			containers, err := c.containersCmd.listContainers(filter)
			if err == nil && len(containers) > 0 {
				c.output.Print(fmt.Sprintf("Stopping all processes for app '%s' (found %d container(s))...", appName, len(containers)))

				for _, container := range containers {
					c.output.Print(fmt.Sprintf("-----> Stopping %s", container.Names))
					c.containersCmd.Stop(container.Names)
				}

				c.output.Success(fmt.Sprintf("Stop complete for app '%s'", appName))
				return nil
			}

			c.output.Print(fmt.Sprintf("No processes running for app '%s'", appName))
			return nil
		}

		c.output.Print(fmt.Sprintf("Stopping all processes for app '%s'...", appName))

		for _, container := range allContainers {
			c.output.Print(fmt.Sprintf("-----> Stopping %s", container.Name))

			if err := c.containersCmd.Stop(container.Name); err != nil {
				c.output.Print(fmt.Sprintf("       Error stopping container %s: %v", container.Name, err))
				continue
			}

			registry.UpdateContainerStatus(appName, container.ProcessType, container.Number, "stopped")
		}
	}

	c.output.Success(fmt.Sprintf("Stop complete for app '%s'", appName))
	return nil
}

// Start inicia containers de uma app
func (c *ProcessesCommand) Start(appName string) error {
	registry := containers.NewContainerRegistry()
	allContainers, err := registry.ListContainers(appName)

	if err != nil {
		c.output.Error(fmt.Sprintf("Error getting containers: %v", err))
		return err
	}

	if len(allContainers) == 0 {
		c.output.Print(fmt.Sprintf("No processes found for app '%s'", appName))
		return nil
	}

	c.output.Print(fmt.Sprintf("Starting processes for app '%s'...", appName))

	for _, container := range allContainers {
		c.output.Print(fmt.Sprintf("-----> Starting %s", container.Name))

		if err := c.containersCmd.Start(container.Name); err != nil {
			c.output.Print(fmt.Sprintf("       Error starting container %s: %v", container.Name, err))
			continue
		}

		registry.UpdateContainerStatus(appName, container.ProcessType, container.Number, "running")
	}

	c.output.Success(fmt.Sprintf("Start complete for app '%s'", appName))
	return nil
}

// Scale escala containers de uma app
func (c *ProcessesCommand) Scale(appName string, processScales map[string]int) error {
	// TODO: Implementar lógica de scaling
	c.output.Success(fmt.Sprintf("Scaled app '%s'", appName))
	return nil
}
