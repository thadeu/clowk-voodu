package v1

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.gokku-vm.com/pkg"
	"go.gokku-vm.com/pkg/util"
)

// ServicesCommand gerencia operações de serviços
type ServicesCommand struct {
	output      Output
	baseDir     string
	servicesDir string
	pluginsDir  string
}

// NewServicesCommand cria uma nova instância de ServicesCommand
func NewServicesCommand(output Output) *ServicesCommand {
	baseDir := os.Getenv("GOKKU_ROOT")
	if baseDir == "" {
		baseDir = "/opt/gokku"
	}

	return &ServicesCommand{
		output:      output,
		baseDir:     baseDir,
		servicesDir: filepath.Join(baseDir, "services"),
		pluginsDir:  filepath.Join(baseDir, "plugins"),
	}
}

// ServiceInfo representa informações de um serviço
type ServiceInfo struct {
	Name        string            `json:"name"`
	Plugin      string            `json:"plugin"`
	Status      string            `json:"status"`
	Running     bool              `json:"running"`
	LinkedApps  []string          `json:"linked_apps"`
	ContainerID string            `json:"container_id,omitempty"`
	CreatedAt   string            `json:"created_at,omitempty"`
	Config      map[string]string `json:"config,omitempty"`
}

// Service representa um serviço interno (para persistência)
type service struct {
	Name        string            `json:"name"`
	Plugin      string            `json:"plugin"`
	ContainerID string            `json:"container_id"`
	Running     bool              `json:"running"`
	LinkedApps  []string          `json:"linked_apps"`
	CreatedAt   string            `json:"created_at"`
	Config      map[string]string `json:"config"`
}

// List lista todos os serviços
func (c *ServicesCommand) List() error {
	servicesData, err := c.listServices()
	if err != nil {
		c.output.Error(err.Error())
		return err
	}

	if len(servicesData) == 0 {
		c.output.Print("No services found")
		return nil
	}

	// Converter para o formato esperado
	var result []ServiceInfo
	for _, svc := range servicesData {
		status := "stopped"
		if pkg.ContainerIsRunning(svc.Name) {
			status = "running"
		}

		result = append(result, ServiceInfo{
			Name:        svc.Name,
			Plugin:      svc.Plugin,
			Status:      status,
			Running:     svc.Running,
			LinkedApps:  svc.LinkedApps,
			ContainerID: svc.ContainerID,
			CreatedAt:   svc.CreatedAt,
			Config:      svc.Config,
		})
	}

	// Para stdout, usar tabela
	if _, ok := c.output.(*StdoutOutput); ok {
		headers := []string{"Name", "Plugin", "Status"}
		var rows [][]string
		for _, svc := range result {
			rows = append(rows, []string{
				svc.Name,
				svc.Plugin,
				svc.Status,
			})
		}
		c.output.Table(headers, rows)
	} else {
		// Para JSON, retornar array de objetos
		c.output.Data(result)
	}

	return nil
}

// Create cria um novo serviço
func (c *ServicesCommand) Create(pluginName, serviceName, version string) error {
	if !c.pluginExists(pluginName) {
		c.output.Error(fmt.Sprintf("Plugin '%s' not found", pluginName))
		return fmt.Errorf("plugin not found")
	}

	if c.serviceExists(serviceName) {
		c.output.Error(fmt.Sprintf("Service '%s' already exists", serviceName))
		return fmt.Errorf("service already exists")
	}

	// Create service directory
	serviceDir := filepath.Join(c.servicesDir, serviceName)
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		c.output.Error(fmt.Sprintf("Failed to create service directory: %v", err))
		return err
	}

	// Save service configuration
	svc := service{
		Name:       serviceName,
		Plugin:     pluginName,
		Running:    false,
		LinkedApps: []string{},
		CreatedAt:  time.Now().Format(time.RFC3339),
		Config:     make(map[string]string),
	}

	// Store version if provided
	if version != "" {
		svc.Config["version"] = version
	}

	if err := c.saveServiceConfig(serviceName, svc); err != nil {
		// Cleanup on error
		os.RemoveAll(serviceDir)
		c.output.Error(fmt.Sprintf("Failed to save service config: %v", err))
		return err
	}

	// Execute plugin install script
	installScript := filepath.Join(c.pluginsDir, pluginName, "bin", "install")
	if _, err := os.Stat(installScript); err == nil {
		c.output.Print(fmt.Sprintf("Running plugin install script for %s...", pluginName))
		cmd := exec.Command("bash", installScript, serviceName, version)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			// Cleanup on error
			os.RemoveAll(serviceDir)
			c.output.Error(fmt.Sprintf("Failed to execute install script: %v", err))
			return err
		}
	}

	c.output.Success(fmt.Sprintf("Service '%s' created successfully", serviceName))
	return nil
}

