// probes.go is the controller-side glue between the manifest's
// probes block and the internal/probe package's runner. The
// registry owns up to three probe runners per container
// (liveness, readiness, startup), starts them when a container
// is born, cancels them when the container is removed, and
// wires the OnTrigger / phase-transition callbacks for
// liveness-failure restarts and readiness state propagation.
//
// M1.1 shipped liveness only. M1.2 wires readiness + startup +
// caddy ingress readiness gating:
//
//   - Liveness: failure threshold → docker restart (unchanged from
//     M1.1).
//
//   - Readiness: phase transitions → flip the replica's ready bit
//     in DeploymentStatus.ReplicaReadiness; caddy ingress can
//     consult this (directly or via the dedicated
//     /pods/{name}/ready endpoint) to skip unready upstreams.
//     Persistence is debounced: only Transition events flush to
//     the status blob, steady-state samples don't touch storage.
//
//   - Startup: short-lived (StopOnReady=true). Replica counts as
//     "not ready" until the startup probe first hits PhaseHealthy,
//     regardless of what readiness reports. Mirrors kubelet's
//     "startup gates readiness" rule.
//
// Concurrency model:
//
//   - One ProbeRegistry per controller (lives on DeploymentHandler).
//   - sync.Map keyed by docker container name; values are
//     *runnerEntry with cancel funcs + done channels for every
//     spawned runner + the per-replica readiness state.
//   - Start is idempotent — calling on an already-running entry is
//     a no-op so reconciles can replay without spawning duplicates.
//   - Stop cancels every spawned runner for the container and waits
//     for them to drain so the next Start sees a clean slate. Also
//     clears the persisted ReplicaReadiness entry (via the
//     Recorder) so a removed pod doesn't leave a ghost in
//     describe.

package controller

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/probe"
)

// ProbeRestarter is the seam the registry uses to act on liveness
// failures. Production wires docker.RestartContainer; tests
// substitute a fake to assert "restart was called for container X."
type ProbeRestarter interface {
	RestartContainer(name string) error
}

// ContainerIPResolver gives the registry the container's bridge IP
// for HTTP / TCP probes. Production wires docker.ContainerIP; tests
// inject canned IPs.
type ContainerIPResolver interface {
	ContainerIP(name string) (string, error)
}

// ProbeExecutor is the docker-exec hook for exec probes. Production
// wires DockerContainerManager.Exec (the same one /exec uses);
// tests inject fakes. Signature matches probe.ExecRunner so the
// adapter is trivial.
type ProbeExecutor interface {
	Exec(name string, command []string, opts ExecOptions) (int, error)
}

// ReadinessRecorder is the seam ProbeRegistry uses to publish
// per-replica readiness phase changes to the controller's status
// store. Decoupled via interface so the registry doesn't take a
// hard dependency on DeploymentHandler (and so tests can verify
// "Record was called with X" without standing up etcd).
//
// nil-tolerant on the registry — when Recorder is nil the registry
// still spawns runners and reports state in memory; only the
// cross-restart persistence and describe-visibility are skipped.
type ReadinessRecorder interface {
	RecordReplicaReadiness(ctx context.Context, app string, status ReplicaReadinessStatus)
	ClearReplicaReadiness(ctx context.Context, app, containerName string)

	// OnProbeTransition is called once per healthy↔unhealthy edge
	// the probe runner detects. The state-machine gating
	// (plannedTeardown suppression, hadFailure recovery gate)
	// happens inside the registry BEFORE this method is invoked,
	// so by the time a recorder sees the call it's guaranteed to
	// be a "fire-worthy" transition.
	//
	// Recorder impls (DeploymentHandler, StatefulsetHandler)
	// stamp ev.Kind, look up the cached on_probe spec, and call
	// fireProbeWebhook. The registry stays free of webhook /
	// HTTP knowledge.
	//
	// nil-tolerant: a Recorder may implement this as a no-op
	// (no webhooks wired in tests, or operator hasn't declared
	// on_probe on any resource).
	OnProbeTransition(ctx context.Context, ev ProbeTransitionEvent)
}

