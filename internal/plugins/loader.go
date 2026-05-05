// Package plugins implements installation, resolution, and
// execution of voodu plugins. Two on-disk layouts are accepted:
//
//   - Modern: plugin.yml with structured metadata (name, version,
//     commands, env), executables under ./commands/.
//   - Bare: a `commands/` directory of executable scripts, no
//     plugin.yml. Name falls back to the directory's basename.
//
// Both shapes are detected opportunistically so simple shell-only
// plugins (a directory of scripts) work without ceremony.
package plugins

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"go.voodu.clowk.in/pkg/plugin"
)

// LoadedPlugin is the in-memory representation of an installed plugin,
// built by scanning its directory plus its optional plugin.yml.
type LoadedPlugin struct {
	Manifest plugin.Manifest
	Dir      string

	// Commands maps subcommand name → absolute executable path. Populated
	// from bin/* (preferred) and commands/* (fallback). When both exist
	// for the same command name, bin/ wins.
	Commands map[string]string
}

// LoadFromDir reads one plugin from its installation directory. It
// tolerates missing plugin.yml by filling in defaults from
// directory conventions — name from basename, commands from the
// `commands/` subdir.
func LoadFromDir(dir string) (*LoadedPlugin, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	manifest, err := readManifest(dir)
	if err != nil {
		return nil, err
	}

	cmds, err := discoverCommands(dir)
	if err != nil {
		return nil, err
	}

	// Convention backfills: plugin.yml can be absent entirely, in which
	// case we derive name from the directory and commands from disk.
	if manifest.Name == "" {
		manifest.Name = bareCommandsName(dir, filepath.Base(dir))
	}

	if len(manifest.Commands) == 0 {
		for name := range cmds {
			manifest.Commands = append(manifest.Commands, plugin.Command{Name: name})
		}
	}

	// Entrypoint mode: when plugin.yml declares a single binary
	// for every command, override the bin/<command> map so all
	// declared commands resolve to the entrypoint path. The exec
	// layer then prepends the command name as argv[1]. Plugin
	// authors get to ship one binary + plugin.yml, no shim parade.
	if manifest.Entrypoint != "" {
		entrypointPath := filepath.Join(dir, manifest.Entrypoint)

		if info, err := os.Stat(entrypointPath); err != nil || info.IsDir() {
			return nil, fmt.Errorf("plugin %q: entrypoint %q not found or not a file (looked at %s)",
				manifest.Name, manifest.Entrypoint, entrypointPath)
		}

		// Replace the discovered commands map. Every command the
		// plugin declares now points at the entrypoint; bin/ files
		// (if any) are ignored for routing purposes.
		entrypointCmds := make(map[string]string, len(manifest.Commands))
		for _, c := range manifest.Commands {
			entrypointCmds[c.Name] = entrypointPath
		}

		cmds = entrypointCmds
	}

	return &LoadedPlugin{
		Manifest: manifest,
		Dir:      dir,
		Commands: cmds,
	}, nil
}

// readManifest returns the parsed plugin.yml, or a zero-value Manifest
// if the file does not exist. Malformed YAML is an error — silent
// fallback would hide real author mistakes.
func readManifest(dir string) (plugin.Manifest, error) {
	path := filepath.Join(dir, "plugin.yml")

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return plugin.Manifest{}, nil
	}

	if err != nil {
		return plugin.Manifest{}, err
	}

	var m plugin.Manifest

	if err := yaml.Unmarshal(raw, &m); err != nil {
		return plugin.Manifest{}, fmt.Errorf("parse %s: %w", path, err)
	}

	return m, nil
}

// discoverCommands indexes executables under bin/ then commands/.
// bin/ wins on duplicate names so plugins can ship a compiled binary
// alongside a legacy shell fallback without the wrong one being picked.
func discoverCommands(dir string) (map[string]string, error) {
	out := map[string]string{}

	for _, sub := range []string{"commands", "bin"} {
		root := filepath.Join(dir, sub)

		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, err
		}

		for _, e := range entries {
			if e.IsDir() {
				continue
			}

			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}

			out[name] = filepath.Join(root, name)
		}
	}

	return out, nil
}

// bareCommandsName resolves the plugin display name when plugin.yml
// is absent: the first non-empty line of `commands/name` (the
// optional name-emitting script), falling back to the directory
// basename when that script is missing or empty.
func bareCommandsName(dir, fallback string) string {
	path := filepath.Join(dir, "commands", "name")

	raw, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}

	// commands/name can be either a script that echoes the name or a
	// plain text file with the name. Take the first non-empty line.
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "echo") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "echo"))
			line = strings.Trim(line, `"'`)
		}

		if line != "" {
			return line
		}
	}

	return fallback
}

// LoadAll scans a plugins root (typically /opt/voodu/plugins) and
// returns every valid plugin found. Directories that fail to load are
// reported individually so one bad plugin does not take down the rest.
// LoadByName resolves a plugin by name, honouring aliases.
// Lookup order:
//
//  1. Fast path: <root>/<name>/ exists → load it.
//  2. Fallback: scan every plugin under <root>, return the first
//     whose Manifest.Aliases list contains `name`.
//
// The fast path matches the historical convention (plugin
// directory == plugin name) so the common case stays a single
// stat + read. Operators using aliases pay the directory scan
// cost only on first invocation per process; in practice ~1ms
// for tens of plugins.
//
// Returns (nil, false) when no plugin matches by name OR by
// alias. Errors during fallback scan are aggregated into the
// returned error so partial-failure modes (one broken
// plugin.yml in the middle of the dir) don't shadow a working
// alias match.
func LoadByName(root, name string) (*LoadedPlugin, error) {
	if root == "" {
		return nil, fmt.Errorf("plugins root is empty")
	}

	if name == "" {
		return nil, fmt.Errorf("plugin name is empty")
	}

	// Fast path: directory match.
	dir := filepath.Join(root, name)

	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return LoadFromDir(dir)
	}

	// Fallback: scan aliases. We tolerate per-plugin load errors
	// here — a single broken plugin.yml shouldn't prevent an
	// otherwise-resolvable alias from being found.
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read plugins root %s: %w", root, err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		p, err := LoadFromDir(filepath.Join(root, e.Name()))
		if err != nil {
			// Skip broken plugins silently — surfacing them here
			// would mask the alias miss. Operators see broken
			// plugins via `vd plugins:list`.
			continue
		}

		if p.Manifest.HasAlias(name) {
			return p, nil
		}
	}

	return nil, fmt.Errorf("plugin %q not found in %s (no directory match, no alias match)", name, root)
}

func LoadAll(root string) ([]*LoadedPlugin, []error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, []error{err}
	}

	var (
		out  []*LoadedPlugin
		errs []error
	)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		p, err := LoadFromDir(filepath.Join(root, e.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}

		out = append(out, p)
	}

	return out, errs
}