// Destroy remove um serviço
func (c *ServicesCommand) Destroy(serviceName string) error {
	if !c.serviceExists(serviceName) {
		c.output.Error(fmt.Sprintf("Service '%s' not found", serviceName))
		return fmt.Errorf("service not found")
	}

	// Get service config to know which plugin it belongs to
	svc, err := c.getServiceConfig(serviceName)
	if err != nil {
		c.output.Error(fmt.Sprintf("Failed to get service config: %v", err))
		return err
	}

	// Find and stop all containers related to this service
	containers, err := c.findServiceContainers(serviceName)
	if err != nil {
		c.output.Print(fmt.Sprintf("Warning: Failed to find service containers: %v", err))
	}

	// Stop and remove all found containers
	for _, containerName := range containers {
		c.output.Print(fmt.Sprintf("-----> Stopping container: %s", containerName))
		if err := pkg.StopContainer(containerName); err != nil {
			c.output.Print(fmt.Sprintf("Warning: Failed to stop container %s: %v", containerName, err))
		}

		c.output.Print(fmt.Sprintf("-----> Removing container: %s", containerName))
		if err := pkg.RemoveContainer(containerName, true); err != nil {
			c.output.Print(fmt.Sprintf("Warning: Failed to remove container %s: %v", containerName, err))
		}
	}

	// Also try to stop/remove the container ID from service config (legacy support)
	if svc.ContainerID != "" {
		c.output.Print(fmt.Sprintf("-----> Stopping legacy container: %s", svc.ContainerID))
		if err := pkg.StopContainer(svc.ContainerID); err != nil {
			c.output.Print(fmt.Sprintf("Warning: Failed to stop legacy container: %v", err))
		}

		c.output.Print(fmt.Sprintf("-----> Removing legacy container: %s", svc.ContainerID))
		if err := pkg.RemoveContainer(svc.ContainerID, true); err != nil {
			c.output.Print(fmt.Sprintf("Warning: Failed to remove legacy container: %v", err))
		}
	}

	// Execute plugin uninstall script
	uninstallScript := filepath.Join(c.pluginsDir, svc.Plugin, "uninstall")
	if _, err := os.Stat(uninstallScript); err == nil {
		c.output.Print("-----> Executing plugin uninstall script")
		cmd := exec.Command("bash", uninstallScript, serviceName)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			c.output.Error(fmt.Sprintf("Failed to execute uninstall script: %v", err))
			return err
		}
	}

	// Remove service directory
	serviceDir := filepath.Join(c.servicesDir, serviceName)
	c.output.Print("-----> Removing service directory")
	if err := os.RemoveAll(serviceDir); err != nil {
		c.output.Error(fmt.Sprintf("Failed to remove service directory: %v", err))
		return err
	}

	c.output.Success(fmt.Sprintf("Service '%s' destroyed successfully", serviceName))
	return nil
}

// Link vincula um serviço a uma aplicação
func (c *ServicesCommand) Link(serviceName, appName string) error {
	env := "default"
	// Check if service exists
	svc, err := c.getServiceConfig(serviceName)
	if err != nil {
		c.output.Error(fmt.Sprintf("Service '%s' not found: %v", serviceName, err))
		return err
	}

	// Check if app exists
	if !c.appExists(appName, env) {
		c.output.Error(fmt.Sprintf("App '%s' not found", appName))
		return fmt.Errorf("app not found")
	}

	// Update service config
	if !c.isAppLinked(svc.LinkedApps, appName, env) {
		svc.LinkedApps = append(svc.LinkedApps, appName)
		if err := c.saveServiceConfig(serviceName, svc); err != nil {
			c.output.Error(fmt.Sprintf("Failed to update service config: %v", err))
			return err
		}
	}

	// Add environment variables to app
	if err := c.addServiceEnvVars(serviceName, appName, svc.Plugin); err != nil {
		c.output.Error(fmt.Sprintf("Failed to add environment variables: %v", err))
		return err
	}

	c.output.Success(fmt.Sprintf("Service '%s' linked to '%s'", serviceName, appName))
	return nil
}