// ReplicaReadinessStatus is one entry in
// DeploymentStatus.ReplicaReadiness, plus the surface
// /pods/{name}/ready returns to caddy / operators. Lives at the
// kind level (not inside the probe package) so the JSON shape
// stays a controller concern.
type ReplicaReadinessStatus struct {
	// ContainerName is the docker name (`voodu-x-web.a3f9`). The
	// keying is by container name so caddy can correlate to its
	// upstream addresses without needing to know about replica IDs.
	ContainerName string `json:"container_name"`

	// ReplicaID is the short hex id (deployments) or ordinal
	// (statefulsets, when those wire up in a future milestone).
	// Carried for describe so the operator doesn't have to parse
	// the container name to find a replica.
	ReplicaID string `json:"replica_id,omitempty"`

	// Ready aggregates startup + readiness phase per the rule:
	// (no startup probe OR startup passed) AND (no readiness
	// probe OR readiness PhaseHealthy). Backward compat: a
	// replica with no probes declared has Ready=true.
	Ready bool `json:"ready"`

	// StartupPassed is true once the startup probe first hit
	// PhaseHealthy. Permanently true thereafter; the startup
	// Runner self-stops at that point. False until success when
	// a startup probe is declared; true at construction time
	// when no startup probe is declared (no-op gate).
	StartupPassed bool `json:"startup_passed"`

	// ReadinessPhase is the readiness Runner's current phase as
	// a string ("healthy" | "unhealthy" | "unknown"). Empty
	// when no readiness probe is declared.
	ReadinessPhase string `json:"readiness_phase,omitempty"`

	// Reason carries the last probe Result's Reason string,
	// surfaced verbatim so the operator sees WHY a pod is
	// unready ("GET /healthz → 502").
	Reason string `json:"reason,omitempty"`

	// LastTransition is the wall-clock timestamp of the most
	// recent phase change. Used by the LRU eviction in
	// DeploymentStatus.ReplicaReadiness and shown in describe.
	LastTransition time.Time `json:"last_transition,omitempty"`
}

// dockerRestarter is the production ProbeRestarter — a one-method
// stub around the docker package func so DeploymentHandler doesn't
// have to take a function pointer.
type dockerRestarter struct{}

func (dockerRestarter) RestartContainer(name string) error {
	return docker.RestartContainer(name)
}

// dockerIPResolver is the production ContainerIPResolver.
type dockerIPResolver struct{}

func (dockerIPResolver) ContainerIP(name string) (string, error) {
	return docker.ContainerIP(name)
}

// ProbeRegistry tracks live probe runners and dispatches lifecycle
// events. One per controller process — DeploymentHandler holds the
// reference and forwards container Start/Stop into the registry.
type ProbeRegistry struct {
	// Restart fires when a liveness probe trips. Nil-tolerant —
	// tests can pass nil to assert "no restart should have been
	// called" by observing no call records.
	Restart ProbeRestarter

	// IPs resolves a container name to its bridge IP. Nil →
	// HTTPGet/TCPSocket probes silently fail (the runner's
	// "no host" guard kicks in); useful for tests that only care
	// about exec probes.
	IPs ContainerIPResolver

	// Exec is the docker exec hook for exec probes. Nil → exec
	// probes always fail (handled in internal/probe — surfaces a
	// clear "no exec runner configured" reason). Production wires
	// DockerContainerManager.
	Exec ProbeExecutor

	// Recorder persists readiness phase changes to
	// DeploymentStatus. Nil → registry stays in-memory only
	// (tests, or a controller mode that doesn't want
	// cross-restart visibility). Production wires
	// *DeploymentHandler.
	Recorder ReadinessRecorder

	// Log carries the registry's own structured log lines (probe
	// triggered, restart called, etc.). Nil → falls back to
	// log.Default at Start time.
	Log *log.Logger

	// runners maps container name → cancel-and-wait + readiness
	// state for that container. Values are *runnerEntry.
	runners sync.Map // map[string]*runnerEntry
}

// runnerEntry is the per-container bookkeeping. cancels stops each
// spawned runner (one entry per spawned probe type: liveness,
// readiness, startup); dones receive the Stopped() channels. state
// is the live per-replica readiness gate the events-drain
// goroutines mutate as phase transitions happen.
//
// state.mu (declared inside replicaReadiness) protects field-level
// writes; the entry-level access is through sync.Map's Load /
// LoadAndDelete so registry-level concurrency is already covered.
type runnerEntry struct {
	cancels []context.CancelFunc
	dones   []<-chan struct{}
	state   *replicaReadiness

	// onProbe is the operator-supplied webhook spec for this
	// container's probe transitions. Cached on the runner so the
	// drain goroutines fetch it locally instead of round-tripping
	// to etcd per transition. Immutable after Start; nil for
	// resources that didn't declare on_probe.
	onProbe *onProbeWireSpec
}

