package plugins

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InstallSource describes where a plugin's files come from. The source
// string can be:
//
//   - a local absolute or relative path to a directory,
//   - a git URL (https://…/foo.git or git@host:owner/repo),
//   - a github shorthand (github.com/owner/repo or owner/repo).
//
// Tarballs are intentionally out of scope for M5 — they add a download
// path, checksum story, and archive extraction we don't need yet.
type InstallSource struct {
	Raw  string
	Kind SourceKind
}

type SourceKind string

const (
	SourceLocal SourceKind = "local"
	SourceGit   SourceKind = "git"
)

// ParseSource classifies an install source. The only failure mode is an
// empty string — everything else falls into local or git.
func ParseSource(s string) (InstallSource, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return InstallSource{}, fmt.Errorf("plugin source is empty")
	}

	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
		return InstallSource{Raw: s, Kind: SourceLocal}, nil
	}

	// Bare directory name that exists locally — treat as path.
	if info, err := os.Stat(s); err == nil && info.IsDir() {
		return InstallSource{Raw: s, Kind: SourceLocal}, nil
	}

	return InstallSource{Raw: s, Kind: SourceGit}, nil
}

// Installer materialises plugins on disk under Root and validates them
// by loading. It does not write to etcd — callers (controller) do that
// after Install returns so the registry only ever contains plugins that
// exist and parse.
type Installer struct {
	Root string // typically /opt/voodu/plugins

	// Git is the git binary used to clone remote sources. Defaults to
	// "git" on PATH; tests can inject a stub.
	Git string
}

// Install places the plugin in Root and returns the loaded plugin. If a
// plugin with the same name already exists, it is replaced atomically
// (scratch dir → rename).
//
// `version` pins a specific git tag for SourceGit sources (e.g.
// `"0.2.0"` or `"v0.2.0"` — leading `v` auto-prefixed when missing).
// Empty string clones the default branch (latest). Local sources
// ignore the version argument; pin via the source path itself.
func (i *Installer) Install(ctx context.Context, source, version string) (*LoadedPlugin, error) {
	src, err := ParseSource(source)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(i.Root, 0755); err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp(i.Root, ".install-*")
	if err != nil {
		return nil, err
	}

	cleanup := func() { _ = os.RemoveAll(tmp) }

	if err := i.fetch(ctx, src, tmp, version); err != nil {
		cleanup()
		return nil, err
	}

	if err := makeExecutable(tmp); err != nil {
		cleanup()
		return nil, err
	}

	loaded, err := LoadFromDir(tmp)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("validate plugin: %w", err)
	}

	name := loaded.Manifest.Name
	if name == "" {
		cleanup()
		return nil, fmt.Errorf("plugin has no name (missing plugin.yml or commands/name)")
	}

	final := filepath.Join(i.Root, name)

	if err := os.RemoveAll(final); err != nil {
		cleanup()
		return nil, err
	}

	if err := os.Rename(tmp, final); err != nil {
		cleanup()
		return nil, err
	}

	loaded.Dir = final
	loaded.Manifest.Source = src.Raw

	// Commands were indexed by absolute path under the scratch dir —
	// rewrite them to the final location.
	for k, v := range loaded.Commands {
		rel, _ := filepath.Rel(tmp, v)
		loaded.Commands[k] = filepath.Join(final, rel)
	}

	i.runLifecycle(ctx, loaded, "install")

	return loaded, nil
}

// Remove deletes the plugin directory after running its uninstall hook,
// if any. Missing plugins are a no-op (ok=false).
func (i *Installer) Remove(ctx context.Context, name string) (bool, error) {
	dir := filepath.Join(i.Root, name)

	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	if !info.IsDir() {
		return false, fmt.Errorf("%s is not a plugin directory", dir)
	}

	if p, err := LoadFromDir(dir); err == nil {
		i.runLifecycle(ctx, p, "uninstall")
	}

	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}

	return true, nil
}