// Unlink desvincula um serviço de uma aplicação
func (c *ServicesCommand) Unlink(serviceName, appName string) error {
	// Check if service exists
	svc, err := c.getServiceConfig(serviceName)
	if err != nil {
		c.output.Error(fmt.Sprintf("Service '%s' not found: %v", serviceName, err))
		return err
	}

	// Remove environment variables from app
	if err := c.removeServiceEnvVars(serviceName, appName, svc.Plugin); err != nil {
		c.output.Error(fmt.Sprintf("Failed to remove environment variables: %v", err))
		return err
	}

	// Remove app from linked apps
	var newLinkedApps []string
	for _, linked := range svc.LinkedApps {
		if linked != appName {
			newLinkedApps = append(newLinkedApps, linked)
		}
	}

	svc.LinkedApps = newLinkedApps
	if err := c.saveServiceConfig(serviceName, svc); err != nil {
		c.output.Error(fmt.Sprintf("Failed to update service config: %v", err))
		return err
	}

	c.output.Success(fmt.Sprintf("Service '%s' unlinked from '%s'", serviceName, appName))
	return nil
}

// Info exibe informações de um serviço
func (c *ServicesCommand) Info(serviceName string) error {
	svc, err := c.getServiceConfig(serviceName)
	if err != nil {
		c.output.Error(fmt.Sprintf("Service '%s' not found", serviceName))
		return err
	}

	// Executar comando de info do plugin
	infoCommand := filepath.Join(c.pluginsDir, svc.Plugin, "commands", "info")
	if _, err := os.Stat(infoCommand); err == nil {
		cmd := exec.Command("bash", infoCommand, serviceName)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			c.output.Error(fmt.Sprintf("Failed to get service info: %v", err))
			return err
		}
		return nil
	}

	// Fallback: show service info
	status := "stopped"
	if pkg.ContainerIsRunning(serviceName) {
		status = "running"
	}

	result := ServiceInfo{
		Name:        svc.Name,
		Plugin:      svc.Plugin,
		Status:      status,
		Running:     svc.Running,
		LinkedApps:  svc.LinkedApps,
		ContainerID: svc.ContainerID,
		CreatedAt:   svc.CreatedAt,
		Config:      svc.Config,
	}

	c.output.Data(result)
	return nil
}