// replicaReadiness is the in-memory mirror of
// ReplicaReadinessStatus — the live source of truth a probe
// transition mutates. Persistence to DeploymentStatus runs through
// the Recorder seam; in-memory reads (the /pods/{name}/ready fast
// path) consult this struct directly.
//
// mu guards every field so the events-drain goroutines + the
// LookupReadiness reader can interleave without tearing.
type replicaReadiness struct {
	mu sync.Mutex

	hasStartup     bool
	hasReadiness   bool
	startupPassed  bool
	readinessPhase probe.Phase
	reason         string
	lastTransition time.Time

	// plannedTeardown is set by MarkPlannedTeardown immediately
	// before Stop so the drain goroutines suppress webhook
	// emission for the in-flight transitions caused by graceful
	// container shutdown (rolling restart, scale-down, manual
	// vd restart). Cleared by Stop's entry deletion.
	plannedTeardown bool

	// hadFailure is the recovery-gating state machine: true once
	// the runner has observed at least one failure transition,
	// reset to false after firing a recovery event. The next
	// recovery requires another failure first. Prevents the
	// "freshly started pod going healthy on its first probe"
	// case from emitting a spurious recovery webhook on every
	// vd apply.
	hadFailure bool
}

// ready aggregates startup + readiness into a single Ready bool.
// See ReplicaReadinessStatus.Ready for the contract:
// (no startup probe OR startupPassed) AND (no readiness probe OR
// readiness PhaseHealthy).
//
// Lock-free for callers — does its own mu.Lock/Unlock.
func (s *replicaReadiness) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	startupOK := !s.hasStartup || s.startupPassed
	readinessOK := !s.hasReadiness || s.readinessPhase == probe.PhaseHealthy

	return startupOK && readinessOK
}

// snapshot returns a flat ReplicaReadinessStatus suitable for
// JSON serialisation / Recorder calls. Takes mu so the read is
// atomic with respect to ongoing mutations.
func (s *replicaReadiness) snapshot(containerName, replicaID string) ReplicaReadinessStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	phaseStr := ""
	if s.hasReadiness {
		phaseStr = s.readinessPhase.String()
	}

	startupOK := !s.hasStartup || s.startupPassed
	readinessOK := !s.hasReadiness || s.readinessPhase == probe.PhaseHealthy

	return ReplicaReadinessStatus{
		ContainerName:  containerName,
		ReplicaID:      replicaID,
		Ready:          startupOK && readinessOK,
		StartupPassed:  s.startupPassed,
		ReadinessPhase: phaseStr,
		Reason:         s.reason,
		LastTransition: s.lastTransition,
	}
}

// Start spawns probe runners for the given container according to
// the spec. Idempotent — calling on a container that already has
// runners is a no-op (the existing runners keep going).
//
// `app` is the AppID used by the Recorder seam to namespace the
// persisted readiness map. Different from `containerName` because
// one app can have many replica containers — the status blob keys
// by app but the readiness map inside it keys by container name.
//
// Returns no error: missing dependencies (no IP, no exec, malformed
// spec) are surfaced as the runner's Result reasons, not as a Start
// failure. The runner stays up emitting failed Events so the
// operator can see WHY the probe isn't passing — silent skip would
// hide misconfiguration.
//
// nilSpec or a probes block with all three sub-blocks empty is a
// clean no-op so callers can wire Start unconditionally.
//
// onProbe is the operator-supplied webhook spec for this
// container's transitions. Cached on the runnerEntry so the
// drain goroutines emit OnProbeTransition events with the right
// targets pre-attached. nil → no webhooks fired (steady state for
// resources without on_probe declared).
func (r *ProbeRegistry) Start(app, containerName string, spec *probesWireSpec, onProbe *onProbeWireSpec) {
	if r == nil || spec == nil {
		return
	}

	// No probes declared at all — the most common case for legacy
	// deployments. Clean no-op preserves backward compat.
	if spec.Liveness == nil && spec.Readiness == nil && spec.Startup == nil {
		return
	}

	// Already running — replay-safe no-op. The hash-folding of the
	// probes block guarantees that any spec change triggers a
	// rolling restart, which Stop-then-Start cycles the runner.
	if _, ok := r.runners.Load(containerName); ok {
		return
	}

	host := ""

	if r.IPs != nil {
		ip, err := r.IPs.ContainerIP(containerName)
		if err == nil {
			host = ip
		}
		// On lookup failure we still spawn — the runner emits
		// failed probes with a clean reason ("dial: no such host")
		// rather than silently skipping.
	}

	logger := r.Log
	if logger == nil {
		logger = log.Default()
	}

	state := &replicaReadiness{
		hasStartup:   spec.Startup != nil,
		hasReadiness: spec.Readiness != nil,
		// No startup probe → gate is open from t=0 (the StartupPassed
		// bit acts as "the gate is closed" only when a startup
		// probe is actually declared).
		startupPassed: spec.Startup == nil,
		// No readiness probe → readiness is "healthy" by definition.
		// Setting PhaseHealthy keeps ready() trivially true for the
		// most common case (no probes declared at all).
		readinessPhase: probe.PhaseHealthy,
	}

	entry := &runnerEntry{
		state:   state,
		onProbe: onProbe,
	}

	// Spawn startup probe first so the gate logic is in place
	// before liveness/readiness start emitting events. The order
	// doesn't actually affect correctness (the state mutex
	// serialises updates), but it's easier to reason about.
	if spec.Startup != nil {
		r.spawnStartup(containerName, host, spec.Startup, app, entry, logger)
	}

	if spec.Liveness != nil {
		r.spawnLiveness(containerName, host, app, spec.Liveness, entry, logger)
	}

	if spec.Readiness != nil {
		r.spawnReadiness(containerName, host, spec.Readiness, app, entry, logger)
	}

	r.runners.Store(containerName, entry)

	// Initial state push: send the at-creation snapshot to the
	// Recorder so describe can show "starting up" before any probe
	// has actually fired. Skip when no Recorder is wired.
	if r.Recorder != nil {
		go r.Recorder.RecordReplicaReadiness(context.Background(), app, state.snapshot(containerName, replicaIDFromContainerName(containerName)))
	}
}

