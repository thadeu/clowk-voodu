package config

import (
	"os"
	"path/filepath"

	"go.gokku-vm.com/pkg"

	"gopkg.in/yaml.v3"
)

// GetAppConfig gets an app configuration by name
func GetAppConfig(c *pkg.Config, appName string) *pkg.App {
	if app, exists := c.Apps[appName]; exists {
		return &app
	}
	return nil
}

// LoadConfig loads the configuration from the config file
func LoadConfig() (*pkg.Config, error) {
	configPath := GetConfigPath()

	data, err := os.ReadFile(configPath)

	if err != nil {
		if os.IsNotExist(err) {
			return &pkg.Config{Apps: make(map[string]pkg.App)}, nil
		}
		return nil, err
	}

	var config pkg.Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveConfig saves the configuration to the config file
func SaveConfig(config *pkg.Config) error {
	configPath := GetConfigPath()

	// Ensure directory exists
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, data, 0644)
}

// GetConfigPath returns the path to the configuration file
func GetConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gokku", "config.yml")
}
