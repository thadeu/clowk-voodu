package controller

import (
	"context"
	"encoding/json"
	"time"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// defaultDrainTimeout bounds how long a roll waits for one replica's
// work to finish before removing it anyway.
//
// Two minutes is long enough for a request-shaped workload to finish
// and short enough that a stuck socket does not look like a hung
// deploy. Workloads whose unit of work is measured in minutes — a call,
// a long upload — should raise it explicitly rather than have the
// platform guess on their behalf.
var defaultDrainTimeout = 2 * time.Minute

// pluginDrainRequest is what a plugin's `drain` receives before the
// controller removes a replica.
//
// The plugin is expected to BLOCK until the replica is finished or the
// timeout expires. Making it blocking rather than event-driven keeps
// the contract to one subprocess with an exit code — the alternative is
// a channel between the controller and a plugin process, which is a lot
// of machinery for "tell me when you are done".
type pluginDrainRequest struct {
	Kind      string            `json:"kind"`
	Scope     string            `json:"scope,omitempty"`
	Name      string            `json:"name"`
	Block     nestedPluginBlock `json:"block"`
	ReplicaID string            `json:"replica_id"`
	TimeoutMS int64             `json:"timeout_ms"`
}

// replicaDrainer takes a replica out of service before it is removed.
//
// This is the half of the rollout that no plugin can do for itself: a
// plugin cannot know a removal is coming, and by the time it notices
// the container is gone the connections are already cut. The controller
// is the only place that knows in advance.
type replicaDrainer struct {
	Registry PluginBlockRegistry

	logf func(string, ...any)
}

// drain asks every plugin holding a block on this workload to stop
// sending work to one replica, and waits for it to go quiet.
//
// It never fails the roll. A drain that could wedge a deployment would
// be worse than the connection cut it prevents: one socket whose peer
// vanished without a FIN never closes, and a deployment that cannot
// roll is an outage of its own. Timeouts and plugin errors are logged
// and the removal proceeds.
func (d *replicaDrainer) drain(ctx context.Context, kind Kind, scope, name string, spec json.RawMessage, replicaID string, timeout time.Duration) error {
	if d == nil || d.Registry == nil || len(spec) == 0 {
		return nil
	}

	var carrier nestedBlockCarrier

	if err := json.Unmarshal(spec, &carrier); err != nil || len(carrier.PluginBlocks) == 0 {
		return nil
	}

	if timeout <= 0 {
		timeout = defaultDrainTimeout
	}

	for _, blk := range carrier.PluginBlocks {
		plug, ok := d.Registry.LookupByBlock(blk.Type)

		if !ok {
			// The plugin can be removed between the apply that
			// declared the block and the roll that would drain it.
			d.logf("%s/%s/%s replica %s: block %q has no installed plugin, removing without drain",
				kind, scope, name, replicaID, blk.Type)

			continue
		}

		d.drainOne(ctx, plug, kind, scope, name, blk, replicaID, timeout)
	}

	return nil
}

func (d *replicaDrainer) drainOne(ctx context.Context, plug *plugins.LoadedPlugin, kind Kind, scope, name string, blk nestedPluginBlock, replicaID string, timeout time.Duration) {
	stdin, err := json.Marshal(pluginDrainRequest{
		Kind:      string(kind),
		Scope:     scope,
		Name:      name,
		Block:     blk,
		ReplicaID: replicaID,
		TimeoutMS: timeout.Milliseconds(),
	})

	if err != nil {
		d.logf("%s/%s/%s replica %s: marshal drain request: %v", kind, scope, name, replicaID, err)

		return
	}

	// The plugin is told the budget and the controller enforces it too.
	// A plugin that ignores its own timeout must not be able to hold a
	// deployment open.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := plug.Run(runCtx, plugins.RunOptions{
		Command: "drain",
		Stdin:   stdin,
		Env:     map[string]string{plugin.EnvRoot: ""},
	})

	switch {
	case err != nil:
		d.logf("%s/%s/%s replica %s: %s drain did not finish in %s — removing anyway, connections still open on it will be cut",
			kind, scope, name, replicaID, blk.Type, timeout)

	case res.Envelope != nil && res.Envelope.Status == "error":
		d.logf("%s/%s/%s replica %s: %s drain reported %q — removing anyway",
			kind, scope, name, replicaID, blk.Type, res.Envelope.Error)

	case res.ExitCode != 0:
		d.logf("%s/%s/%s replica %s: %s drain exited %d — removing anyway",
			kind, scope, name, replicaID, blk.Type, res.ExitCode)

	default:
		d.logf("%s/%s/%s replica %s: drained by %s", kind, scope, name, replicaID, blk.Type)
	}
}
