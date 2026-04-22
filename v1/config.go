package v1

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.gokku-vm.com/pkg"
	"go.gokku-vm.com/pkg/util"
)

// ConfigCommand gerencia configurações de aplicações
type ConfigCommand struct {
	output  Output
	baseDir string
}

// NewConfigCommand cria uma nova instância de ConfigCommand
func NewConfigCommand(output Output) *ConfigCommand {
	baseDir := os.Getenv("GOKKU_ROOT")
	if baseDir == "" {
		baseDir = "/opt/gokku"
	}

	return &ConfigCommand{
		output:  output,
		baseDir: baseDir,
	}
}

// Set define variáveis de ambiente (movido de internal/services/config.go)
func (c *ConfigCommand) Set(appName string, pairs []string) error {
	envFile := c.getEnvFilePath(appName)
	envVars := util.LoadEnvFile(envFile)

	// Parse and update env vars
	for _, pair := range pairs {
		parts := strings.SplitN(pair, "=", 2)

		if len(parts) != 2 {
			c.output.Error(fmt.Sprintf("invalid format '%s', expected KEY=VALUE", pair))
			return fmt.Errorf("invalid format")
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		envVars[key] = value
	}

	if err := util.SaveEnvFile(envFile, envVars); err != nil {
		c.output.Error(err.Error())
		return err
	}

	// Para stdout, mostrar as variáveis definidas
	if _, ok := c.output.(*StdoutOutput); ok {
		for _, pair := range pairs {
			c.output.Print(pair)
		}
	} else {
		// Para JSON, retornar as variáveis
		c.output.Data(map[string]interface{}{
			"app":  appName,
			"vars": pairs,
		})
	}

	return nil
}

// Get obtém uma variável de ambiente (movido de internal/services/config.go)
func (c *ConfigCommand) Get(appName, key string) error {
	envVars := c.listEnvVars(appName)

	value, ok := envVars[key]
	if !ok {
		c.output.Error(fmt.Sprintf("variable '%s' not found", key))
		return fmt.Errorf("variable not found")
	}

	// Para stdout, mostrar KEY=VALUE
	if _, ok := c.output.(*StdoutOutput); ok {
		c.output.Print(key + "=" + value)
	} else {
		// Para JSON, retornar objeto
		c.output.Data(map[string]string{
			"key":   key,
			"value": value,
		})
	}

	return nil
}

// List lista todas as variáveis de ambiente (movido de internal/services/config.go)
func (c *ConfigCommand) List(appName string) error {
	envVars := c.listEnvVars(appName)

	if len(envVars) == 0 {
		c.output.Print("No environment variables set")
		return nil
	}

	// Para stdout, mostrar KEY=VALUE
	if _, ok := c.output.(*StdoutOutput); ok {
		// Ordenar as chaves
		keys := make([]string, 0, len(envVars))
		for k := range envVars {
			keys = append(keys, k)
		}

		// Bubble sort simples
		for i := 0; i < len(keys); i++ {
			for j := i + 1; j < len(keys); j++ {
				if keys[i] > keys[j] {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
		}

		for _, key := range keys {
			c.output.Print(key + "=" + envVars[key])
		}
	} else {
		// Para JSON, retornar objeto
		c.output.Data(envVars)
	}

	return nil
}

// Unset remove variáveis de ambiente (movido de internal/services/config.go)
func (c *ConfigCommand) Unset(appName string, keys []string) error {
	envFile := c.getEnvFilePath(appName)
	envVars := util.LoadEnvFile(envFile)

	for _, key := range keys {
		delete(envVars, key)
	}

	if err := util.SaveEnvFile(envFile, envVars); err != nil {
		c.output.Error(err.Error())
		return err
	}

	// Para stdout, mostrar as variáveis removidas
	if _, ok := c.output.(*StdoutOutput); ok {
		for _, key := range keys {
			c.output.Print("Unset " + key)
		}
	} else {
		// Para JSON, retornar as variáveis removidas
		c.output.Data(map[string]interface{}{
			"app":  appName,
			"keys": keys,
		})
	}

	return nil
}

// Reload reinicia a aplicação para aplicar as mudanças (movido de internal/services/config.go)
func (c *ConfigCommand) Reload(appName string) error {
	envFile := c.getEnvFilePath(appName)
	appDir := filepath.Join(c.baseDir, "apps", appName, "current")

	if err := pkg.RecreateActiveContainer(appName, envFile, appDir); err != nil {
		c.output.Error(fmt.Sprintf("Failed to reload app: %v", err))
		return err
	}

	c.output.Success(fmt.Sprintf("App '%s' reloaded successfully", appName))
	return nil
}

// Métodos privados (movidos de internal/services/config.go)

func (c *ConfigCommand) getEnvFilePath(appName string) string {
	return filepath.Join(c.baseDir, "apps", appName, "shared", ".env")
}

func (c *ConfigCommand) listEnvVars(appName string) map[string]string {
	envFile := c.getEnvFilePath(appName)
	return util.LoadEnvFile(envFile)
}
