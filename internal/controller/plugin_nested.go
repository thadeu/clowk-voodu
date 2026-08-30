package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// nestedPluginBlock mirrors manifest.PluginBlock on the wire. The
// controller cannot import the manifest package (manifest imports this
// one), so the shape is restated rather than shared — it is three
// fields and a JSON contract, and the alternative is an import cycle.
type nestedPluginBlock struct {
	Type   string          `json:"type"`
	Labels []string        `json:"labels,omitempty"`
	Spec   json.RawMessage `json:"spec,omitempty"`
}

// nestedBlockCarrier is the sliver of a core spec this pass reads and
// writes. Decoding into it rather than the full DeploymentSpec keeps
// every other field untouched — the spec is re-encoded from the
// original bytes, so a field this controller version has never heard
// of survives a round trip.
type nestedBlockCarrier struct {
	PluginBlocks []nestedPluginBlock `json:"plugin_blocks"`
}

// expandNestedPluginBlocks hands every unrecognised block on a core
// manifest to the plugin that owns it, and keeps what comes back.
//
// The plugin's job here is to validate and normalise its own block, NOT
// to emit structure. That restriction is deliberate: the moment a
// plugin can inject arbitrary fields into a deployment's spec, two
// plugins can write the same field and there is no one to arbitrate.
// Validation has an obvious owner; merging does not.
//
// The payoff is where errors land. A misspelled or misconfigured block
// used to fail at runtime, inside a container, hours after the apply
// that introduced it. Now it fails at apply, with the plugin's own
// message.
func (a *API) expandNestedPluginBlocks(ctx context.Context, m *Manifest) error {
	if m == nil || len(m.Spec) == 0 {
		return nil
	}

	var carrier nestedBlockCarrier

	if err := json.Unmarshal(m.Spec, &carrier); err != nil {
		// A spec this pass cannot read is not this pass's problem —
		// the kind's own decoder will report it with better context.
		return nil
	}

	if len(carrier.PluginBlocks) == 0 {
		return nil
	}

	ref := fmt.Sprintf("%s/%s/%s", m.Kind, m.Scope, m.Name)

	for i := range carrier.PluginBlocks {
		blk := &carrier.PluginBlocks[i]

		plug, ok := a.lookupBlockPlugin(blk.Type)

		if !ok {
			// This is where a typo surfaces now that the parser
			// carries unknown blocks instead of refusing them. Name
			// the block, where it is, and the way out — the parser's
			// old "block not expected here" said none of that.
			return fmt.Errorf(
				"%s: block %q belongs to a plugin named %q, which is not installed — run `vd plugins:install <source>` or remove the block",
				ref, blk.Type, blk.Type)
		}

		normalized, err := a.runNestedExpand(ctx, plug, m, *blk)

		if err != nil {
			return fmt.Errorf("%s: %s block: %w", ref, blk.Type, err)
		}

		if len(normalized) > 0 {
			blk.Spec = normalized
		}
	}

	return spliceNestedBlocks(m, carrier.PluginBlocks)
}

// parentHostPorts classifies the parent's host ports for the plugin.
//
// Only the ports field is decoded: the rest of the spec belongs to the
// kind's own handler, and a decode failure here should cost the plugin
// a missing hint, not the apply.
func parentHostPorts(spec json.RawMessage) string {
	var carrier struct {
		Ports []string `json:"ports"`
	}

	if err := json.Unmarshal(spec, &carrier); err != nil {
		return ""
	}

	return hostPortMode(deploymentSpec{Ports: carrier.Ports})
}

// lookupBlockPlugin resolves a block type to its plugin, tolerating an
// unconfigured registry so handlers built for validation only do not
// panic.
func (a *API) lookupBlockPlugin(blockType string) (*plugins.LoadedPlugin, bool) {
	if a.PluginBlocks == nil {
		return nil, false
	}

	return a.PluginBlocks.LookupByBlock(blockType)
}

// runNestedExpand invokes one plugin's expand in nested position and
// returns the normalised block spec it answered with.
func (a *API) runNestedExpand(ctx context.Context, plug *plugins.LoadedPlugin, parent *Manifest, blk nestedPluginBlock) (json.RawMessage, error) {
	req := expandRequest{
		Kind:     blk.Type,
		Scope:    parent.Scope,
		Name:     parent.Name,
		Spec:     blk.Spec,
		Position: expandPositionNested,
		Parent: &expandParent{
			Kind:      string(parent.Kind),
			Scope:     parent.Scope,
			Name:      parent.Name,
			Spec:      parent.Spec,
			HostPorts: parentHostPorts(parent.Spec),
		},
	}

	// The parent's config bucket, same as a top-level expand gets, so a
	// nested block can be idempotent about state it generated before.
	if a.Store != nil {
		req.Config, _ = a.Store.ResolveConfig(ctx, parent.Scope, parent.Name)
	}

	stdin, err := json.Marshal(req)

	if err != nil {
		return nil, fmt.Errorf("marshal expand request: %w", err)
	}

	res, err := plug.Run(ctx, plugins.RunOptions{
		Command: "expand",
		Stdin:   stdin,
		Env: map[string]string{
			plugin.EnvRoot:       a.PluginsRoot,
			plugin.EnvNode:       a.NodeName,
			plugin.EnvEtcdClient: a.EtcdClient,
		},
	})

	if err != nil {
		return nil, err
	}

	// A plugin that refuses its own config must fail the apply. Its
	// message is the useful part, so it is carried verbatim rather
	// than replaced with an exit code.
	if res.Envelope != nil && res.Envelope.Status == "error" {
		return nil, fmt.Errorf("%s", res.Envelope.Error)
	}

	if res.ExitCode != 0 {
		return nil, fmt.Errorf("plugin exited %d: %s", res.ExitCode, pluginErrorDetail(res))
	}

	if res.Envelope == nil {
		return nil, fmt.Errorf("returned no JSON envelope (got: %s)", truncateForLog(res.Raw, 200))
	}

	if res.Envelope.Data == nil {
		return nil, nil
	}

	out, err := json.Marshal(res.Envelope.Data)

	if err != nil {
		return nil, fmt.Errorf("re-encode normalised block: %w", err)
	}

	return out, nil
}

// spliceNestedBlocks writes the normalised blocks back into the spec,
// leaving every other field exactly as it arrived.
//
// Decode-modify-reencode of the whole spec would silently drop any
// field this controller does not know about, which is precisely the
// failure mode a rolling upgrade produces. Editing the one key keeps
// the rest byte-for-byte.
func spliceNestedBlocks(m *Manifest, blocks []nestedPluginBlock) error {
	var raw map[string]json.RawMessage

	if err := json.Unmarshal(m.Spec, &raw); err != nil {
		return fmt.Errorf("re-encode spec: %w", err)
	}

	encoded, err := json.Marshal(blocks)

	if err != nil {
		return fmt.Errorf("encode plugin blocks: %w", err)
	}

	raw["plugin_blocks"] = encoded

	out, err := json.Marshal(raw)

	if err != nil {
		return fmt.Errorf("re-encode spec: %w", err)
	}

	m.Spec = out

	return nil
}