// spawnLiveness creates the per-container liveness runner and
// drains its events. OnTrigger fires docker restart on the
// transition into PhaseUnhealthy — same behaviour as M1.1; the
// drain additionally emits OnProbeTransition so the webhook
// firer learns about the same edges.
func (r *ProbeRegistry) spawnLiveness(containerName, host, app string, livenessSpec *probeWireSpec, entry *runnerEntry, logger *log.Logger) {
	probeSpec := wireToProbeSpec(livenessSpec)

	ctx, cancel := context.WithCancel(context.Background())

	runner := probe.New(probeSpec, containerName, host, probe.Options{
		Exec: r.execAdapter(containerName),
		OnTrigger: func(res probe.Result) {
			// Run the restart in its own goroutine so docker
			// restart's 10s+ latency doesn't stall the next probe
			// sample. The probe Runner suppresses re-firing
			// OnTrigger while already unhealthy, so two restart
			// calls back-to-back only happen on a healthy→unhealthy
			// re-trip — exactly the right signal.
			go func() {
				if r.Restart == nil {
					logger.Printf("probe/%s liveness failed (%s) but no restarter configured", containerName, res.Reason)
					return
				}

				logger.Printf("probe/%s liveness failed (%s) — restarting container", containerName, res.Reason)

				if err := r.Restart.RestartContainer(containerName); err != nil {
					logger.Printf("probe/%s restart failed: %v", containerName, err)
				}
			}()
		},
	})

	entry.cancels = append(entry.cancels, cancel)
	entry.dones = append(entry.dones, runner.Stopped())

	go runner.Run(ctx)

	// Liveness doesn't propagate state to /pods/{name}/ready (that's
	// the readiness probe's job), but we DO consume its events to
	// emit OnProbeTransition for the on_probe webhook firer.
	// Healthy→Unhealthy fires failure (alongside the OnTrigger
	// restart); Unhealthy→Healthy fires recovery once the
	// post-restart container probes pass again. The drain naturally
	// ends when ctx is cancelled and the runner closes its events
	// channel.
	go r.drainLivenessEvents(runner.Events(), containerName, app, entry, logger)
}