// Logs exibe os logs de um serviço
func (c *ServicesCommand) Logs(serviceName string, follow bool) error {
	svc, err := c.getServiceConfig(serviceName)
	if err != nil {
		c.output.Error(fmt.Sprintf("Service '%s' not found", serviceName))
		return err
	}

	// Executar comando de logs do plugin
	logsCommand := filepath.Join(c.pluginsDir, svc.Plugin, "commands", "logs")
	if _, err := os.Stat(logsCommand); err == nil {
		args := []string{logsCommand, serviceName}
		if follow {
			args = append(args, "-f")
		}

		cmd := exec.Command("bash", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin

		if err := cmd.Run(); err != nil {
			c.output.Error(fmt.Sprintf("Failed to get service logs: %v", err))
			return err
		}
		return nil
	}

	c.output.Error(fmt.Sprintf("Logs command not found for plugin '%s'", svc.Plugin))
	return fmt.Errorf("logs command not found")
}

// ExecuteCommand executa um comando de plugin em um serviço
func (c *ServicesCommand) ExecuteCommand(pluginName, command, serviceName string, args []string) error {
	commandPath := filepath.Join(c.pluginsDir, pluginName, "commands", command)

	cmdArgs := append([]string{commandPath, serviceName}, args...)
	cmd := exec.Command("bash", cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		c.output.Error(fmt.Sprintf("Failed to execute command: %v", err))
		return err
	}

	return nil
}

// Get obtém informações de um serviço específico
func (c *ServicesCommand) Get(serviceName string) error {
	svc, err := c.getServiceConfig(serviceName)
	if err != nil {
		c.output.Error(fmt.Sprintf("Service '%s' not found", serviceName))
		return err
	}

	status := "stopped"
	if pkg.ContainerIsRunning(serviceName) {
		status = "running"
	}

	result := ServiceInfo{
		Name:        svc.Name,
		Plugin:      svc.Plugin,
		Status:      status,
		Running:     svc.Running,
		LinkedApps:  svc.LinkedApps,
		ContainerID: svc.ContainerID,
		CreatedAt:   svc.CreatedAt,
		Config:      svc.Config,
	}

	c.output.Data(result)
	return nil
}

// UpdateServiceConfig atualiza a configuração de um serviço
func (c *ServicesCommand) UpdateServiceConfig(serviceName string, config map[string]string) error {
	svc, err := c.getServiceConfig(serviceName)
	if err != nil {
		c.output.Error(fmt.Sprintf("Service '%s' not found", serviceName))
		return err
	}

	svc.Config = config
	if err := c.saveServiceConfig(serviceName, svc); err != nil {
		c.output.Error(fmt.Sprintf("Failed to update service config: %v", err))
		return err
	}

	c.output.Success(fmt.Sprintf("Service '%s' config updated successfully", serviceName))
	return nil
}

// Métodos privados

func (c *ServicesCommand) listServices() ([]service, error) {
	var services []service

	if _, err := os.Stat(c.servicesDir); os.IsNotExist(err) {
		return []service{}, nil
	}

	entries, err := os.ReadDir(c.servicesDir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			svc, err := c.getServiceConfig(entry.Name())
			if err != nil {
				continue // Skip invalid services
			}
			services = append(services, svc)
		}
	}

	return services, nil
}

func (c *ServicesCommand) getServiceConfig(serviceName string) (service, error) {
	configPath := filepath.Join(c.servicesDir, serviceName, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return service{}, err
	}

	var svc service
	if err := json.Unmarshal(data, &svc); err != nil {
		return service{}, err
	}

	return svc, nil
}

