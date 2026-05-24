// autoscaler.go is the M7 CPU-driven horizontal scaler for
// `deployment` resources that declare an `autoscale { }` block.
//
// The loop is deliberately small: every tick, walk every deployment
// with an autoscale block, ask the shared StatsCollector for live
// CPU%, decide up/down/hold against a hysteresis band, and call back
// into the store to write the new replica count. The reconciler's
// existing watch on /desired/deployments picks up the change and
// the standard ensureReplicaCount path spawns or stops pods —
// no second control plane, no separate runtime; the autoscaler is
// just a control loop that nudges spec.Replicas.
//
// Two design decisions worth flagging:
//
//  1. Hysteresis bands (target * 0.7 .. target * 1.1) widen the
//     "hold" zone so steady-state noise around the target doesn't
//     thrash. With a symmetric band, a workload running at exactly
//     target% would flap up/down on every measurement jitter.
//
//  2. Cooldowns are asymmetric (30s up, 5m down). Scale-up is the
//     cheap direction — spawning a pod under load is what protects
//     latency, so the cap is short. Scale-down causes 503s if it
//     races a traffic burst, so it's intentionally conservative.
//     Operators tune via cooldown_up / cooldown_down in the HCL.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// Default scaler tunings. Operators override per-deployment in HCL
// (cooldown_up = "...", cooldown_down = "..."); the global Tick
// only adjusts how often we evaluate, not the per-deployment
// cooldowns.
const (
	defaultAutoscalerTick = 15 * time.Second
	defaultCooldownUp     = 30 * time.Second
	defaultCooldownDown   = 5 * time.Minute

	// scaleUpFactor / scaleDownFactor define the hysteresis bands
	// around cpu_target. > target * 1.1 → scale up; < target * 0.7
	// → scale down. The asymmetric gap (10% above vs 30% below)
	// matches the operational intuition that being above target is
	// urgent (latency at risk) and being below target is fine
	// (resources are cheap).
	scaleUpFactor   = 1.1
	scaleDownFactor = 0.7
)

// AutoscaleApplier writes the new desired replica count for a
// deployment. The production impl (StoreReplicasApplier) reads the
// stored manifest, mutates spec.replicas, and re-Puts — which fires
// the standard watch event and the reconciler picks the change up.
//
// The interface exists for tests: a fake records the (scope, name,
// replicas) tuples it was asked to apply without touching the store.
type AutoscaleApplier interface {
	// SetReplicas writes the new replica count to the manifest
	// store. Implementations should be idempotent against the
	// same (scope, name, replicas) call — the autoscaler may
	// retry on transient errors.
	SetReplicas(ctx context.Context, scope, name string, replicas int) error
}

// Autoscaler runs a periodic decision loop on deployments that
// declare an `autoscale { }` block. Lives on its own goroutine
// (started by server.go) and shuts down with the controller's
// context. Stateless across restarts: cooldown timers are per-
// process and reset on controller bounce, which is conservative
// (immediate re-evaluation after restart, no held cooldowns).
//
// Wiring:
//
//   - Store: the manifest source. List+Get pull deployments; the
//     Applier puts back the mutated spec.
//   - Stats: the same StatsCollector that powers `vd stats`, so
//     scaler decisions use the exact runtime numbers operators see.
//   - Apply: indirection layer for tests; production wires
//     StoreReplicasApplier{Store: store}.
//
// The state map (per-deployment last-up/last-down timestamps) is
// keyed by "<scope>/<name>" and grows monotonically. Stale entries
// for deleted deployments don't cause correctness problems — they
// just sit there. A future enhancement could prune them based on
// the live deployment list each tick, but the cost is negligible
// in practice.
type Autoscaler struct {
	// Store is the manifest source. The autoscaler lists deployments
	// every tick to discover which ones have autoscale blocks; it
	// never caches the list, so newly applied / deleted deployments
	// are picked up on the next tick without a restart.
	Store Store

	// Stats is the shared collector for live CPU% across pods. Reusing
	// it (rather than re-implementing docker stats parsing) means the
	// scaler sees the same per-replica numbers `vd stats` shows — one
	// source of truth.
	Stats *StatsCollector

	// Apply writes new replica counts. Indirection lets tests assert
	// "the scaler decided to scale to N" without spinning up a real
	// reconciler. Production: StoreReplicasApplier{Store: store}.
	Apply AutoscaleApplier

	// Logger is the controller's log target. The autoscaler logs each
	// scale event (with reason: CPU%, current replicas, new replicas)
	// so operators can correlate "vd describe" output with scaler
	// activity in the journal.
	Logger *log.Logger

	// Tick is the per-deployment evaluation cadence. Defaults to 15s
	// when zero — fast enough to respond to a ramp inside one human-
	// noticeable interval, slow enough to keep docker stats sampling
	// cheap on hosts with many containers.
	Tick time.Duration

	// state holds per-deployment cooldown timestamps. Keyed by
	// "<scope>/<name>". Reset to zero values across controller
	// restarts; see struct doc for the rationale.
	state sync.Map
}