// spawnReadiness creates the readiness runner and wires the
// phase-transition drain. Persistence is debounced via the
// Transition flag — only an actual phase change writes to the
// recorder.
func (r *ProbeRegistry) spawnReadiness(containerName, host string, readinessSpec *probeWireSpec, app string, entry *runnerEntry, logger *log.Logger) {
	probeSpec := wireToProbeSpec(readinessSpec)

	ctx, cancel := context.WithCancel(context.Background())

	// Readiness has no OnTrigger — it's not a "restart container"
	// signal. State propagation happens through the events drain
	// below, which filters on Transition so steady-state samples
	// don't touch storage.
	//
	// Initial state: readinessPhase starts at PhaseHealthy in
	// `state` (set in Start above). The first Event from the
	// runner overrides that if it samples differently — at which
	// point Transition fires and we persist the real phase.
	// Setting it to PhaseUnknown initially would mean every fresh
	// replica is briefly "not ready" — but for a brand-new
	// container with the app not yet listening, that's actually
	// correct behaviour. So we flip the initial state to
	// PhaseUnknown here, BEFORE the runner samples, so the first
	// healthy sample registers as a transition (Unknown → Healthy)
	// and the recorder gets the proper "now ready" event.
	entry.state.mu.Lock()
	entry.state.readinessPhase = probe.PhaseUnknown
	entry.state.mu.Unlock()

	runner := probe.New(probeSpec, containerName, host, probe.Options{
		Exec: r.execAdapter(containerName),
	})

	entry.cancels = append(entry.cancels, cancel)
	entry.dones = append(entry.dones, runner.Stopped())

	go runner.Run(ctx)

	go r.drainReadinessEvents(runner.Events(), containerName, app, entry, logger)
}

// spawnStartup creates the startup runner. StopOnReady=true makes
// the runner self-stop on the first PhaseHealthy transition; we
// catch that transition in the drain to flip startupPassed.
//
// Startup probe Events get a different drain than readiness
// because: (a) startup has a single significant transition (the
// gate opening), (b) the runner stops on that transition, so the
// drain is short-lived.
func (r *ProbeRegistry) spawnStartup(containerName, host string, startupSpec *probeWireSpec, app string, entry *runnerEntry, logger *log.Logger) {
	probeSpec := wireToProbeSpec(startupSpec)

	ctx, cancel := context.WithCancel(context.Background())

	runner := probe.New(probeSpec, containerName, host, probe.Options{
		Exec:        r.execAdapter(containerName),
		StopOnReady: true,
	})

	entry.cancels = append(entry.cancels, cancel)
	entry.dones = append(entry.dones, runner.Stopped())

	go runner.Run(ctx)

	go r.drainStartupEvents(runner.Events(), containerName, app, entry, logger)
}

// drainReadinessEvents consumes the readiness runner's events,
// mutates state on transitions, and pushes to the Recorder once
// per transition. Steady-state samples (Transition=false) update
// nothing — exactly the debouncing the plan calls for.
//
// In addition to the readiness state push, this drain now emits
// OnProbeTransition events so the on_probe webhook firer reacts
// to the same edges. State-machine gating (plannedTeardown,
// hadFailure) lives in emitProbeTransition.
func (r *ProbeRegistry) drainReadinessEvents(events <-chan probe.Event, containerName, app string, entry *runnerEntry, logger *log.Logger) {
	for ev := range events {
		if !ev.Transition {
			continue
		}

		entry.state.mu.Lock()
		entry.state.readinessPhase = ev.Phase
		entry.state.reason = ev.Result.Reason
		entry.state.lastTransition = time.Now().UTC()
		entry.state.mu.Unlock()

		logger.Printf("probe/%s readiness → %s (%s)", containerName, ev.Phase.String(), ev.Result.Reason)

		if r.Recorder != nil {
			snap := entry.state.snapshot(containerName, replicaIDFromContainerName(containerName))
			r.Recorder.RecordReplicaReadiness(context.Background(), app, snap)
		}

		r.emitProbeTransition(app, containerName, "readiness", ev, entry)
	}
}

// drainLivenessEvents consumes the liveness runner's events solely
// for OnProbeTransition emission. State updates (readiness phase,
// snapshot to /pods/{name}/ready) belong to the readiness drain —
// liveness conceptually says "container is alive or being
// restarted", not "ready to take traffic". The runner's own
// OnTrigger callback still handles the docker restart side.
//
// Replaces M1.1's no-op drainEvents which discarded these events
// entirely. Now we put them to work.
func (r *ProbeRegistry) drainLivenessEvents(events <-chan probe.Event, containerName, app string, entry *runnerEntry, logger *log.Logger) {
	for ev := range events {
		if !ev.Transition {
			continue
		}

		logger.Printf("probe/%s liveness → %s (%s)", containerName, ev.Phase.String(), ev.Result.Reason)

		r.emitProbeTransition(app, containerName, "liveness", ev, entry)
	}
}

