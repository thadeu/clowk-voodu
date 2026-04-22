package controller

import (
	"context"
	"fmt"
	"path/filepath"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// PluginInvoker runs one plugin command. The reconciler uses this to
// materialise desired state into real resources (database containers,
// certificates, etc.) without reaching back through HTTP.
//
// Defined as an interface so handlers can be tested with stubs that
// return canned envelopes — no disk, no fork/exec.
type PluginInvoker interface {
	Invoke(ctx context.Context, pluginName, command string, args []string, env map[string]string) (*plugins.Result, error)
}

// DirInvoker loads plugins from disk and runs them, exactly like the
// /exec HTTP handler. It's the production wiring; tests substitute
// something simpler.
type DirInvoker struct {
	PluginsRoot string
	NodeName    string
	EtcdClient  string
}

// Invoke resolves the plugin dir, loads it, and runs the command. The
// injected env matches what /exec does — reconciler invocations look
// identical to user-triggered invocations from the plugin's perspective.
func (d *DirInvoker) Invoke(ctx context.Context, pluginName, command string, args []string, env map[string]string) (*plugins.Result, error) {
	if d.PluginsRoot == "" {
		return nil, fmt.Errorf("no plugins root configured")
	}

	loaded, err := plugins.LoadFromDir(filepath.Join(d.PluginsRoot, pluginName))
	if err != nil {
		return nil, fmt.Errorf("plugin %q: %w", pluginName, err)
	}

	merged := map[string]string{
		plugin.EnvRoot:       d.PluginsRoot,
		plugin.EnvNode:       d.NodeName,
		plugin.EnvEtcdClient: d.EtcdClient,
	}

	for k, v := range env {
		merged[k] = v
	}

	return loaded.Run(ctx, plugins.RunOptions{
		Command: command,
		Args:    args,
		Env:     merged,
	})
}
