package containers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.gokku-vm.com/pkg"
)

// ContainerRegistry manages container information
type ContainerRegistry struct {
	basePath string
}

// NewContainerRegistry creates a new container registry
func NewContainerRegistry() *ContainerRegistry {
	return &ContainerRegistry{
		basePath: "/opt/gokku/apps",
	}
}

// SaveContainerInfo saves container information to disk
func (cr *ContainerRegistry) SaveContainerInfo(info pkg.ContainerInfo) error {
	appPath := filepath.Join(cr.basePath, info.AppName)
	containersPath := filepath.Join(appPath, "containers", info.ProcessType)

	// Create directory if it doesn't exist
	if err := os.MkdirAll(containersPath, 0755); err != nil {
		return fmt.Errorf("failed to create containers directory: %w", err)
	}

	// Create filename based on container number
	filename := fmt.Sprintf("%d.json", info.Number)
	filePath := filepath.Join(containersPath, filename)

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal container info: %w", err)
	}

	return os.WriteFile(filePath, data, 0644)
}

// LoadContainerInfo loads container information from disk
func (cr *ContainerRegistry) LoadContainerInfo(appName, processType string, number int) (*pkg.ContainerInfo, error) {
	appPath := filepath.Join(cr.basePath, appName)
	containersPath := filepath.Join(appPath, "containers", processType)

	filename := fmt.Sprintf("%d.json", number)
	filePath := filepath.Join(containersPath, filename)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read container info file: %w", err)
	}

	var info pkg.ContainerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal container info: %w", err)
	}

	return &info, nil
}

// ListContainers lists all containers for an app
func (cr *ContainerRegistry) ListContainers(appName string) ([]pkg.ContainerInfo, error) {
	appPath := filepath.Join(cr.basePath, appName)
	containersPath := filepath.Join(appPath, "containers")

	if _, err := os.Stat(containersPath); os.IsNotExist(err) {
		return []pkg.ContainerInfo{}, nil
	}

	var containers []pkg.ContainerInfo

	// Walk through all process types
	processDirs, err := os.ReadDir(containersPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read containers directory: %w", err)
	}

	for _, processDir := range processDirs {
		if !processDir.IsDir() {
			continue
		}

		processType := processDir.Name()
		processPath := filepath.Join(containersPath, processType)

		containerFiles, err := os.ReadDir(processPath)
		if err != nil {
			continue // Skip if can't read process directory
		}

		for _, containerFile := range containerFiles {
			if containerFile.IsDir() || !strings.HasSuffix(containerFile.Name(), ".json") {
				continue
			}

			// Extract number from filename
			name := containerFile.Name()
			numberStr := name[:len(name)-5] // Remove .json

			var number int
			if _, err := fmt.Sscanf(numberStr, "%d", &number); err != nil {
				continue // Skip invalid files
			}

			info, err := cr.LoadContainerInfo(appName, processType, number)
			if err != nil {
				continue // Skip corrupted files
			}

			containers = append(containers, *info)
		}
	}

	// Sort by creation time (newest first)
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].CreatedAt > containers[j].CreatedAt
	})

	return containers, nil
}

// DeleteContainerInfo deletes container information from disk
func (cr *ContainerRegistry) DeleteContainerInfo(appName, processType string, number int) error {
	appPath := filepath.Join(cr.basePath, appName)
	containersPath := filepath.Join(appPath, "containers", processType)

	filename := fmt.Sprintf("%d.json", number)
	filePath := filepath.Join(containersPath, filename)

	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete container info file: %w", err)
	}

	return nil
}

// GetNextContainerNumber gets the next available container number for a process type
func (cr *ContainerRegistry) GetNextContainerNumber(appName, processType string) (int, error) {
	containers, err := cr.ListContainers(appName)
	if err != nil {
		return 1, err
	}

	maxNumber := 0
	for _, container := range containers {
		if container.ProcessType == processType && container.Number > maxNumber {
			maxNumber = container.Number
		}
	}

	return maxNumber + 1, nil
}

// ContainerExists checks if a container exists
func (cr *ContainerRegistry) ContainerExists(appName string, processType string, number int) bool {
	appPath := filepath.Join(cr.basePath, appName)
	containersPath := filepath.Join(appPath, "containers", processType)

	filename := fmt.Sprintf("%d.json", number)
	filePath := filepath.Join(containersPath, filename)

	_, err := os.Stat(filePath)
	return !os.IsNotExist(err)
}

// GetContainerPort gets the host port for a container
func (cr *ContainerRegistry) GetContainerPort(appName string, processType string, number int) (int, error) {
	info, err := cr.LoadContainerInfo(appName, processType, number)
	if err != nil {
		return 0, err
	}
	return info.HostPort, nil
}

// UpdateContainerStatus updates the status of a container
func (cr *ContainerRegistry) UpdateContainerStatus(appName string, processType string, number int, status string) error {
	info, err := cr.LoadContainerInfo(appName, processType, number)
	if err != nil {
		return err
	}

	info.Status = status
	return cr.SaveContainerInfo(*info)
}

// GetActiveContainers returns containers that are currently running
func (cr *ContainerRegistry) GetActiveContainers(appName string) ([]pkg.ContainerInfo, error) {
	allContainers, err := cr.ListContainers(appName)
	if err != nil {
		return nil, err
	}

	var active []pkg.ContainerInfo
	for _, container := range allContainers {
		if container.Status == "running" {
			active = append(active, container)
		}
	}

	return active, nil
}
