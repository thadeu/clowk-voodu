// Package containers persists per-app container metadata under the
// voodu state tree (<root>/apps/<app>/containers) and provides the
// label vocabulary used to identify voodu-managed containers in
// docker (voodu.kind, voodu.scope, voodu.name, voodu.replica_id).
package containers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/paths"
)

// ContainerRegistry manages container information on disk.
type ContainerRegistry struct {
	basePath string
}

// NewContainerRegistry creates a new container registry rooted at the current
// Voodu apps directory.
func NewContainerRegistry() *ContainerRegistry {
	return &ContainerRegistry{
		basePath: paths.AppsDir(),
	}
}

// SaveContainerInfo saves container information to disk.
func (cr *ContainerRegistry) SaveContainerInfo(info docker.ContainerInfo) error {
	appPath := filepath.Join(cr.basePath, info.AppName)
	containersPath := filepath.Join(appPath, "containers", info.ProcessType)

	if err := os.MkdirAll(containersPath, 0755); err != nil {
		return fmt.Errorf("failed to create containers directory: %w", err)
	}

	filename := fmt.Sprintf("%d.json", info.Number)
	filePath := filepath.Join(containersPath, filename)

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal container info: %w", err)
	}

	return os.WriteFile(filePath, data, 0644)
}

// LoadContainerInfo loads container information from disk.
func (cr *ContainerRegistry) LoadContainerInfo(appName, processType string, number int) (*docker.ContainerInfo, error) {
	appPath := filepath.Join(cr.basePath, appName)
	containersPath := filepath.Join(appPath, "containers", processType)

	filename := fmt.Sprintf("%d.json", number)
	filePath := filepath.Join(containersPath, filename)

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read container info file: %w", err)
	}

	var info docker.ContainerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal container info: %w", err)
	}

	return &info, nil
}

// ListContainers lists all containers for an app.
func (cr *ContainerRegistry) ListContainers(appName string) ([]docker.ContainerInfo, error) {
	appPath := filepath.Join(cr.basePath, appName)
	containersPath := filepath.Join(appPath, "containers")

	if _, err := os.Stat(containersPath); os.IsNotExist(err) {
		return []docker.ContainerInfo{}, nil
	}

	var containers []docker.ContainerInfo

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
			continue
		}

		for _, containerFile := range containerFiles {
			if containerFile.IsDir() || !strings.HasSuffix(containerFile.Name(), ".json") {
				continue
			}

			name := containerFile.Name()
			numberStr := name[:len(name)-5]

			var number int
			if _, err := fmt.Sscanf(numberStr, "%d", &number); err != nil {
				continue
			}

			info, err := cr.LoadContainerInfo(appName, processType, number)
			if err != nil {
				continue
			}

			containers = append(containers, *info)
		}
	}

	sort.Slice(containers, func(i, j int) bool {
		return containers[i].CreatedAt > containers[j].CreatedAt
	})

	return containers, nil
}

// DeleteContainerInfo deletes container information from disk.
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

// GetNextContainerNumber gets the next available container number for a process type.
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

// ContainerExists checks if a container entry exists on disk.
func (cr *ContainerRegistry) ContainerExists(appName string, processType string, number int) bool {
	appPath := filepath.Join(cr.basePath, appName)
	containersPath := filepath.Join(appPath, "containers", processType)

	filename := fmt.Sprintf("%d.json", number)
	filePath := filepath.Join(containersPath, filename)

	_, err := os.Stat(filePath)

	return !os.IsNotExist(err)
}

// GetContainerPort gets the host port for a container.
func (cr *ContainerRegistry) GetContainerPort(appName string, processType string, number int) (int, error) {
	info, err := cr.LoadContainerInfo(appName, processType, number)
	if err != nil {
		return 0, err
	}

	return info.HostPort, nil
}

// UpdateContainerStatus updates the status of a container.
func (cr *ContainerRegistry) UpdateContainerStatus(appName string, processType string, number int, status string) error {
	info, err := cr.LoadContainerInfo(appName, processType, number)
	if err != nil {
		return err
	}

	info.Status = status

	return cr.SaveContainerInfo(*info)
}

// GetActiveContainers returns containers that are currently running.
func (cr *ContainerRegistry) GetActiveContainers(appName string) ([]docker.ContainerInfo, error) {
	allContainers, err := cr.ListContainers(appName)
	if err != nil {
		return nil, err
	}

	var active []docker.ContainerInfo
	for _, container := range allContainers {
		if container.Status == "running" {
			active = append(active, container)
		}
	}

	return active, nil
}
