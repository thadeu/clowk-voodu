// Package git wraps the `git` CLI for Voodu-specific operations:
// managing remotes on the client, and bootstrapping bare repos with
// a post-receive hook on the server.
package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.voodu.clowk.in/internal/paths"
)

type Client struct{}

func (c *Client) Exec(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	return cmd.CombinedOutput()
}

func (c *Client) ExecCtx(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.CombinedOutput()
}

func (c *Client) GetRemoteURL(name string) (string, error) {
	out, err := c.Exec("remote", "get-url", name)
	if err != nil {
		return "", fmt.Errorf("get remote url %s: %w", name, err)
	}

	return strings.TrimSpace(string(out)), nil
}

func (c *Client) AddRemote(name, url string) error {
	if _, err := c.Exec("remote", "add", name, url); err != nil {
		return fmt.Errorf("add remote %s: %w", name, err)
	}

	return nil
}

func (c *Client) RemoveRemote(name string) error {
	if _, err := c.Exec("remote", "remove", name); err != nil {
		return fmt.Errorf("remove remote %s: %w", name, err)
	}

	return nil
}

// PushHead pushes the current HEAD to <remote>:refs/heads/<branch>. Used
// by `voodu apply` to ship source for build-mode deployments before
// POSTing manifests — the post-receive hook on the bare repo is what
// turns the push into a built image. Output streams to the caller's
// stdout/stderr so the user sees hook progress live.
func PushHead(ctx context.Context, remote, branch string) error {
	if remote == "" || branch == "" {
		return fmt.Errorf("git push: remote and branch are required")
	}

	refspec := fmt.Sprintf("HEAD:refs/heads/%s", branch)

	cmd := exec.CommandContext(ctx, "git", "push", remote, refspec)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push %s %s: %w", remote, refspec, err)
	}

	return nil
}

// SetupBareRepo initializes the server-side bare repository for an app
// at <root>/repos/<app>.git. Idempotent: re-running on an existing repo
// is a no-op.
func SetupBareRepo(app string) error {
	repoDir := paths.AppRepoDir(app)

	if _, err := os.Stat(filepath.Join(repoDir, "HEAD")); err == nil {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(repoDir), 0755); err != nil {
		return fmt.Errorf("create repos dir: %w", err)
	}

	cmd := exec.Command("git", "init", "--bare", repoDir, "--initial-branch=main")

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git init --bare %s: %w (output: %s)", repoDir, err, string(out))
	}

	return nil
}

// SetupPostReceiveHook writes <repo>/hooks/post-receive that triggers
// `voodu deploy -a <app>` on push. Hook is idempotent — overwrites any
// existing hook.
func SetupPostReceiveHook(app string) error {
	repoDir := paths.AppRepoDir(app)
	hookDir := filepath.Join(repoDir, "hooks")
	hookFile := filepath.Join(hookDir, "post-receive")

	if err := os.MkdirAll(hookDir, 0755); err != nil {
		return fmt.Errorf("create hooks dir: %w", err)
	}

	content := fmt.Sprintf(`#!/bin/bash
set -e

APP_NAME="%s"

echo "-----> Received push for $APP_NAME"

if git rev-parse --verify HEAD >/dev/null 2>&1; then
    CURRENT_HEAD_REF=$(git symbolic-ref HEAD 2>/dev/null || echo "")
    CURRENT_HEAD_BRANCH=$(basename "$CURRENT_HEAD_REF" 2>/dev/null || echo "")

    echo "-----> Deploying from branch: $CURRENT_HEAD_BRANCH"

    voodu deploy -a "$APP_NAME"

    echo "-----> Deployment completed"
else
    echo "-----> Repository is empty, skipping deployment"
    echo "-----> Run 'voodu deploy -a $APP_NAME' manually after your first push"
fi

echo "-----> Done"
`, app)

	if err := os.WriteFile(hookFile, []byte(content), 0755); err != nil {
		return fmt.Errorf("write post-receive hook: %w", err)
	}

	return nil
}
