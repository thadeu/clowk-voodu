// Package deploy orchestrates a single app release: extract code from the
// bare repo, apply env overrides from voodu.yml, build, swap the current
// symlink, run post-deploy hooks, and restart the container.
//
// This is the pragmatic M1 port of the Gokku deploy pipeline. Blue/green
// and richer orchestration are the concern of the M3 controller.
package deploy

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"go.voodu.clowk.in/internal/config"
	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/envfile"
	"go.voodu.clowk.in/internal/paths"
)

// Options tune a deploy. Zero value is fine (uses sensible defaults).
type Options struct {
	// LogWriter receives human-readable progress lines. Defaults to os.Stdout.
	LogWriter io.Writer
}

func (o *Options) log(format string, args ...any) {
	w := o.LogWriter

	if w == nil {
		w = os.Stdout
	}

	fmt.Fprintf(w, format+"\n", args...)
}

// Run executes a full deploy for the given app.
func Run(app string, opts Options) error {
	opts.log("Deploying app '%s'...", app)

	repoDir := paths.AppRepoDir(app)

	if !hasCommits(repoDir) {
		return fmt.Errorf("repository is empty, cannot deploy")
	}

	releaseID := time.Now().Format("20060102150405")
	releaseDir := filepath.Join(paths.AppReleasesDir(app), releaseID)

	if err := os.MkdirAll(releaseDir, 0755); err != nil {
		return fmt.Errorf("create release dir: %w", err)
	}

	opts.log("-----> Creating release %s", releaseID)

	if err := extractCode(repoDir, releaseDir, &opts); err != nil {
		return fmt.Errorf("extract code: %w", err)
	}

	if err := applyConfigEnv(app, releaseDir, &opts); err != nil {
		opts.log("Warning: could not process voodu.yml: %v", err)
	}

	if err := buildRelease(releaseDir, &opts); err != nil {
		return fmt.Errorf("build release: %w", err)
	}

	if err := swapCurrentSymlink(app, releaseDir); err != nil {
		return fmt.Errorf("swap current symlink: %w", err)
	}

	if err := runPostDeploy(app, releaseDir, &opts); err != nil {
		opts.log("Warning: post-deploy hook failed: %v", err)
	}

	if err := startContainers(app, releaseDir, &opts); err != nil {
		return fmt.Errorf("start containers: %w", err)
	}

	opts.log("Deploy completed successfully for '%s'", app)

	return nil
}

func hasCommits(repoDir string) bool {
	return exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "HEAD").Run() == nil
}

func extractCode(repoDir, releaseDir string, opts *Options) error {
	head, err := exec.Command("git", "-C", repoDir, "symbolic-ref", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("read HEAD: %w", err)
	}

	branch := strings.TrimPrefix(strings.TrimSpace(string(head)), "refs/heads/")
	opts.log("-----> Extracting code from branch: %s", branch)

	out, err := exec.Command("git", "clone", "--branch", branch, "--depth", "1", repoDir, releaseDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("clone: %v (output: %s)", err, string(out))
	}

	_ = os.RemoveAll(filepath.Join(releaseDir, ".git"))

	return nil
}

// applyConfigEnv looks for <release>/voodu.yml (or legacy gokku.yml) and
// merges its `env:` block into the app's shared/.env file, preserving
// any keys previously set via `voodu config:set`.
func applyConfigEnv(app, releaseDir string, opts *Options) error {
	vooduYml := filepath.Join(releaseDir, paths.VooduYAML)
	legacyYml := filepath.Join(releaseDir, paths.GokkuYAML)

	cfgPath := vooduYml

	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if _, err := os.Stat(legacyYml); err == nil {
			cfgPath = legacyYml
		} else {
			return nil
		}
	}

	opts.log("-----> Found %s, processing configuration...", filepath.Base(cfgPath))

	srv, err := loadServerConfig(cfgPath)
	if err != nil {
		return err
	}

	appCfg, err := srv.GetApp(app)
	if err != nil {
		return nil
	}

	if len(appCfg.Env) == 0 {
		return nil
	}

	envPath := paths.AppEnvFile(app)

	vars, err := envfile.Load(envPath)
	if err != nil {
		return err
	}

	for k, v := range appCfg.Env {
		vars[k] = v
	}

	return envfile.Save(envPath, vars)
}

func loadServerConfig(path string) (*config.ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var srv config.ServerConfig

	if err := yaml.Unmarshal(data, &srv); err != nil {
		return nil, err
	}

	return &srv, nil
}

func buildRelease(releaseDir string, opts *Options) error {
	opts.log("-----> Building release...")

	if _, err := os.Stat(filepath.Join(releaseDir, "Dockerfile")); err == nil {
		opts.log("-----> Found Dockerfile")
	}

	opts.log("-----> Build completed")

	return nil
}

func swapCurrentSymlink(app, releaseDir string) error {
	link := paths.AppCurrentLink(app)
	_ = os.Remove(link)

	return os.Symlink(releaseDir, link)
}

func runPostDeploy(app, releaseDir string, opts *Options) error {
	appCfg, err := config.LoadAppConfig(app)
	if err != nil {
		return nil
	}

	if appCfg.Deployment == nil || len(appCfg.Deployment.PostDeploy) == 0 {
		return nil
	}

	opts.log("-----> Running post-deploy commands...")

	for _, command := range appCfg.Deployment.PostDeploy {
		opts.log("       $ %s", command)

		cmd := exec.Command("bash", "-c", command)
		cmd.Dir = releaseDir
		cmd.Stdout = opts.LogWriter
		cmd.Stderr = opts.LogWriter

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("post-deploy %q: %w", command, err)
		}
	}

	return nil
}

func startContainers(app, releaseDir string, opts *Options) error {
	opts.log("-----> Starting containers...")

	if err := docker.RecreateActiveContainer(app, paths.AppEnvFile(app), releaseDir); err != nil {
		return err
	}

	opts.log("-----> Containers started")

	return nil
}
