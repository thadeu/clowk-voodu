// runner.go owns the per-container probe lifecycle. One Runner per
// (container, probe-type) tuple; the Runner ticks at Spec.Period,
// counts consecutive successes/failures against the thresholds, and
// fires its OnTrigger callback when a threshold is crossed.
//
// Liveness probes use OnTrigger to ask docker to restart the
// container. Readiness probes use it to flip the pod's ready bit
// in etcd (M1.2). Startup probes are short-lived: once they hit
// SuccessThreshold, the Runner self-stops and hands control over
// to a regular liveness Runner with the long-tail Period (M1.2).
//
// All state transitions are observable via the Events channel so
// the controller can audit-log every probe outcome without polling
// the Runner — useful for `vd describe pod` to show the recent
// probe history.

package probe

import (
	"context"
	"sync"
	"time"
)

// Phase signals what state-change the Runner just experienced. The
// Phase channel emits one of these every time the Runner's
// consecutive-counter crosses a threshold (or on the very first
// sample, so callers can latch the initial state without polling).
type Phase int

const (
	// PhaseUnknown is the synthetic state before the first sample.
	// Callers initializing pod-ready state should NOT use this as
	// "not ready" — wait for PhaseHealthy or PhaseUnhealthy.
	PhaseUnknown Phase = iota

	// PhaseHealthy is the steady-state "probes passing". For
	// liveness this is the default; for readiness this means
	// "include in ingress upstream".
	PhaseHealthy

	// PhaseUnhealthy is the steady-state "probes failing past
	// FailureThreshold". The OnTrigger callback fires on the
	// transition into this phase (not on every subsequent fail
	// while still unhealthy — that would amplify the noise).
	PhaseUnhealthy
)

// String makes phase strings human-readable in logs.
func (p Phase) String() string {
	switch p {
	case PhaseHealthy:
		return "healthy"
	case PhaseUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// Event is what the Runner emits on every sample. The Phase field
// reflects the Runner's state AFTER processing this sample's Result.
// Callers can subscribe to all events (debug visibility) or just
// watch for phase transitions (alerting).
type Event struct {
	// Phase is the Runner's state after this sample. Steady-state
	// emissions repeat the same Phase; transitions show up as a
	// value differing from the previous Event's Phase.
	Phase Phase

	// Result is the raw sample. Surface in `vd describe pod` so
	// operators see WHY the runner flipped (e.g., "GET /healthz
	// → 502").
	Result Result

	// Transition is true when this Event represents a Phase change
	// from the previous one. Callers that only care about edges
	// (alerting, OnTrigger calls) filter on this.
	Transition bool
}

// Runner samples one probe spec against one container.
//
// Lifecycle:
//
//	r := New(spec, "voodu-x-web.a3f9", "172.18.0.7", deps)
//	go r.Run(ctx)
//	<-r.Stopped()  // or cancel ctx
//
// One Runner = one goroutine. Multiple probes against the same
// container = multiple Runners (e.g., liveness + readiness =
// two Runners). They share nothing.
type Runner struct {
	spec      Spec
	container string
	host      string
	execs     executors

	// onTrigger fires on the transition into PhaseUnhealthy (for
	// liveness probes: this is where "restart container" runs).
	// Optional — readiness probes typically read from Events channel
	// instead.
	onTrigger func(Result)

	// events is buffered (16) so a slow consumer can't block the
	// Runner. Production consumers (the audit logger) drain
	// continuously; if they fall further behind we drop the
	// oldest events rather than wedge the probe loop.
	events chan Event

	// done is closed when Run returns. Lets callers wait for clean
	// shutdown after cancelling the context.
	done chan struct{}

	// Internal counters — only touched from the Run goroutine.
	consecFail  int
	consecOK    int
	phase       Phase
	stopOnReady bool // true for startup probes: stop on first PhaseHealthy

	mu        sync.Mutex
	lastPhase Phase
}

// New constructs a Runner. Caller-supplied dependencies (HTTPGetter,
// TCPDialer, ExecRunner) get bundled via Options; nil values fall
// back to production defaults (net/http, net.Dial, no exec → exec
// probes always fail without one).
//
// container is the docker container name the Runner reports against
// (used by OnTrigger callbacks); host is what the runner connects
// to for HTTP/TCP probes (typically the container's IP on voodu0).
// For exec probes, host is unused.
func New(spec Spec, container, host string, opts Options) *Runner {
	spec.applyDefaults()

	r := &Runner{
		spec:        spec,
		container:   container,
		host:        host,
		stopOnReady: opts.StopOnReady,
		onTrigger:   opts.OnTrigger,
		execs: executors{
			http: opts.HTTP,
			tcp:  opts.TCP,
			exec: opts.Exec,
		},
		events: make(chan Event, 16),
		done:   make(chan struct{}),
		phase:  PhaseUnknown,
	}

	if r.execs.http == nil {
		r.execs.http = defaultHTTPGetter{}
	}

	if r.execs.tcp == nil {
		r.execs.tcp = defaultTCPDialer{}
	}
	// execs.exec stays nil if not provided — exec probes will fail
	// with a clear error rather than crash the runner.

	return r
}

// Options bundles the optional dependencies + callbacks for a Runner.
// Keeping them off the constructor argument list keeps tests readable
// (only override what you care about) and prod wiring simple.
type Options struct {
	// HTTP overrides the default net/http probe executor. Nil → use
	// the package default.
	HTTP HTTPGetter

	// TCP overrides the default net.Dial executor. Nil → use the
	// package default.
	TCP TCPDialer

	// Exec is the docker-exec hook. No default — leaving nil means
	// exec probes always fail with "no exec runner configured".
	// Production wires DockerContainerManager.Exec; tests inject
	// fakes.
	Exec ExecRunner

	// OnTrigger fires when the Runner transitions into PhaseUnhealthy.
	// Receives the Result that pushed it over the failure threshold.
	// Nil → no callback (caller observes via Events channel instead).
	//
	// IMPORTANT: OnTrigger runs in the Runner's goroutine. Long-
	// running callbacks (a docker restart that blocks for seconds)
	// will delay the next probe sample. Callers that do heavy work
	// should kick off a goroutine inside the callback.
	OnTrigger func(Result)

	// StopOnReady is true for startup probes: the Runner self-stops
	// (closes done) on the first transition into PhaseHealthy, and
	// the caller is expected to then start a regular liveness
	// Runner with a longer Period. Default false (steady-state
	// probes run forever).
	StopOnReady bool
}

// Run is the main loop. Blocks until the context is cancelled
// (or, when StopOnReady, until the first PhaseHealthy transition).
// One sample at a time — no concurrent probes against the same
// container.
func (r *Runner) Run(ctx context.Context) {
	defer close(r.done)
	defer close(r.events)

	// InitialDelay applies BEFORE the first sample. Boot-time grace
	// period: Rails apps that take 15s to start shouldn't be marked
	// unhealthy at second 1.
	if r.spec.InitialDelay > 0 {
		select {
		case <-time.After(r.spec.InitialDelay):
		case <-ctx.Done():
			return
		}
	}

	// First sample fires immediately after InitialDelay, then we
	// space at Period. Same shape as kubelet.
	r.sample(ctx)

	if r.shouldStop() {
		return
	}

	ticker := time.NewTicker(r.spec.Period)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sample(ctx)

			if r.shouldStop() {
				return
			}
		}
	}
}