// scaleState carries the last-scale-time per deployment, used to
// enforce cooldown windows. Only LastUp and LastDown matter — the
// current replica count is read fresh from the container list each
// tick.
type scaleState struct {
	LastUp   time.Time
	LastDown time.Time
}

// Run is the main loop. Blocks until ctx is cancelled. Logs but
// does not propagate per-tick errors — a transient docker hiccup or
// store read error should not kill the whole scaler.
func (a *Autoscaler) Run(ctx context.Context) {
	tick := a.Tick
	if tick <= 0 {
		tick = defaultAutoscalerTick
	}

	t := time.NewTicker(tick)
	defer t.Stop()

	// First tick fires immediately so the scaler doesn't sit idle
	// for the first Tick interval after process start — operators
	// expect a freshly applied autoscale block to take effect within
	// seconds, not minutes.
	a.evaluate(ctx)

	for {
		select {
		case <-ctx.Done():
			return

		case <-t.C:
			a.evaluate(ctx)
		}
	}
}

// evaluate walks every deployment with an autoscale block once.
// Per-deployment errors are logged but don't abort the sweep — one
// broken manifest shouldn't freeze scaling on every other one.
func (a *Autoscaler) evaluate(ctx context.Context) {
	mans, err := a.Store.List(ctx, KindDeployment)
	if err != nil {
		if a.Logger != nil {
			a.Logger.Printf("autoscaler: list deployments: %v", err)
		}

		return
	}

	for _, m := range mans {
		if m == nil {
			continue
		}

		spec, err := decodeDeploymentSpec(m)
		if err != nil {
			continue
		}

		if spec.Autoscale == nil {
			continue
		}

		if err := a.evaluateOne(ctx, m.Scope, m.Name, spec); err != nil {
			if a.Logger != nil {
				a.Logger.Printf("autoscaler: deployment/%s/%s: %v", m.Scope, m.Name, err)
			}
		}
	}
}

// evaluateOne is the per-deployment decision. Reads current replica
// count from the pods listing (the controller's ground truth — the
// container set the runtime actually has, not what we declared) and
// the mean CPU% across those replicas, then applies the hysteresis
// band and the cooldown windows to decide up/down/hold.
//
// Returns an error only for unrecoverable conditions (store/stats
// failures). The hold case is the silent success path.
func (a *Autoscaler) evaluateOne(ctx context.Context, scope, name string, spec deploymentSpec) error {
	as := spec.Autoscale

	// Read from the StatsCollector's in-memory snapshot (refreshed
	// every ~15s by the metrics sampler). Stale-by-up-to-one-tick is
	// fine for autoscale decisions: the cooldown windows are 30s+
	// anyway, so a 15s read-age never changes a scale verdict it
	// wouldn't have changed off a fresh sample. Saves a `docker stats`
	// roundtrip per deployment per evaluation tick on busy hosts.
	filter := StatsFilter{
		Kind:  string(KindDeployment),
		Scope: scope,
		Name:  name,
	}

	stats, _, ok := a.Stats.SnapshotPods(filter)
	if !ok {
		// Sampler hasn't ticked yet (first-boot warmup). Fall back
		// to a live call so the autoscaler doesn't sit idle until
		// the first snapshot lands.
		live, err := a.Stats.Collect(ctx, filter)
		if err != nil {
			return fmt.Errorf("collect stats: %w", err)
		}

		stats = live
	}

	if len(stats) == 0 {
		// No running pods (yet): the bootstrap path (ensureReplicaCount
		// off spec.Replicas) handles the "spec applied, pods not up"
		// transition. The scaler holds — it can't decide off no data.
		return nil
	}

	mean := meanCPUPercent(stats)
	current := len(stats)

	target := float64(as.CPUTarget)

	cooldownUp := parseDurationDefault(as.CooldownUp, defaultCooldownUp)
	cooldownDown := parseDurationDefault(as.CooldownDown, defaultCooldownDown)

	now := time.Now()

	st := a.loadState(scope, name)

	switch {
	case mean > target*scaleUpFactor && current < as.Max:
		if now.Sub(st.LastUp) < cooldownUp {
			return nil
		}

		next := current + 1
		if next > as.Max {
			next = as.Max
		}

		if err := a.Apply.SetReplicas(ctx, scope, name, next); err != nil {
			return fmt.Errorf("scale up: %w", err)
		}

		st.LastUp = now
		a.storeState(scope, name, st)

		if a.Logger != nil {
			a.Logger.Printf("autoscaler: deployment/%s/%s scale up %d -> %d (cpu=%.1f%% target=%d%%)",
				scope, name, current, next, mean, as.CPUTarget)
		}

	case mean < target*scaleDownFactor && current > as.Min:
		if now.Sub(st.LastDown) < cooldownDown {
			return nil
		}

		next := current - 1
		if next < as.Min {
			next = as.Min
		}

		if err := a.Apply.SetReplicas(ctx, scope, name, next); err != nil {
			return fmt.Errorf("scale down: %w", err)
		}

		st.LastDown = now
		a.storeState(scope, name, st)

		if a.Logger != nil {
			a.Logger.Printf("autoscaler: deployment/%s/%s scale down %d -> %d (cpu=%.1f%% target=%d%%)",
				scope, name, current, next, mean, as.CPUTarget)
		}

	default:
		// In the hold band, or already pinned at min/max in the
		// direction the CPU is pushing. No-op.
	}

	return nil
}

