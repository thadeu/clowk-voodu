package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// addressingDNS asks for container names instead of addresses.
//
// A consumer on voodu0 should prefer it: docker's embedded DNS resolves
// the name, and the name survives the container being recreated at a
// different address, so it stays correct between reconciles. A consumer
// on the host network cannot resolve anything docker knows, which is
// why the default is an address.
const addressingDNS = "dns"

// replicaEndpoint is one live replica, as a plugin sees it.
//
// ReplicaID travels alongside the address because that is the identity
// a drain is keyed by. An address alone cannot answer "is the backend I
// asked you to drain the one that just went quiet" — addresses get
// reused, replica ids do not.
type replicaEndpoint struct {
	ReplicaID string `json:"replica_id"`
	Address   string `json:"address"`
}

// pluginApplyRequest is what a plugin's `apply` receives when the
// workload carrying its block has been reconciled.
type pluginApplyRequest struct {
	Kind      string            `json:"kind"`
	Scope     string            `json:"scope,omitempty"`
	Name      string            `json:"name"`
	Block     nestedPluginBlock `json:"block"`
	Endpoints []replicaEndpoint `json:"endpoints"`
}

// endpointPublisher tells a plugin which replicas of a workload are
// currently up.
//
// This is the half that no amount of manifest parsing can supply. The
// live set changes on every roll, every scale, every crash — nothing
// the operator wrote describes it, and the plugin has no way to observe
// it. The controller is the only place where the desired state and the
// runtime meet, so it is the only place this can come from.
type endpointPublisher struct {
	Registry   PluginBlockRegistry
	Containers ContainerManager

	logf func(string, ...any)
}

// publish resolves the live replicas and hands them to every plugin
// that claimed a block on this workload.
func (p *endpointPublisher) publish(ctx context.Context, kind Kind, scope, name string, spec json.RawMessage, live []ContainerSlot) error {
	if p == nil || p.Registry == nil || len(spec) == 0 {
		return nil
	}

	var carrier nestedBlockCarrier

	if err := json.Unmarshal(spec, &carrier); err != nil || len(carrier.PluginBlocks) == 0 {
		return nil
	}

	for _, blk := range carrier.PluginBlocks {
		plug, ok := p.Registry.LookupByBlock(blk.Type)

		if !ok {
			// Apply already refused a block with no plugin behind it.
			// Reaching here means the plugin was removed after the
			// fact — worth saying, not worth failing the reconcile of
			// a workload that is otherwise healthy.
			p.logf("%s/%s/%s: block %q has no installed plugin, skipping endpoint publish", kind, scope, name, blk.Type)

			continue
		}

		endpoints, err := p.endpointsFor(blk, live)

		if err != nil {
			return fmt.Errorf("%s/%s/%s: %s endpoints: %w", kind, scope, name, blk.Type, err)
		}

		if err := p.callApply(ctx, plug, kind, scope, name, blk, endpoints); err != nil {
			return fmt.Errorf("%s/%s/%s: %s apply: %w", kind, scope, name, blk.Type, err)
		}
	}

	return nil
}

// endpointsFor turns the live slots into addresses the block's owner
// can dial.
func (p *endpointPublisher) endpointsFor(blk nestedPluginBlock, live []ContainerSlot) ([]replicaEndpoint, error) {
	var cfg struct {
		Port       int    `json:"port"`
		Addressing string `json:"addressing"`
	}

	if len(blk.Spec) > 0 {
		if err := json.Unmarshal(blk.Spec, &cfg); err != nil {
			return nil, fmt.Errorf("read block config: %w", err)
		}
	}

	out := make([]replicaEndpoint, 0, len(live))

	for _, s := range live {
		// A container that exists but is not running is an address
		// with nothing behind it.
		if !s.Running {
			continue
		}

		host := s.Name

		if cfg.Addressing != addressingDNS {
			ip, err := p.Containers.IP(s.Name)

			if err != nil {
				return nil, fmt.Errorf("replica %s: %w", s.Identity.ReplicaID, err)
			}

			host = ip
		}

		addr := host

		if cfg.Port > 0 {
			addr = fmt.Sprintf("%s:%d", host, cfg.Port)
		}

		out = append(out, replicaEndpoint{ReplicaID: s.Identity.ReplicaID, Address: addr})
	}

	return out, nil
}

func (p *endpointPublisher) callApply(ctx context.Context, plug *plugins.LoadedPlugin, kind Kind, scope, name string, blk nestedPluginBlock, endpoints []replicaEndpoint) error {
	stdin, err := json.Marshal(pluginApplyRequest{
		Kind:      string(kind),
		Scope:     scope,
		Name:      name,
		Block:     blk,
		Endpoints: endpoints,
	})

	if err != nil {
		return fmt.Errorf("marshal apply request: %w", err)
	}

	res, err := plug.Run(ctx, plugins.RunOptions{
		Command: "apply",
		Stdin:   stdin,
		Env:     map[string]string{plugin.EnvRoot: ""},
	})

	if err != nil {
		return err
	}

	if res.Envelope != nil && res.Envelope.Status == "error" {
		return fmt.Errorf("%s", res.Envelope.Error)
	}

	if res.ExitCode != 0 {
		return fmt.Errorf("plugin exited %d: %s", res.ExitCode, pluginErrorDetail(res))
	}

	return nil
}
