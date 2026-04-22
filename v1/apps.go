package v1

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.gokku-vm.com/pkg"
	"go.gokku-vm.com/pkg/util"
)

// AppsCommand gerencia operações de aplicações
type AppsCommand struct {
	output  Output
	baseDir string
}

// NewAppsCommand cria uma nova instância de AppsCommand
func NewAppsCommand(output Output) *AppsCommand {
	baseDir := os.Getenv("GOKKU_ROOT")
	if baseDir == "" {
		baseDir = "/opt/gokku"
	}

	return &AppsCommand{
		output:  output,
		baseDir: baseDir,
	}
}

// AppInfo representa informações de uma aplicação
type AppInfo struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	ReleasesCount  int    `json:"releases_count"`
	CurrentRelease string `json:"current_release"`
}

// AppDetail representa informações detalhadas de uma aplicação
type AppDetail struct {
	AppInfo
	Config     *pkg.App            `json:"config,omitempty"`
	Containers []pkg.ContainerInfo `json:"containers"`
	EnvVars    map[string]string   `json:"env_vars"`
}

// List lista todas as aplicações
func (c *AppsCommand) List() error {
	apps, err := c.listApps()
	if err != nil {
		c.output.Error(err.Error())
		return err
	}

	if len(apps) == 0 {
		c.output.Print("No apps found")
		return nil
	}

	// Para stdout, usar tabela
	if _, ok := c.output.(*StdoutOutput); ok {
		headers := []string{"App Name", "Status", "Releases", "Current Release"}
		var rows [][]string
		for _, app := range apps {
			rows = append(rows, []string{
				app.Name,
				app.Status,
				fmt.Sprintf("%d", app.ReleasesCount),
				app.CurrentRelease,
			})
		}
		c.output.Table(headers, rows)
	} else {
		// Para JSON, retornar array de objetos
		c.output.Data(apps)
	}

	return nil
}

// Get obtém informações detalhadas de uma aplicação
func (c *AppsCommand) Get(appName string) error {
	app, err := c.getApp(appName)
	if err != nil {
		c.output.Error(err.Error())
		return err
	}

	c.output.Data(app)
	return nil
}

// Create cria uma nova aplicação
func (c *AppsCommand) Create(appName string, deployUser string) error {
	if c.appExists(appName) {
		c.output.Error(fmt.Sprintf("App '%s' already exists", appName))
		return fmt.Errorf("app already exists")
	}

	// Criar estrutura de diretórios
	if err := c.createDirectoryStructure(appName); err != nil {
		c.output.Error(fmt.Sprintf("Failed to create directory structure: %v", err))
		return err
	}

	// Inicializar repositório git
	if err := c.setupGitRepository(appName); err != nil {
		c.output.Error(fmt.Sprintf("Failed to setup git repository: %v", err))
		return err
	}

	// Criar arquivo .env inicial
	if err := c.createInitialEnvFile(appName); err != nil {
		c.output.Error(fmt.Sprintf("Failed to create .env file: %v", err))
		return err
	}

	// Criar hook post-receive
	if err := c.setupPostReceiveHook(appName); err != nil {
		c.output.Error(fmt.Sprintf("Failed to setup hook: %v", err))
		return err
	}

	c.output.Success(fmt.Sprintf("App '%s' created successfully", appName))
	return nil
}

// Destroy remove uma aplicação
func (c *AppsCommand) Destroy(appName string) error {
	if !c.appExists(appName) {
		c.output.Error(fmt.Sprintf("App '%s' not found", appName))
		return fmt.Errorf("app not found")
	}

	// Parar e remover containers
	if pkg.ContainerExists(appName) {
		pkg.StopContainer(appName)
		pkg.RemoveContainer(appName, true)
	}

	// Remover diretório da app
	appDir := filepath.Join(c.baseDir, "apps", appName)
	if err := os.RemoveAll(appDir); err != nil {
		c.output.Error(fmt.Sprintf("Failed to remove app directory: %v", err))
		return err
	}

	// Remover repositório
	repoDir := filepath.Join(c.baseDir, "repos", appName+".git")
	if err := os.RemoveAll(repoDir); err != nil {
		c.output.Error(fmt.Sprintf("Failed to remove repository: %v", err))
		return err
	}

	c.output.Success(fmt.Sprintf("App '%s' destroyed successfully", appName))
	return nil
}

// AppExists verifica se uma aplicação existe
func (c *AppsCommand) AppExists(appName string) bool {
	return c.appExists(appName)
}

// Métodos privados (lógica interna movida de internal/services/apps.go)

func (c *AppsCommand) listApps() ([]AppInfo, error) {
	appsDir := filepath.Join(c.baseDir, "apps")

	if _, err := os.Stat(appsDir); os.IsNotExist(err) {
		return []AppInfo{}, nil
	}

	entries, err := os.ReadDir(appsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read apps directory: %w", err)
	}

	var apps []AppInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		appName := entry.Name()
		status := c.getAppStatus(appName)
		releasesCount := c.countReleases(appName)
		currentRelease := c.getCurrentRelease(appName)

		apps = append(apps, AppInfo{
			Name:           appName,
			Status:         status,
			ReleasesCount:  releasesCount,
			CurrentRelease: currentRelease,
		})
	}

	return apps, nil
}