// drainStartupEvents listens for startup probe transitions. The
// PhaseHealthy edge (the "gate opens" signal) flips startupPassed
// and lets the runner self-exit (StopOnReady=true).
//
// Unhealthy transitions are NOT a no-op anymore: we emit
// OnProbeTransition so on_probe.failure fires when the container
// stays unhealthy past its FailureThreshold — the operator gets a
// signal that the pod is failing to come up. The runner keeps
// probing until either healthy (then exits) or ctx is cancelled.
//
// We don't fire on_probe.recovery from startup transitions
// because the "gate opened" event is not a recovery — it's the
// first time the pod was ever healthy, which the state machine
// in emitProbeTransition already filters out via the hadFailure
// gate. If a startup probe went unhealthy then healthy, the
// recovery WILL fire because hadFailure was set by the unhealthy
// drain.
func (r *ProbeRegistry) drainStartupEvents(events <-chan probe.Event, containerName, app string, entry *runnerEntry, logger *log.Logger) {
	for ev := range events {
		if !ev.Transition {
			continue
		}

		if ev.Phase == probe.PhaseHealthy {
			entry.state.mu.Lock()
			entry.state.startupPassed = true
			entry.state.lastTransition = time.Now().UTC()
			entry.state.reason = ev.Result.Reason
			entry.state.mu.Unlock()

			logger.Printf("probe/%s startup probe passed (%s)", containerName, ev.Result.Reason)

			if r.Recorder != nil {
				snap := entry.state.snapshot(containerName, replicaIDFromContainerName(containerName))
				r.Recorder.RecordReplicaReadiness(context.Background(), app, snap)
			}
		}

		r.emitProbeTransition(app, containerName, "startup", ev, entry)
		// The runner self-exits on PhaseHealthy; the for-range
		// exits when its events channel closes. No explicit break.
	}
}

// emitProbeTransition is the shared edge-to-event helper used by
// all three drains. Encapsulates:
//
//   - plannedTeardown suppression (skip emission when the
//     container is being gracefully stopped)
//   - recovery state-machine gating (a fresh runner's first
//     PhaseHealthy doesn't fire recovery — only after a prior
//     PhaseUnhealthy)
//   - hadFailure bookkeeping (set on failure, reset on recovery
//     so the next cycle has to see another failure before its
//     recovery webhook fires)
//   - PhaseUnknown filter (no edge worth a webhook)
//
// Calls Recorder.OnProbeTransition only when all gates pass.
// Nil-tolerant: skips when r.Recorder is nil (in-memory test
// configuration).
func (r *ProbeRegistry) emitProbeTransition(app, containerName, probeName string, ev probe.Event, entry *runnerEntry) {
	if r.Recorder == nil || entry == nil {
		return
	}

	if ev.Phase != probe.PhaseHealthy && ev.Phase != probe.PhaseUnhealthy {
		// Unknown / other phases are not edge-worthy.
		return
	}

	var transition string

	entry.state.mu.Lock()

	if entry.state.plannedTeardown {
		entry.state.mu.Unlock()
		// Suppress: this transition is caused by an
		// orchestrator-driven stop, not a real probe failure.
		return
	}

	switch ev.Phase {
	case probe.PhaseUnhealthy:
		transition = "failure"
		entry.state.hadFailure = true
	case probe.PhaseHealthy:
		if !entry.state.hadFailure {
			entry.state.mu.Unlock()
			// First-time healthy on a fresh runner — normal
			// startup, not a recovery from anything.
			return
		}

		transition = "recovery"
		// Reset so the next recovery requires another failure
		// first. Without this reset, a flapping container that
		// goes failure→recovery→recovery would fire two recovery
		// webhooks; the state machine should be paired edges only.
		entry.state.hadFailure = false
	}

	entry.state.mu.Unlock()

	scope, name := splitAppID(app)

	r.Recorder.OnProbeTransition(context.Background(), ProbeTransitionEvent{
		Scope:      scope,
		Name:       name,
		Pod:        containerName,
		Probe:      probeName,
		Transition: transition,
		Reason:     ev.Result.Reason,
		At:         time.Now().UTC(),
		OnProbe:    entry.onProbe,
	})
}

// MarkPlannedTeardown flags a container's runnerEntry so the
// drain goroutines suppress OnProbeTransition emission for any
// transitions caused by the orchestrator's graceful shutdown
// (rolling restart, scale-down, manual vd restart). Must be
// called IMMEDIATELY before Probes.Stop — the flag is read by
// the drains during the cancellation race, so setting it after
// Stop would be too late.
//
// No-op when the container has no live runner (Stop already
// called, or never started). Safe to call multiple times.
func (r *ProbeRegistry) MarkPlannedTeardown(app, containerName string) {
	if r == nil {
		return
	}

	v, ok := r.runners.Load(containerName)
	if !ok {
		return
	}

	entry, ok := v.(*runnerEntry)
	if !ok || entry == nil || entry.state == nil {
		return
	}

	entry.state.mu.Lock()
	entry.state.plannedTeardown = true
	entry.state.mu.Unlock()

	// app is unused for now (the flag is per-container, app is
	// just there to mirror Stop's signature so the wiring is
	// symmetric). Reserved in case future suppression rules
	// need per-app scoping.
	_ = app
}

