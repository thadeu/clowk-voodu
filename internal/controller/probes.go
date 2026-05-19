// probes.go is the controller-side glue between the manifest's
// probes block and the internal/probe package's runner. The
// registry owns one probe runner per (container, probe-type) tuple,
// starts them when a container is born, cancels them when the
// container is removed, and wires the OnTrigger callback for
// liveness-failure restarts.
//
// M1.1 scope: liveness only. Readiness + startup wire in M1.2 — the
// data path (wire spec, hash inclusion) is already in place; M1.2
// adds the runners + ingress integration.
//
// Concurrency model:
//
//   - One ProbeRegistry per controller (singleton, lives on
//     DeploymentHandler).
//   - sync.Map keyed by docker container name; values are the cancel
//     funcs for that container's runners.
//   - Start is idempotent — calling on an already-running entry is
//     a no-op so reconciles can replay without spawning duplicates.
//   - Stop cancels the runners and waits for them to drain so the
//     next Start sees a clean slate.

package controller

import (
	"context"
	"log"
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

	// Log carries the registry's own structured log lines (probe
	// triggered, restart called, etc.). Nil → falls back to
	// log.Default at Start time.
	Log *log.Logger

	// runners maps container name → cancel-and-wait entry.
	runners sync.Map // map[string]*runnerEntry
}

// runnerEntry is the per-container bookkeeping. cancel stops the
// runner; done is closed when the runner's goroutine has fully
// exited (used so Stop is synchronous and Start can rely on a clean
// slate).
type runnerEntry struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

// Start spawns probe runners for the given container according to
// the spec. Idempotent — calling on a container that already has
// runners is a no-op (the existing runners keep going).
//
// Returns no error: missing dependencies (no IP, no exec, malformed
// spec) are surfaced as the runner's Result reasons, not as a Start
// failure. The runner stays up emitting failed Events so the
// operator can see WHY the probe isn't passing — silent skip would
// hide misconfiguration.
//
// nilSpec is treated as "no probes for this container" — cleanly
// no-op so callers can wire Start unconditionally.
func (r *ProbeRegistry) Start(containerName string, spec *probesWireSpec) {
	if r == nil || spec == nil {
		return
	}

	// Skip if already running for this container.
	if _, ok := r.runners.Load(containerName); ok {
		return
	}

	// M1.1: only Liveness is wired. Readiness/Startup are decoded
	// into the wire spec already but their runners land in M1.2.
	if spec.Liveness == nil {
		return
	}

	host := ""
	if r.IPs != nil {
		ip, err := r.IPs.ContainerIP(containerName)
		if err == nil {
			host = ip
		}
		// On lookup failure we still spawn the runner — it'll emit
		// failed probes with a clean reason ("dial: no such host"),
		// which is more debuggable than a silent skip.
	}

	probeSpec := wireToProbeSpec(spec.Liveness)

	ctx, cancel := context.WithCancel(context.Background())

	logger := r.Log
	if logger == nil {
		logger = log.Default()
	}

	runner := probe.New(probeSpec, containerName, host, probe.Options{
		Exec: r.execAdapter(containerName),
		OnTrigger: func(res probe.Result) {
			// Run the restart in its own goroutine — docker restart
			// can take 10s+ under load and we don't want to delay
			// the next probe sample by that long. The probe Runner
			// suppresses re-firing OnTrigger while already
			// unhealthy, so two restart calls in rapid succession
			// can only happen after a transition back to healthy
			// and then back to unhealthy — exactly the right
			// signal.
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

	done := runner.Stopped()

	go runner.Run(ctx)

	// Drain the events channel in a sibling goroutine so the
	// runner's buffered emission doesn't fill up. We don't act on
	// the events here (M1.2 will surface them via /pods/{name}
	// for `vd describe`); just drain so the runner stays
	// unblocked.
	go func() {
		for range runner.Events() {
		}
	}()

	r.runners.Store(containerName, &runnerEntry{cancel: cancel, done: done})
}

// Stop cancels the runner(s) for the named container and blocks
// until the runner goroutine has exited. Idempotent — calling Stop
// on a container that has no runners is a no-op.
//
// The synchronous wait is intentional: the caller (typically
// DeploymentHandler before docker remove) wants to know the
// runner is GONE before tearing down the container, so the runner
// can't make spurious restart calls against a container in the
// middle of being deleted.
func (r *ProbeRegistry) Stop(containerName string) {
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

	entry.cancel()

	// Bounded wait — a runner with a 1s probe timeout should exit
	// within one tick. Beyond a few seconds is a bug; we log and
	// move on rather than block reconcile indefinitely.
	select {
	case <-entry.done:
	case <-time.After(5 * time.Second):
		if r.Log != nil {
			r.Log.Printf("probe/%s runner did not stop within 5s — leaking goroutine", containerName)
		}
	}
}

// StopAll cancels every runner the registry knows about. Used on
// controller shutdown — leave no goroutines behind.
func (r *ProbeRegistry) StopAll() {
	if r == nil {
		return
	}

	r.runners.Range(func(key, _ any) bool {
		name, _ := key.(string)
		r.Stop(name)
		return true
	})
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