// fetch materialises src into dst. For local sources it copies the tree
// (so the original stays pristine); for git it shells out to `git clone`.
// When `version` is non-empty, the git path adds `--branch v<version>`
// (auto-prefixing `v` when the operator omits it) so JIT installs pin
// to specific tags. Local sources ignore version — pin via path.
func (i *Installer) fetch(ctx context.Context, src InstallSource, dst, version string) error {
	switch src.Kind {
	case SourceLocal:
		return copyTree(src.Raw, dst)

	case SourceGit:
		git := i.Git
		if git == "" {
			git = "git"
		}

		args := []string{"clone", "--depth=1"}

		if version != "" {
			tag := version
			if !strings.HasPrefix(tag, "v") {
				tag = "v" + tag
			}

			args = append(args, "--branch", tag)
		}

		args = append(args, normaliseGitURL(src.Raw), dst)

		cmd := exec.CommandContext(ctx, git, args...)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			if version != "" {
				return fmt.Errorf("git clone %s @ v%s: %w (tag may not exist)", src.Raw, version, err)
			}

			return fmt.Errorf("git clone %s: %w", src.Raw, err)
		}

		// Drop .git to keep installed plugins small and immutable.
		_ = os.RemoveAll(filepath.Join(dst, ".git"))

		return nil
	}

	return fmt.Errorf("unknown source kind %q", src.Kind)
}

// normaliseGitURL turns shorthand forms into real clone URLs.
//
//	owner/repo                 → https://github.com/owner/repo
//	github.com/owner/repo      → https://github.com/owner/repo
//	https://host/owner/repo    → unchanged
//	git@host:owner/repo        → unchanged (ssh clone)
func normaliseGitURL(s string) string {
	if strings.Contains(s, "://") || strings.HasPrefix(s, "git@") {
		return s
	}

	if strings.HasPrefix(s, "github.com/") {
		return "https://" + s
	}

	// owner/repo shorthand (exactly 2 segments, both non-empty).
	if parts := strings.Split(s, "/"); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		// Validate against URL.Parse so we don't turn garbage into an https scheme.
		if _, err := url.Parse("https://github.com/" + s); err == nil {
			return "https://github.com/" + s
		}
	}

	return s
}

// runLifecycle runs `install` or `uninstall` in the plugin dir if
// that script exists. Failures are logged to the caller's stderr
// but do not abort the operation — lifecycle scripts are advisory,
// the plugin's commands/* are the contract.
func (i *Installer) runLifecycle(ctx context.Context, p *LoadedPlugin, name string) {
	path := filepath.Join(p.Dir, name)

	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return
	}

	cmd := exec.CommandContext(ctx, path, p.Manifest.Name)
	cmd.Dir = p.Dir
	cmd.Env = buildEnv(p, nil)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	_ = cmd.Run()
}

// makeExecutable sets 0755 on every file under commands/ and bin/.
// Defensive: shell scripts in plugin repos can lose the +x bit
// after `git clone` (Windows checkouts, archive extraction with
// strict umask, etc.) and the plugin would silently fail with
// "permission denied" on first invocation.
func makeExecutable(root string) error {
	for _, sub := range []string{"commands", "bin"} {
		dir := filepath.Join(root, sub)

		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}

		if err != nil {
			return err
		}

		for _, e := range entries {
			if e.IsDir() {
				continue
			}

			if err := os.Chmod(filepath.Join(dir, e.Name()), 0755); err != nil {
				return err
			}
		}
	}

	for _, hook := range []string{"install", "uninstall"} {
		p := filepath.Join(root, hook)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			_ = os.Chmod(p, 0755)
		}
	}

	return nil
}

// copyTree does a shallow-ish recursive copy. It preserves mode bits so
// executables stay executable after copy. Symlinks are dereferenced —
// we want the plugin directory to be self-contained.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}

		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := os.Readlink(path)
			if err != nil {
				return err
			}

			// Dereference by copying the pointed-to file.
			return copyFile(filepath.Join(filepath.Dir(path), resolved), target)
		}

		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, in, info.Mode())
}