func (c *ServicesCommand) saveServiceConfig(serviceName string, svc service) error {
	configPath := filepath.Join(c.servicesDir, serviceName, "config.json")
	data, err := json.Marshal(svc)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

func (c *ServicesCommand) pluginExists(pluginName string) bool {
	pluginPath := filepath.Join(c.pluginsDir, pluginName)
	_, err := os.Stat(pluginPath)
	return !os.IsNotExist(err)
}

func (c *ServicesCommand) serviceExists(serviceName string) bool {
	servicePath := filepath.Join(c.servicesDir, serviceName)
	_, err := os.Stat(servicePath)
	return !os.IsNotExist(err)
}

func (c *ServicesCommand) appExists(appName, env string) bool {
	appPath := filepath.Join(c.baseDir, "apps", appName)
	_, err := os.Stat(appPath)
	return !os.IsNotExist(err)
}

func (c *ServicesCommand) isAppLinked(linkedApps []string, appName, env string) bool {
	for _, linked := range linkedApps {
		if linked == appName {
			return true
		}
	}
	return false
}

func (c *ServicesCommand) addServiceEnvVars(serviceName, appName, pluginName string) error {
	// Get service configuration
	svc, err := c.getServiceConfig(serviceName)
	if err != nil {
		return err
	}

	// Get environment variables based on plugin type
	envVars := c.getServiceEnvVars(serviceName, svc.Config, pluginName)
	if len(envVars) == 0 {
		return nil
	}

	// Get app env file path
	envFile := filepath.Join(c.baseDir, "apps", appName, "shared", ".env")

	// Load existing env vars
	existingVars := util.LoadEnvFile(envFile)

	// Add service env vars
	for key, value := range envVars {
		existingVars[key] = value
	}

	// Save updated env vars
	return util.SaveEnvFile(envFile, existingVars)
}

func (c *ServicesCommand) removeServiceEnvVars(serviceName, appName, pluginName string) error {
	// Get environment variable keys based on plugin type
	envKeys := c.getServiceEnvKeys(pluginName)
	if len(envKeys) == 0 {
		return nil
	}

	// Get app env file path
	envFile := filepath.Join(c.baseDir, "apps", appName, "shared", ".env")

	// Load existing env vars
	existingVars := util.LoadEnvFile(envFile)

	// Remove service env vars
	for _, key := range envKeys {
		delete(existingVars, key)
	}

	// Save updated env vars
	return util.SaveEnvFile(envFile, existingVars)
}

func (c *ServicesCommand) getServiceEnvVars(serviceName string, config map[string]string, pluginName string) map[string]string {
	envVars := make(map[string]string)

	switch pluginName {
	case "postgres":
		host := "localhost"
		port := config["port"]
		user := config["user"]
		password := config["password"]
		database := config["database"]

		if port == "" || user == "" || password == "" || database == "" {
			return envVars
		}

		envVars["DATABASE_URL"] = fmt.Sprintf("postgres://%s:%s@%s:%s/%s", user, password, host, port, database)
		envVars["POSTGRES_HOST"] = host
		envVars["POSTGRES_PORT"] = port
		envVars["POSTGRES_USER"] = user
		envVars["POSTGRES_PASSWORD"] = password
		envVars["POSTGRES_DB"] = database

	case "redis":
		host := "localhost"
		port := config["port"]
		password := config["password"]

		if port == "" || password == "" {
			return envVars
		}

		envVars["REDIS_URL"] = fmt.Sprintf("redis://:%s@%s:%s", password, host, port)
		envVars["REDIS_HOST"] = host
		envVars["REDIS_PORT"] = port
		envVars["REDIS_PASSWORD"] = password
	}

	return envVars
}

func (c *ServicesCommand) getServiceEnvKeys(pluginName string) []string {
	switch pluginName {
	case "postgres":
		return []string{
			"DATABASE_URL",
			"POSTGRES_HOST",
			"POSTGRES_PORT",
			"POSTGRES_USER",
			"POSTGRES_PASSWORD",
			"POSTGRES_DB",
		}
	case "redis":
		return []string{
			"REDIS_URL",
			"REDIS_HOST",
			"REDIS_PORT",
			"REDIS_PASSWORD",
		}
	default:
		return []string{}
	}
}

func (c *ServicesCommand) findServiceContainers(serviceName string) ([]string, error) {
	var containers []string

	// Find containers with exact name match
	checkCmd := exec.Command("docker", "ps", "-aq", "-f", fmt.Sprintf("name=^%s$", serviceName))
	output, err := checkCmd.Output()
	if err == nil && len(strings.TrimSpace(string(output))) > 0 {
		containerIDs := strings.Split(strings.TrimSpace(string(output)), "\n")
		for _, id := range containerIDs {
			if id != "" {
				// Get container name from ID
				nameCmd := exec.Command("docker", "inspect", "--format", "{{.Name}}", id)
				nameOutput, err := nameCmd.Output()
				if err == nil {
					containerName := strings.TrimPrefix(strings.TrimSpace(string(nameOutput)), "/")
					containers = append(containers, containerName)
				}
			}
		}
	}

	// Also find containers that start with the service name (for services with multiple containers)
	patternCmd := exec.Command("docker", "ps", "-aq", "-f", fmt.Sprintf("name=^%s-", serviceName))
	patternOutput, err := patternCmd.Output()
	if err == nil && len(strings.TrimSpace(string(patternOutput))) > 0 {
		containerIDs := strings.Split(strings.TrimSpace(string(patternOutput)), "\n")
		for _, id := range containerIDs {
			if id != "" {
				// Get container name from ID
				nameCmd := exec.Command("docker", "inspect", "--format", "{{.Name}}", id)
				nameOutput, err := nameCmd.Output()
				if err == nil {
					containerName := strings.TrimPrefix(strings.TrimSpace(string(nameOutput)), "/")
					// Avoid duplicates
					found := false
					for _, existing := range containers {
						if existing == containerName {
							found = true
							break
						}
					}
					if !found {
						containers = append(containers, containerName)
					}
				}
			}
		}
	}

	return containers, nil
}
