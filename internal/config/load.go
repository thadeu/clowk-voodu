package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"go.voodu.clowk.in/internal/paths"
)

// LoadServerConfigByApp reads the app-scoped voodu.yml living at
// <root>/apps/<app>/voodu.yml. Missing files produce an empty config (no error).
func LoadServerConfigByApp(app string) (*ServerConfig, error) {
	path := paths.AppConfigYAML(app)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return &ServerConfig{Apps: make(map[string]App)}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg ServerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	return &cfg, nil
}

// LoadAppConfig extracts the single-app view from the app-scoped voodu.yml.
func LoadAppConfig(app string) (*App, error) {
	cfg, err := LoadServerConfigByApp(app)
	if err != nil {
		return nil, err
	}

	return cfg.GetApp(app)
}

// LoadUserConfig reads ~/.voodu/config.yml. Missing file returns empty config.
func LoadUserConfig() (*Config, error) {
	return loadConfigFile(paths.UserCfgPath())
}

// SaveUserConfig writes ~/.voodu/config.yml (creating the directory if needed).
func SaveUserConfig(cfg *Config) error {
	p := paths.UserCfgPath()

	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	return os.WriteFile(p, data, 0644)
}

// GetApp finds an app by name; returns a non-nil error when the app is unknown.
func (c *ServerConfig) GetApp(name string) (*App, error) {
	if app, exists := c.Apps[name]; exists {
		app.Name = name
		return &app, nil
	}

	return nil, fmt.Errorf("app %q not found", name)
}

// Validate performs basic sanity checks on the server config.
func (c *ServerConfig) Validate() error {
	if len(c.Apps) == 0 {
		return fmt.Errorf("no apps defined")
	}

	for name, app := range c.Apps {
		if name == "" {
			return fmt.Errorf("app name cannot be empty")
		}

		if app.Path == "" && app.Image == "" {
			return fmt.Errorf("app %q must specify either 'path' or 'image'", name)
		}
	}

	return nil
}

// GetAppFromConfig is a convenience for looking up an app in a client Config.
func GetAppFromConfig(c *Config, name string) *App {
	if app, ok := c.Apps[name]; ok {
		return &app
	}

	return nil
}

func loadConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Apps: make(map[string]App)}, nil
		}

		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if cfg.Apps == nil {
		cfg.Apps = make(map[string]App)
	}

	return &cfg, nil
}