func (c *AppsCommand) getApp(appName string) (*AppDetail, error) {
	if !c.appExists(appName) {
		return nil, fmt.Errorf("app '%s' not found", appName)
	}

	status := c.getAppStatus(appName)
	releasesCount := c.countReleases(appName)
	currentRelease := c.getCurrentRelease(appName)

	config, _ := pkg.LoadAppConfig(appName)
	containers, _ := c.getAppContainers(appName)
	envVars := c.getAppEnvVars(appName)
	if envVars == nil {
		envVars = make(map[string]string)
	}

	return &AppDetail{
		AppInfo: AppInfo{
			Name:           appName,
			Status:         status,
			ReleasesCount:  releasesCount,
			CurrentRelease: currentRelease,
		},
		Config:     config,
		Containers: containers,
		EnvVars:    envVars,
	}, nil
}

func (c *AppsCommand) appExists(appName string) bool {
	appDir := filepath.Join(c.baseDir, "apps", appName)
	_, err := os.Stat(appDir)
	return err == nil
}

func (c *AppsCommand) getAppStatus(appName string) string {
	if pkg.ContainerIsRunning(appName) {
		return "running"
	} else if pkg.ContainerExists(appName) {
		return "stopped"
	}
	return "not deployed"
}

func (c *AppsCommand) countReleases(appName string) int {
	releasesDir := filepath.Join(c.baseDir, "apps", appName, "releases")
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return 0
	}
	return len(entries)
}

func (c *AppsCommand) getCurrentRelease(appName string) string {
	currentLink := filepath.Join(c.baseDir, "apps", appName, "current")
	linkTarget, err := os.Readlink(currentLink)
	if err != nil {
		return "none"
	}
	return filepath.Base(linkTarget)
}

func (c *AppsCommand) getAppContainers(appName string) ([]pkg.ContainerInfo, error) {
	containers, err := pkg.ListContainers(false)
	if err != nil {
		return nil, err
	}

	var appContainers []pkg.ContainerInfo
	for _, container := range containers {
		if strings.Contains(container.Name, appName) {
			appContainers = append(appContainers, container)
		}
	}

	return appContainers, nil
}

func (c *AppsCommand) getAppEnvVars(appName string) map[string]string {
	envFile := filepath.Join(c.baseDir, "apps", appName, "shared", ".env")
	return util.LoadEnvFile(envFile)
}

func (c *AppsCommand) createDirectoryStructure(appName string) error {
	dirs := []string{
		filepath.Join(c.baseDir, "repos"),
		filepath.Join(c.baseDir, "apps", appName, "releases"),
		filepath.Join(c.baseDir, "apps", appName, "shared"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return nil
}

func (c *AppsCommand) setupGitRepository(appName string) error {
	repoDir := filepath.Join(c.baseDir, "repos", appName+".git")

	if _, err := os.Stat(filepath.Join(repoDir, "HEAD")); err == nil {
		return nil // Repository already exists
	}

	cmd := exec.Command("git", "init", "--bare", repoDir, "--initial-branch=main")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to initialize git repository: %v (output: %s)", err, string(output))
	}

	return nil
}

func (c *AppsCommand) createInitialEnvFile(appName string) error {
	envFile := filepath.Join(c.baseDir, "apps", appName, "shared", ".env")
	envContent := fmt.Sprintf(`# App: %s
# Generated: %s
ZERO_DOWNTIME=0
`, appName, time.Now().Format("2006-01-02 15:04:05"))

	return os.WriteFile(envFile, []byte(envContent), 0644)
}

func (c *AppsCommand) setupPostReceiveHook(appName string) error {
	repoDir := filepath.Join(c.baseDir, "repos", appName+".git")
	hookDir := filepath.Join(repoDir, "hooks")
	hookFile := filepath.Join(hookDir, "post-receive")

	if err := os.MkdirAll(hookDir, 0755); err != nil {
		return fmt.Errorf("failed to create hooks directory: %w", err)
	}

	hookContent := fmt.Sprintf(`#!/bin/bash
set -e

APP_NAME="%s"

echo "-----> Received push for $APP_NAME"

# Check if repository has commits
if git rev-parse --verify HEAD >/dev/null 2>&1; then
    echo "-----> Repository has commits"

    # Get the current HEAD branch
    CURRENT_HEAD_REF=$(git symbolic-ref HEAD 2>/dev/null || echo "")
    CURRENT_HEAD_BRANCH=$(basename "$CURRENT_HEAD_REF" 2>/dev/null || echo "")

    echo "-----> Deploying from branch: $CURRENT_HEAD_BRANCH"

    # Execute deployment using the centralized deploy command
    gokku deploy -a "$APP_NAME"

    echo "-----> Deployment completed"
else
    echo "-----> Repository is empty, skipping deployment"
    echo "-----> Run 'gokku deploy $APP_NAME' manually after your first push"
fi

echo "-----> Done"
`, appName)

	if err := os.WriteFile(hookFile, []byte(hookContent), 0755); err != nil {
		return fmt.Errorf("failed to create post-receive hook: %w", err)
	}

	return nil
}
