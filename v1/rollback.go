package v1

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.gokku-vm.com/pkg"
)

// RollbackCommand gerencia rollback de aplicações
type RollbackCommand struct {
	output  Output
	baseDir string
}

// NewRollbackCommand cria uma nova instância de RollbackCommand
func NewRollbackCommand(output Output) *RollbackCommand {
	baseDir := os.Getenv("GOKKU_ROOT")
	if baseDir == "" {
		baseDir = "/opt/gokku"
	}

	return &RollbackCommand{
		output:  output,
		baseDir: baseDir,
	}
}

// Execute executa rollback para uma aplicação
func (c *RollbackCommand) Execute(appName string, releaseID string) error {
	appDir := filepath.Join(c.baseDir, "apps", appName)
	releasesDir := filepath.Join(appDir, "releases")

	// Se releaseID não foi fornecido, pegar o release anterior
	if releaseID == "" {
		var err error
		releaseID, err = c.getPreviousRelease(releasesDir)
		if err != nil {
			c.output.Error(fmt.Sprintf("Failed to get previous release: %v", err))
			return err
		}
	}

	if releaseID == "" {
		c.output.Error("No previous release found")
		return fmt.Errorf("no previous release found")
	}

	// Verificar se o release existe
	releaseDir := filepath.Join(releasesDir, releaseID)
	if _, err := os.Stat(releaseDir); os.IsNotExist(err) {
		c.output.Error(fmt.Sprintf("Release '%s' not found", releaseID))
		return fmt.Errorf("release not found")
	}

	c.output.Print(fmt.Sprintf("Rolling back %s to release: %s", appName, releaseID))

	// Verificar se o container existe
	if !pkg.ContainerExists(appName) {
		c.output.Error(fmt.Sprintf("Container '%s' not found", appName))
		return fmt.Errorf("container not found")
	}

	// Parar e remover container atual
	c.output.Print("-----> Stopping current container...")
	if err := pkg.StopContainer(appName); err != nil {
		c.output.Print(fmt.Sprintf("Warning: Failed to stop container: %v", err))
	}

	if err := pkg.RemoveContainer(appName, true); err != nil {
		c.output.Print(fmt.Sprintf("Warning: Failed to remove container: %v", err))
	}

	// Criar novo container com a imagem do release
	envFile := filepath.Join(appDir, "shared", ".env")
	imageName := fmt.Sprintf("%s:release-%s", appName, releaseID)

	c.output.Print(fmt.Sprintf("-----> Starting container with release %s...", releaseID))

	// Verificar se a imagem existe
	cmd := exec.Command("docker", "images", "-q", imageName)
	output, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(output)) == "" {
		c.output.Error(fmt.Sprintf("Docker image '%s' not found", imageName))
		return fmt.Errorf("docker image not found")
	}

	// Criar e iniciar novo container
	if err := pkg.RecreateActiveContainer(appName, envFile, releaseDir); err != nil {
		c.output.Error(fmt.Sprintf("Failed to start container: %v", err))
		return err
	}

	// Atualizar symlink current
	currentLink := filepath.Join(appDir, "current")
	os.Remove(currentLink)
	if err := os.Symlink(releaseDir, currentLink); err != nil {
		c.output.Print(fmt.Sprintf("Warning: Failed to update current symlink: %v", err))
	}

	c.output.Success(fmt.Sprintf("Rollback complete: %s -> %s", appName, releaseID))
	return nil
}

// ListReleases lista todos os releases disponíveis
func (c *RollbackCommand) ListReleases(appName string) error {
	releasesDir := filepath.Join(c.baseDir, "apps", appName, "releases")

	if _, err := os.Stat(releasesDir); os.IsNotExist(err) {
		c.output.Print("No releases found")
		return nil
	}

	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		c.output.Error(fmt.Sprintf("Failed to read releases directory: %v", err))
		return err
	}

	if len(entries) == 0 {
		c.output.Print("No releases found")
		return nil
	}

	// Ler current symlink para identificar release atual
	currentLink := filepath.Join(c.baseDir, "apps", appName, "current")
	currentRelease := ""
	if linkTarget, err := os.Readlink(currentLink); err == nil {
		currentRelease = filepath.Base(linkTarget)
	}

	var releases []string
	for _, entry := range entries {
		if entry.IsDir() {
			releases = append(releases, entry.Name())
		}
	}

	// Para stdout, usar tabela
	if _, ok := c.output.(*StdoutOutput); ok {
		headers := []string{"Release ID", "Status"}
		var rows [][]string
		for _, release := range releases {
			status := ""
			if release == currentRelease {
				status = "current"
			}
			rows = append(rows, []string{release, status})
		}
		c.output.Table(headers, rows)
	} else {
		// Para JSON, retornar array de objetos
		result := make([]map[string]string, len(releases))
		for i, release := range releases {
			result[i] = map[string]string{
				"release_id": release,
				"current":    fmt.Sprintf("%t", release == currentRelease),
			}
		}
		c.output.Data(result)
	}

	return nil
}

// Métodos privados

func (c *RollbackCommand) getPreviousRelease(releasesDir string) (string, error) {
	cmd := exec.Command("bash", "-c", fmt.Sprintf("cd %s && ls -t | sed -n '2p'", releasesDir))
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	releaseID := strings.TrimSpace(string(output))
	return releaseID, nil
}
