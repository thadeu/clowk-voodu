package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type AbstractGitClient interface {
	AddRemote(remoteName string, remoteURL string) (string, error)
	RemoveRemote(remoteName string) (string, error)
	GetRemoteURL(remoteName string) (string, error)
}

type GitClient struct{}

func (c *GitClient) ExecuteCommand(command ...string) ([]byte, error) {
	cmd := exec.Command("git", command...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return nil, err
	}

	return output, nil
}

func (c *GitClient) ExecuteCommandWithContext(ctx context.Context, command ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", command...)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	output, err := cmd.CombinedOutput()

	if err != nil {
		return nil, err
	}

	return output, nil
}

func (c *GitClient) GetRemoteURL(remoteName string) (string, error) {
	output, err := c.ExecuteCommand("remote", "get-url", remoteName)

	if err != nil {
		return "", fmt.Errorf("failed to get remote URL: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

func (c *GitClient) AddRemote(remoteName string, remoteURL string) (string, error) {
	_, err := c.ExecuteCommand("remote", "add", remoteName, remoteURL)

	if err != nil {
		return "", fmt.Errorf("failed to add remote: %w", err)
	}

	return fmt.Sprintf("Added remote %s: %s", remoteName, remoteURL), nil
}

func (c *GitClient) RemoveRemote(remoteName string) (string, error) {
	_, err := c.ExecuteCommand("remote", "remove", remoteName)

	if err != nil {
		return "", fmt.Errorf("failed to remove remote: %w", err)
	}

	return fmt.Sprintf("Removed remote %s", remoteName), nil
}

// RemoteInfo contains information about remote connection
type RemoteInfo struct {
	Host    string
	BaseDir string
	App     string
}

// GetRemoteInfoWithClient extracts info from git remote using a specific client
// Example: ubuntu@server:api
// Returns: RemoteInfo{Host: "ubuntu@server", BaseDir: "/opt/gokku", App: "api"}
func GetRemoteInfoWithClient(client AbstractGitClient, remoteName string) (*RemoteInfo, error) {
	remoteURL, err := client.GetRemoteURL(remoteName)

	if err != nil {
		return nil, fmt.Errorf("remote '%s' not found: %w", remoteName, err)
	}

	// Parse remote URL: user@host:app
	// Example: ubuntu@server.example.com:api
	parts := strings.Split(remoteURL, ":")

	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid remote URL format: %s", remoteURL)
	}

	host := parts[0]
	app := parts[1]

	return &RemoteInfo{
		Host:    host,
		BaseDir: "/opt/gokku",
		App:     app,
	}, nil
}