// compositeReadinessLookup chains multiple ReadinessLookup
// implementations — first hit wins. Used to expose a single
// /pods/{name}/ready endpoint that spans deployments and
// statefulsets without having to teach the caller which kind
// owns a given container.
//
// Order matters only when the SAME container name lives in
// both registries (shouldn't happen — docker enforces uniqueness
// host-wide), so the slice ordering is purely about lookup
// priority.
type compositeReadinessLookup []ReadinessLookup

func (c compositeReadinessLookup) LookupReadiness(containerName string) (ReplicaReadinessStatus, bool) {
	for _, l := range c {
		if l == nil {
			continue
		}

		if s, ok := l.LookupReadiness(containerName); ok {
			return s, true
		}
	}

	return ReplicaReadinessStatus{}, false
}

// LookupReadiness returns the in-memory ReplicaReadinessStatus for
// a container, plus a found bool. Fast path for
// GET /pods/{name}/ready — no etcd hop, no JSON decode, no lock
// contention with other registries.
//
// Returns (zero, false) when the container has no live runner —
// either never had probes declared, or the runner was Stopped.
// Callers should fall back to /status read for cross-restart
// scenarios.
func (r *ProbeRegistry) LookupReadiness(containerName string) (ReplicaReadinessStatus, bool) {
	if r == nil {
		return ReplicaReadinessStatus{}, false
	}

	v, ok := r.runners.Load(containerName)
	if !ok {
		return ReplicaReadinessStatus{}, false
	}

	entry, ok := v.(*runnerEntry)
	if !ok || entry == nil || entry.state == nil {
		return ReplicaReadinessStatus{}, false
	}

	return entry.state.snapshot(containerName, replicaIDFromContainerName(containerName)), true
}

// Stop cancels every runner spawned for the named container and
// blocks until each has exited. Idempotent — calling Stop on a
// container that has no runners is a no-op.
//
// Also clears the persisted ReplicaReadiness entry (via the
// Recorder, when wired) so a removed pod doesn't leave a ghost
// in describe.
//
// The synchronous wait is intentional: the caller (typically
// DeploymentHandler before docker remove) wants to know the
// runners are GONE before tearing down the container, so they
// can't make spurious restart calls against a container in the
// middle of being deleted.
func (r *ProbeRegistry) Stop(app, containerName string) {
	if r == nil {
		return
	}

	v, ok := r.runners.LoadAndDelete(containerName)
	if !ok {
		return
	}

	entry, ok := v.(*runnerEntry)
	if !ok || entry == nil {
		return
	}

	for _, c := range entry.cancels {
		c()
	}

	// Wait for every spawned runner to drain. 5s per runner is
	// generous; the probe Runner exits on the first ctx.Done()
	// observed inside the sample loop, which is bounded by the
	// probe Timeout (default 1s).
	deadline := time.After(5 * time.Second)
	for _, done := range entry.dones {
		select {
		case <-done:
		case <-deadline:
			if r.Log != nil {
				r.Log.Printf("probe/%s a runner did not stop within 5s — leaking goroutine", containerName)
			}
		}
	}

	if r.Recorder != nil && app != "" {
		r.Recorder.ClearReplicaReadiness(context.Background(), app, containerName)
	}
}

// StopAll cancels every runner the registry knows about. Used on
// controller shutdown — leave no goroutines behind. Doesn't try
// to look up the app for each entry since we have no per-entry
// app stored; persistence cleanup happens at the per-deployment
// remove() path instead.
func (r *ProbeRegistry) StopAll() {
	if r == nil {
		return
	}

	// Snapshot the keys first so the LoadAndDelete inside Stop
	// doesn't race with Range's iteration semantics.
	names := make([]string, 0)
	r.runners.Range(func(key, _ any) bool {
		name, _ := key.(string)
		names = append(names, name)
		return true
	})

	// Deterministic order so log output (and any controller-
	// shutdown audit) stays predictable across restarts.
	sort.Strings(names)

	for _, name := range names {
		// "" for app skips the Recorder.Clear call — the
		// controller-shutdown path doesn't care about scrubbing
		// state since the process is going away.
		r.Stop("", name)
	}
}