// sample runs one probe attempt, updates counters, and emits an
// Event. Calls OnTrigger on the transition into PhaseUnhealthy.
func (r *Runner) sample(ctx context.Context) {
	result := execute(ctx, r.spec, r.container, r.host, r.execs)

	prevPhase := r.phase

	if result.OK {
		r.consecFail = 0
		r.consecOK++

		// Promote to healthy on the SuccessThreshold-th consecutive
		// success. Threshold of 1 (default) promotes immediately.
		if r.consecOK >= r.spec.SuccessThreshold {
			r.phase = PhaseHealthy
		}
	} else {
		r.consecOK = 0
		r.consecFail++

		// Demote to unhealthy on the FailureThreshold-th consecutive
		// fail. Threshold of 3 (default) gives the container two
		// "freebies" before we count it as down.
		if r.consecFail >= r.spec.FailureThreshold {
			r.phase = PhaseUnhealthy
		}
	}

	transition := r.phase != prevPhase && r.phase != PhaseUnknown

	r.mu.Lock()
	r.lastPhase = r.phase
	r.mu.Unlock()

	// Best-effort event emission. We drop the oldest if the buffer
	// fills (the consumer is slow) rather than block the probe loop
	// — a slow audit logger should never stall liveness detection.
	select {
	case r.events <- Event{Phase: r.phase, Result: result, Transition: transition}:
	default:
		// Drain one to make room, then push. If even that doesn't
		// fit (highly concurrent consumer), drop the event entirely.
		select {
		case <-r.events:
		default:
		}

		select {
		case r.events <- Event{Phase: r.phase, Result: result, Transition: transition}:
		default:
		}
	}

	// OnTrigger fires only on the transition INTO unhealthy.
	// Repeated unhealthy samples don't re-fire — caller asked for
	// "fix the container" once; spamming the docker daemon with
	// restart commands while it's still restarting is anti-pattern.
	if transition && r.phase == PhaseUnhealthy && r.onTrigger != nil {
		r.onTrigger(result)
	}
}

// shouldStop returns true when the Runner should exit its loop —
// either because StopOnReady is set and we just hit PhaseHealthy
// (startup probe handoff), or because the context was cancelled
// elsewhere.
func (r *Runner) shouldStop() bool {
	return r.stopOnReady && r.phase == PhaseHealthy
}

// Events exposes the read-only side of the event channel. Callers
// consume in a loop:
//
//	for ev := range runner.Events() {
//	    // ...
//	}
//
// The channel closes when Run returns.
func (r *Runner) Events() <-chan Event {
	return r.events
}

// Stopped returns a channel closed when Run has fully exited. Use
// for clean shutdown: after cancelling the context, wait on
// Stopped to be sure no probe attempt is mid-flight.
func (r *Runner) Stopped() <-chan struct{} {
	return r.done
}

// Phase returns the Runner's current phase. Thread-safe (uses a
// mutex internally). Use this when polling is fine — for tighter
// integration (ingress updates on phase change), prefer Events.
func (r *Runner) Phase() Phase {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.lastPhase
}
