package controller

import (
	"path/filepath"

	"go.voodu.clowk.in/internal/plugins"
)

// PluginBlockRegistry resolves a plugin-block kind to the on-disk
// plugin that handles it. The /apply pipeline calls this before
// persisting to decide whether a non-core manifest can be expanded
// (kind matches an installed plugin) or must be rejected (no
// plugin → operator typo or missing install).
//
// Convention: block_type == plugin_name. A `postgres { … }` block
// dispatches to the plugin installed under
// `<plugins_root>/postgres/`. Plugins that want to declare
// multiple block types under one binary can override this in a
// future plugin.yml `blocks:` field — kept simple here while the
// 1:1 convention covers postgres / redis / mysql / mongo /
// rabbitmq cleanly.
//
// Behind an interface so /apply tests can stub the lookup without
// touching the filesystem.
type PluginBlockRegistry interface {
	// LookupByBlock returns the plugin that handles the given
	// block type, or (nil, false) when no plugin is installed
	// for it. The plugin must expose an "expand" command — a
	// plugin that ships only operator-facing subcommands (e.g.
	// `vd postgres backup`) without an expand binary is treated
	// as "not registered" for block-expansion purposes.
	LookupByBlock(blockType string) (*plugins.LoadedPlugin, bool)
}

// DirPluginRegistry is the production registry: it resolves
// plugins by directory name under PluginsRoot. Cheap to construct
// (no caching today — plugins are loaded fresh on every /apply,
// which adds ~1ms but keeps the lifecycle simple). Future
// optimisation can add a map cache invalidated on
// install/uninstall events.
type DirPluginRegistry struct {
	// PluginsRoot is typically /opt/voodu/plugins. Plugin
	// directory layout follows the same shape internal/plugins
	// LoadFromDir expects (plugin.yml + bin/<command> or
	// commands/<command>).
	PluginsRoot string
}

// LookupByBlock implements PluginBlockRegistry.
func (r *DirPluginRegistry) LookupByBlock(blockType string) (*plugins.LoadedPlugin, bool) {
	if r.PluginsRoot == "" || blockType == "" {
		return nil, false
	}

	dir := filepath.Join(r.PluginsRoot, blockType)

	loaded, err := plugins.LoadFromDir(dir)
	if err != nil {
		return nil, false
	}

	// "expand" is the contract a macro plugin must honour. If the
	// plugin only ships operator commands (like "backup") it
	// can't be used to materialise blocks, so we treat it as
	// missing for expansion purposes.
	if _, ok := loaded.Commands["expand"]; !ok {
		return nil, false
	}

	return loaded, true
}
