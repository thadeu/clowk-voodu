package pkg

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type RemoteInfo struct {
	Host    string
	BaseDir string
	App     string
}

// ServerConfig represents the gokku.yml configuration on the server
type ServerConfig struct {
	Apps         map[string]App `yaml:"apps"`
	Defaults     *Defaults      `yaml:"defaults,omitempty"`
	Docker       *Docker        `yaml:"docker,omitempty"`
	Environments []Environment  `yaml:"environments,omitempty"`
}

// Config represents the CLI configuration
type Config struct {
	Apps map[string]App `yaml:"apps"`
}

// App represents an application configuration
type App struct {
	Name         string            `yaml:"name,omitempty"`
	Lang         string            `yaml:"lang,omitempty"`
	Path         string            `yaml:"path,omitempty"`
	WorkDir      string            `yaml:"workdir,omitempty"`
	BinaryName   string            `yaml:"binary_name,omitempty"`
	GoVersion    string            `yaml:"go_version,omitempty"`
	Goos         string            `yaml:"goos,omitempty"`
	Goarch       string            `yaml:"goarch,omitempty"`
	CgoEnabled   *bool             `yaml:"cgo_enabled,omitempty"`
	Dockerfile   string            `yaml:"dockerfile,omitempty"`
	Image        string            `yaml:"image,omitempty"`
	Entrypoint   string            `yaml:"entrypoint,omitempty"`
	Command      string            `yaml:"command,omitempty"`
	Env          map[string]string `yaml:"env,omitempty"`
	Volumes      []string          `yaml:"volumes,omitempty"`
	Security     string            `yaml:"security,omitempty"`
	Deployment   *Deployment       `yaml:"deployment,omitempty"`
	Network      *NetworkConfig    `yaml:"network"`
	Ports        []string          `yaml:"ports"`
	Environments []Environment     `yaml:"environments,omitempty"`
}

type NetworkConfig struct {
	Mode string `yaml:"mode,omitempty"`
}

// Deployment represents deployment configuration
type Deployment struct {
	KeepReleases  int      `yaml:"keep_releases,omitempty"`
	KeepImages    int      `yaml:"keep_images,omitempty"`
	RestartPolicy string   `yaml:"restart_policy,omitempty"`
	RestartDelay  int      `yaml:"restart_delay,omitempty"`
	PostDeploy    []string `yaml:"post_deploy,omitempty"`
}

// Environment represents environment-specific app config
type Environment struct {
	Name           string            `yaml:"name"`
	Branch         string            `yaml:"branch,omitempty"`
	DefaultEnvVars map[string]string `yaml:"default_env_vars,omitempty"`
}

// Defaults represents default configurations
type Defaults struct {
	BuildType string `yaml:"build_type,omitempty"`
}

// Docker represents Docker-related configurations
type Docker struct {
	Registry []string `yaml:"registry,omitempty"`
}

// ContainerInfo represents information about a running container
type ContainerInfo struct {
	// Docker ps fields
	ID      string `json:"ID"`
	Names   string `json:"Names"`
	Image   string `json:"Image"`
	Status  string `json:"Status"`
	Ports   string `json:"Ports"`
	Command string `json:"Command"`
	Created string `json:"CreatedAt"`

	// Registry fields
	Name         string `json:"name"`
	AppName      string `json:"app_name"`
	ProcessType  string `json:"process_type"`
	Number       int    `json:"number"`
	HostPort     int    `json:"host_port"`
	InternalPort int    `json:"internal_port"`
	CreatedAt    string `json:"created_at"`
}

// LoadServerConfig loads the server configuration from gokku.yml
func LoadServerConfigByApp(appName string) (*ServerConfig, error) {
	configPath := fmt.Sprintf("/opt/gokku/apps/%s/gokku.yml", appName)

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return &ServerConfig{
			Apps: make(map[string]App),
		}, nil
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config ServerConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &config, nil
}

func LoadAppConfig(appName string) (*App, error) {
	filePath := fmt.Sprintf("/opt/gokku/apps/%s/gokku.yml", appName)

	data, err := os.ReadFile(filePath)

	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var serverConfig ServerConfig

	if err := yaml.Unmarshal(data, &serverConfig); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Find the app by name
	if app, exists := serverConfig.Apps[appName]; exists {
		app.Name = appName
		return &app, nil
	}

	return nil, fmt.Errorf("app '%s' not found in configuration", appName)
}

// GetApp finds an app by name
func (c *ServerConfig) GetApp(name string) (*App, error) {
	if app, exists := c.Apps[name]; exists {
		app.Name = name
		return &app, nil
	}

	return nil, fmt.Errorf("app '%s' not found", name)
}

// Validate validates the server configuration
func (c *ServerConfig) Validate() error {
	if len(c.Apps) == 0 {
		return fmt.Errorf("no apps defined")
	}

	for appName, app := range c.Apps {
		if appName == "" {
			return fmt.Errorf("app name cannot be empty")
		}

		// Validate that either path or image is specified
		if app.Path == "" && app.Image == "" {
			return fmt.Errorf("app '%s' must specify either 'path' or 'image'", appName)
		}
	}

	return nil
}