// drainEvents is the no-op consumer used by liveness — we don't
// react to liveness events for state propagation (the OnTrigger
// callback handles the only side effect). Drain so the runner's
// buffered channel doesn't stall the probe loop.
func drainEvents(events <-chan probe.Event) {
	for range events {
	}
}

// replicaIDFromContainerName recovers the trailing-after-`.`
// component from a docker container name composed by
// containers.ContainerName. Matches the inverse of:
//
//	<scope>-<name>.<replicaID>     scoped
//	<name>.<replicaID>             unscoped
//
// Returns the whole name on no dot (defensive — every voodu-managed
// container has one, but legacy / hand-spawned ones might not).
func replicaIDFromContainerName(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[i+1:]
		}
	}

	return name
}

// execAdapter wraps the controller's ProbeExecutor into the
// probe package's ExecRunner shape (exec returns (code, stderr,
// err); the controller's exec returns just (code, err)). The
// adapter buffers stdout/stderr in-memory so the failure reason
// surfaces in probe events — operators reading `vd describe pod`
// see "exit 2 — connection refused" not just "exit 2".
//
// Returns nil when r.Exec is nil so the probe package's defensive
// nil check fires with the canonical "no exec runner configured"
// reason.
func (r *ProbeRegistry) execAdapter(container string) probe.ExecRunner {
	if r.Exec == nil {
		return nil
	}

	return &probeExecAdapter{container: container, exec: r.Exec}
}

type probeExecAdapter struct {
	container string
	exec      ProbeExecutor
}

func (a *probeExecAdapter) Exec(ctx context.Context, container string, command []string) (int, string, error) {
	// Capture stderr in a small buffer for the failure-reason
	// string. The probe package surfaces this verbatim in
	// Result.Reason — so an operator reading the log sees the
	// in-container error text alongside the exit code.
	stderr := &capturingWriter{limit: 1024}

	// Container is passed-in for parity with the probe.ExecRunner
	// interface; we honour the adapter's pre-bound name since
	// each runner is per-container.
	_ = container

	code, err := a.exec.Exec(a.container, command, ExecOptions{
		Stderr: stderr,
		TTY:    false,
	})

	return code, stderr.String(), err
}

// capturingWriter is a small io.Writer that buffers up to `limit`
// bytes and silently drops the rest. Used by the exec adapter for
// the stderr capture — we don't need the full output, just enough
// to surface a meaningful failure reason in probe events.
type capturingWriter struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (w *capturingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.buf) >= w.limit {
		return len(p), nil // pretend we wrote it
	}

	remaining := w.limit - len(w.buf)
	if len(p) > remaining {
		w.buf = append(w.buf, p[:remaining]...)
	} else {
		w.buf = append(w.buf, p...)
	}

	return len(p), nil
}

func (w *capturingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return string(w.buf)
}

// wireToProbeSpec converts the controller wire shape into the
// internal/probe Spec. Duration strings are parsed permissively —
// invalid values fall through to the package defaults (matches
// kubelet behaviour: a malformed period becomes the 10s default
// rather than failing the apply).
func wireToProbeSpec(p *probeWireSpec) probe.Spec {
	out := probe.Spec{
		InitialDelay:     parseProbeDuration(p.InitialDelay),
		Period:           parseProbeDuration(p.Period),
		Timeout:          parseProbeDuration(p.Timeout),
		FailureThreshold: p.FailureThreshold,
		SuccessThreshold: p.SuccessThreshold,
	}

	if p.HTTPGet != nil {
		out.Action.HTTPGet = &probe.HTTPGetAction{
			Path:        p.HTTPGet.Path,
			Port:        p.HTTPGet.Port,
			Scheme:      p.HTTPGet.Scheme,
			HTTPHeaders: p.HTTPGet.HTTPHeaders,
		}
	}

	if p.TCPSocket != nil {
		out.Action.TCPSocket = &probe.TCPSocketAction{Port: p.TCPSocket.Port}
	}

	if p.Exec != nil {
		out.Action.Exec = &probe.ExecAction{Command: p.Exec.Command}
	}

	return out
}

// parseProbeDuration is a tolerant time.ParseDuration wrapper.
// Invalid strings (or empty) return 0, which the probe package
// then promotes to its default. This matches kubelet's "broken
// probe config falls back to defaults rather than blocking apply"
// posture.
//
// Named with the probe- prefix to avoid collision with the
// package-local parseDurationOrZero used elsewhere — same shape,
// different domain.
func parseProbeDuration(s string) time.Duration {
	if s == "" {
		return 0
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}

	return d
}