// loadState looks up the per-deployment cooldown timestamps. Returns
// a zero-value scaleState on first sight, which makes the first
// scale event always pass the cooldown check.
func (a *Autoscaler) loadState(scope, name string) scaleState {
	if v, ok := a.state.Load(autoscalerKey(scope, name)); ok {
		if st, ok := v.(scaleState); ok {
			return st
		}
	}

	return scaleState{}
}

// storeState writes back the updated cooldown timestamps.
func (a *Autoscaler) storeState(scope, name string, st scaleState) {
	a.state.Store(autoscalerKey(scope, name), st)
}

func autoscalerKey(scope, name string) string {
	return "deployment/" + scope + "/" + name
}

// meanCPUPercent computes the mean CPU% across the pod list, with
// one wrinkle: zero-CPU rows are dropped IF there are other rows
// with non-zero data. The reason is docker stats races — a pod that
// just started has not accumulated CPU samples yet, so its first
// reading is exactly 0.0, which would drag the mean down and either
// hide a real scale-up trigger or fire a phantom scale-down.
//
// If ALL rows are zero (a genuinely idle deployment), the mean is
// zero, which is the correct signal: the scaler will hold or scale
// down depending on the band.
func meanCPUPercent(stats []PodStats) float64 {
	if len(stats) == 0 {
		return 0
	}

	hasNonZero := false

	for _, s := range stats {
		if s.Usage.CPUPercent > 0 {
			hasNonZero = true
			break
		}
	}

	var sum float64
	count := 0

	for _, s := range stats {
		if hasNonZero && s.Usage.CPUPercent == 0 {
			continue
		}

		sum += s.Usage.CPUPercent
		count++
	}

	if count == 0 {
		return 0
	}

	return sum / float64(count)
}

// parseDurationDefault wraps time.ParseDuration with a fallback to
// the supplied default when the input is empty or unparseable. The
// scaler doesn't surface parse errors — the parser already
// validates the wider schema, and falling back to a sane default
// on malformed runtime input is more useful than refusing to scale
// the deployment.
func parseDurationDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}

	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return def
	}

	return d
}

// StoreReplicasApplier is the production AutoscaleApplier. Reads
// the stored manifest, mutates spec.replicas in-place, and Put's
// the result back. The store's watch path picks up the change and
// the standard reconciler flow (ensureReplicaCount) spawns / stops
// pods — same path a `vd apply` would trigger.
//
// Loop-back safety: the autoscaler reads spec.Replicas indirectly
// via the pod list (current count = number of running containers),
// not directly from the manifest. So an autoscaler-written Replicas
// = N doesn't immediately re-trigger evaluation — the next tick
// will see "current = N matches desired = N" and hold unless CPU
// has moved.
type StoreReplicasApplier struct {
	Store Store
}

// SetReplicas reads the deployment manifest, mutates the
// `replicas` field of its JSON spec in-place, and writes it back.
// We round-trip the spec as map[string]any so unrelated fields
// (build args, env, asset digests) are preserved verbatim — using
// the typed deploymentSpec for re-encoding would risk silently
// dropping any field the controller-side struct hasn't surfaced
// yet (plugin-stamped state, future additions, etc.).
func (a StoreReplicasApplier) SetReplicas(ctx context.Context, scope, name string, replicas int) error {
	m, err := a.Store.Get(ctx, KindDeployment, scope, name)
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}

	if m == nil {
		// Deployment was deleted between the scaler's list and apply.
		// Silent success — there's nothing to scale.
		return nil
	}

	var spec map[string]any
	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return fmt.Errorf("unmarshal spec: %w", err)
	}

	spec["replicas"] = replicas

	newSpec, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal spec: %w", err)
	}

	m.Spec = newSpec

	if _, err := a.Store.Put(ctx, m); err != nil {
		return fmt.Errorf("put manifest: %w", err)
	}

	return nil
}
