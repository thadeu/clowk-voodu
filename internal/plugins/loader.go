// Package plugins implements installation, resolution, and execution of
// Voodu plugins. The on-disk layout and execution contract match Gokku —
// existing Gokku plugins run unchanged — with two additive extras:
//
//   - Optional plugin.yml for structured metadata (Gokku had none)
//   - Optional JSON envelope for stdout (Gokku plugins printed plain text)
//
// Both extras are detected opportunistically so plain Gokku plugins
// work without modification.
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
// tolerates missing plugin.yml by filling in defaults from directory
// conventions — which is how Gokku plugins are loaded.
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
		manifest.Name = gokkuCommandsName(dir, filepath.Base(dir))
	}

	if len(manifest.Commands) == 0 {
		for name := range cmds {
			manifest.Commands = append(manifest.Commands, plugin.Command{Name: name})
		}
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

// gokkuCommandsName implements the Gokku fallback where the plugin's
// display name is the first line of commands/name. If that script is
// missing or errors, fall back to the directory basename.
func gokkuCommandsName(dir, fallback string) string {
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
