// Tests for the controller-side ProbeRegistry — the seam between
// manifest probe specs and the internal/probe package's runners.
// Verifies Start/Stop lifecycle, OnTrigger → restart wiring, and
// the wire→probe spec conversion.

package controller

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/probe"
)

// fakeContainerRestarter records every container name passed to it.
// Tests assert "the right container got restarted" and "the wrong
// one didn't" through this. Named -Container to avoid clashing with
// api_restart_test.go's fakeRestarter (which stubs the higher-level
// DeploymentRestarter — different domain, same convenient name).
type fakeContainerRestarter struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeContainerRestarter) RestartContainer(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, name)

	return f.err
}

func (f *fakeContainerRestarter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.calls)
}

// fakeIPResolver returns canned IP per container name.
type fakeIPResolver struct {
	ips map[string]string
}

func (f fakeIPResolver) ContainerIP(name string) (string, error) {
	if ip, ok := f.ips[name]; ok {
		return ip, nil
	}

	return "", errors.New("unknown container")
}

// fakeProbeExec satisfies the ProbeExecutor interface — fixed exit
// code per call. Records the command for assertion.
type fakeProbeExec struct {
	mu      sync.Mutex
	code    int
	stderr  string
	err     error
	called  int32
	gotCmds [][]string
}

func (f *fakeProbeExec) Exec(name string, command []string, opts ExecOptions) (int, error) {
	atomic.AddInt32(&f.called, 1)

	f.mu.Lock()
	f.gotCmds = append(f.gotCmds, command)
	f.mu.Unlock()

	if opts.Stderr != nil && f.stderr != "" {
		_, _ = io.WriteString(opts.Stderr, f.stderr)
	}

	return f.code, f.err
}

// TestProbeRegistry_StartIdempotent pins the no-duplicate-runner
// rule: calling Start twice on the same container leaves exactly
// one runner. The reconciler replays manifests on every event,
// so without this idempotency a busy deployment would accumulate
// dozens of parallel runners per container.
func TestProbeRegistry_StartIdempotent(t *testing.T) {
	r := &ProbeRegistry{}

	spec := &probesWireSpec{
		Liveness: &probeWireSpec{
			TCPSocket: &tcpSocketActionWire{Port: 80},
			Period:    "1s",
		},
	}

	r.Start("app", "x", spec)
	r.Start("app", "x", spec)
	r.Start("app", "x", spec)

	count := 0
	r.runners.Range(func(_, _ any) bool {
		count++
		return true
	})

	if count != 1 {
		t.Errorf("got %d runners, want 1 (Start should be idempotent)", count)
	}

	r.Stop("app", "x")
}

// TestProbeRegistry_NilSpecNoOps covers the common case: most
// deployments don't declare probes. Start with nil spec should
// register no runner and emit no calls.
func TestProbeRegistry_NilSpecNoOps(t *testing.T) {
	restarter := &fakeContainerRestarter{}
	r := &ProbeRegistry{Restart: restarter}

	r.Start("app", "x", nil)
	r.Start("app", "y", &probesWireSpec{}) // probes block but no liveness inside

	if restarter.callCount() != 0 {
		t.Errorf("nil spec must not trigger any restart, got %d calls", restarter.callCount())
	}

	count := 0
	r.runners.Range(func(_, _ any) bool {
		count++
		return true
	})

	if count != 0 {
		t.Errorf("nil/empty spec must not register a runner, got %d", count)
	}
}

// TestProbeRegistry_StopCancelsRunner verifies Stop synchronously
// drains the runner. After Stop returns, no further probe samples
// can fire — important because the typical caller pattern is
// Stop → docker remove, and a runner that's still ticking would
// fire failed probes against a missing container.
func TestProbeRegistry_StopCancelsRunner(t *testing.T) {
	r := &ProbeRegistry{
		IPs: fakeIPResolver{ips: map[string]string{"x": "127.0.0.1"}},
	}

	spec := &probesWireSpec{
		Liveness: &probeWireSpec{
			TCPSocket: &tcpSocketActionWire{Port: 1}, // port 1 — unlikely to be open
			Period:    "100ms",
			Timeout:   "10ms",
		},
	}

	r.Start("app", "x", spec)

	// Give it a moment to make a few samples.
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})

	go func() {
		r.Stop("app", "x")
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(time.Second):
		t.Fatal("Stop did not return within 1s")
	}

	count := 0
	r.runners.Range(func(_, _ any) bool {
		count++
		return true
	})

	if count != 0 {
		t.Errorf("Stop should remove the runner; %d remain", count)
	}
}

// TestProbeRegistry_StopUnknownContainerIsNoOp pins the defensive
// idempotency: calling Stop on a container that never had a
// runner is a clean no-op. Lets the handler always-call-Stop on
// every container it removes without bookkeeping which ones it
// started.
func TestProbeRegistry_StopUnknownContainerIsNoOp(t *testing.T) {
	r := &ProbeRegistry{}

	// Just shouldn't panic.
	r.Stop("app", "never-started")
	r.Stop("app", "")
}

// TestProbeRegistry_LivenessFailureRestartsContainer is the core
// integration: a liveness probe that fails its threshold triggers
// a docker restart call. Uses TCP to a closed port so we don't
// need a real HTTP fake — connect failure is deterministic.
func TestProbeRegistry_LivenessFailureRestartsContainer(t *testing.T) {
	restarter := &fakeContainerRestarter{}

	r := &ProbeRegistry{
		Restart: restarter,
		IPs:     fakeIPResolver{ips: map[string]string{"voodu-x.a1": "127.0.0.1"}},
	}

	spec := &probesWireSpec{
		Liveness: &probeWireSpec{
			TCPSocket:        &tcpSocketActionWire{Port: 1}, // closed
			Period:           "20ms",
			Timeout:          "10ms",
			FailureThreshold: 2,
		},
	}

	r.Start("voodu-x", "voodu-x.a1", spec)

	// 2 failures × 20ms = ~40ms. Give 500ms slack for CI jitter.
	deadline := time.After(500 * time.Millisecond)
	for {
		if restarter.callCount() >= 1 {
			break
		}

		select {
		case <-deadline:
			t.Fatalf("expected at least 1 restart call within 500ms, got %d", restarter.callCount())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if restarter.calls[0] != "voodu-x.a1" {
		t.Errorf("restarted wrong container: %q", restarter.calls[0])
	}

	r.Stop("voodu-x", "voodu-x.a1")
}

// TestWireToProbeSpec covers the wire → probe.Spec conversion —
// each field maps to the right place, durations parse, missing
// values stay at zero (probe package promotes to defaults).
func TestWireToProbeSpec(t *testing.T) {
	wire := &probeWireSpec{
		HTTPGet: &httpGetActionWire{
			Path:        "/healthz",
			Port:        8080,
			Scheme:      "https",
			HTTPHeaders: map[string]string{"X-Probe": "voodu"},
		},
		InitialDelay:     "10s",
		Period:           "5s",
		Timeout:          "2s",
		FailureThreshold: 5,
		SuccessThreshold: 2,
	}

	got := wireToProbeSpec(wire)

	if got.Action.HTTPGet == nil {
		t.Fatal("HTTPGet missing")
	}

	if got.Action.HTTPGet.Path != "/healthz" || got.Action.HTTPGet.Port != 8080 {
		t.Errorf("http_get fields lost: %+v", got.Action.HTTPGet)
	}

	if got.Action.HTTPGet.Scheme != "https" {
		t.Errorf("scheme: %q", got.Action.HTTPGet.Scheme)
	}

	if got.Action.HTTPGet.HTTPHeaders["X-Probe"] != "voodu" {
		t.Errorf("headers lost: %+v", got.Action.HTTPGet.HTTPHeaders)
	}

	if got.InitialDelay != 10*time.Second || got.Period != 5*time.Second || got.Timeout != 2*time.Second {
		t.Errorf("durations: initial=%v period=%v timeout=%v", got.InitialDelay, got.Period, got.Timeout)
	}

	if got.FailureThreshold != 5 || got.SuccessThreshold != 2 {
		t.Errorf("thresholds lost: fail=%d success=%d", got.FailureThreshold, got.SuccessThreshold)
	}
}

// TestParseProbeDuration covers tolerant duration parsing — empty
// and invalid strings return 0, valid strings parse cleanly.
func TestParseProbeDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"garbage", 0},
		{"10s", 10 * time.Second},
		{"1m30s", time.Minute + 30*time.Second},
		{"500ms", 500 * time.Millisecond},
	}

	for _, c := range cases {
		got := parseProbeDuration(c.in)
		if got != c.want {
			t.Errorf("parseProbeDuration(%q): got %v, want %v", c.in, got, c.want)
		}
	}
}

// fakeReadinessRecorder captures every RecordReplicaReadiness
// and ClearReplicaReadiness call so M1.2 tests can assert the
// debouncing (one persist per Phase transition, not per sample)
// and the per-replica clear on Stop.
type fakeReadinessRecorder struct {
	mu       sync.Mutex
	records  []ReplicaReadinessStatus
	cleared  []string
}

func (f *fakeReadinessRecorder) RecordReplicaReadiness(_ context.Context, _ string, s ReplicaReadinessStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, s)
}

func (f *fakeReadinessRecorder) ClearReplicaReadiness(_ context.Context, _ string, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, name)
}

func (f *fakeReadinessRecorder) recordCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

// TestProbeRegistry_NoProbes_NoOp pins the backward-compat
// guarantee: a deployment with zero probes declared never
// registers a runner, never persists state, never logs noise.
// This is the most common shape pre-M1, and any regression here
// would churn fleets.
func TestProbeRegistry_NoProbes_NoOp(t *testing.T) {
	rec := &fakeReadinessRecorder{}
	r := &ProbeRegistry{Recorder: rec}

	r.Start("app", "x", &probesWireSpec{}) // empty block — no sub-probes

	count := 0

	r.runners.Range(func(_, _ any) bool {
		count++
		return true
	})

	if count != 0 {
		t.Errorf("empty probes block must not register a runner, got %d", count)
	}

	if rec.recordCount() != 0 {
		t.Errorf("empty probes block must not record state, got %d", rec.recordCount())
	}
}

// TestProbeRegistry_ReadinessOnlyNoLiveness pins the "removed
// early-out" fix: in M1.1 the registry early-returned when
// liveness was nil — readiness alone would never spawn. M1.2
// must spawn a readiness runner even without liveness declared.
func TestProbeRegistry_ReadinessOnlyNoLiveness(t *testing.T) {
	rec := &fakeReadinessRecorder{}

	r := &ProbeRegistry{
		IPs:      fakeIPResolver{ips: map[string]string{"x": "127.0.0.1"}},
		Recorder: rec,
	}

	spec := &probesWireSpec{
		Readiness: &probeWireSpec{
			TCPSocket: &tcpSocketActionWire{Port: 1}, // closed
			Period:    "30ms",
			Timeout:   "10ms",
		},
	}

	r.Start("app", "x", spec)

	// Initial state push should arrive — verify a record landed.
	deadline := time.After(500 * time.Millisecond)
	for {
		if rec.recordCount() >= 1 {
			break
		}

		select {
		case <-deadline:
			t.Fatalf("no readiness record after 500ms (records=%d)", rec.recordCount())
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	r.Stop("app", "x")
}

// TestProbeRegistry_StartupGatesReadiness pins the gating rule:
// while a startup probe is declared and not yet passed, the
// replica counts as NOT ready, regardless of readiness phase.
// Once startup hits PhaseHealthy, the gate opens and readiness
// determines Ready.
func TestProbeRegistry_StartupGatesReadiness(t *testing.T) {
	rec := &fakeReadinessRecorder{}

	r := &ProbeRegistry{Recorder: rec}

	// Synthesise an entry with both startup + readiness declared,
	// startup not yet passed, readiness PhaseHealthy. The
	// aggregator must report Ready=false because the gate is
	// closed.
	state := &replicaReadiness{
		hasStartup:     true,
		hasReadiness:   true,
		startupPassed:  false,
		readinessPhase: probe.PhaseHealthy,
	}

	if state.ready() {
		t.Error("startup-not-passed + readiness-healthy must NOT be Ready")
	}

	// Open the gate; same readiness → Ready=true.
	state.startupPassed = true

	if !state.ready() {
		t.Error("startup-passed + readiness-healthy must be Ready")
	}

	// Independent: no probes at all → Ready=true (backward
	// compat path).
	noProbes := &replicaReadiness{}
	if !noProbes.ready() {
		t.Error("no probes declared must be Ready by default")
	}

	_ = r // keep r referenced — test exercises the pure aggregator
}

// TestProbeRegistry_LookupReadiness_Roundtrip verifies the
// in-memory fast path: snapshot a state, Start it via a fake
// minimal entry, LookupReadiness must return the same Ready
// status. The same code path serves the high-frequency caddy
// active health-check query.
func TestProbeRegistry_LookupReadiness_Roundtrip(t *testing.T) {
	r := &ProbeRegistry{}

	// Inject a synthetic runnerEntry directly. We're not
	// exercising the runner here — just the lookup surface.
	r.runners.Store("synthetic.a1", &runnerEntry{
		state: &replicaReadiness{
			hasStartup:     false,
			hasReadiness:   false,
			startupPassed:  true,
			readinessPhase: probe.PhaseHealthy,
		},
	})

	status, ok := r.LookupReadiness("synthetic.a1")
	if !ok {
		t.Fatal("LookupReadiness should find synthetic entry")
	}

	if !status.Ready {
		t.Error("synthetic entry with no probes must report Ready=true")
	}

	if status.ContainerName != "synthetic.a1" {
		t.Errorf("ContainerName=%q, want synthetic.a1", status.ContainerName)
	}

	if status.ReplicaID != "a1" {
		t.Errorf("ReplicaID=%q, want a1 (parsed from container name)", status.ReplicaID)
	}

	// Unknown name → (zero, false).
	_, ok = r.LookupReadiness("ghost")
	if ok {
		t.Error("LookupReadiness should return false for unknown container")
	}
}

// TestProbeRegistry_Stop_CallsClear verifies that Stop notifies
// the Recorder so describe doesn't show a ghost entry for a
// torn-down replica. Independent of probe spec — the clear
// fires whenever a runner had been spawned.
func TestProbeRegistry_Stop_CallsClear(t *testing.T) {
	rec := &fakeReadinessRecorder{}

	r := &ProbeRegistry{
		Recorder: rec,
		IPs:      fakeIPResolver{ips: map[string]string{"x": "127.0.0.1"}},
	}

	spec := &probesWireSpec{
		Liveness: &probeWireSpec{
			TCPSocket: &tcpSocketActionWire{Port: 1},
			Period:    "100ms",
			Timeout:   "10ms",
		},
	}

	r.Start("app", "x", spec)
	r.Stop("app", "x")

	// Drain any in-flight goroutine.
	time.Sleep(50 * time.Millisecond)

	rec.mu.Lock()
	gotCleared := len(rec.cleared) >= 1 && rec.cleared[0] == "x"
	rec.mu.Unlock()

	if !gotCleared {
		t.Errorf("Stop should ClearReplicaReadiness(x), got cleared=%v", rec.cleared)
	}
}

// TestReplicaIDFromContainerName pins the trailing-after-dot
// parser shape — matches containers.ContainerName's output for
// scoped and unscoped names.
func TestReplicaIDFromContainerName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"prod-api.a3f9", "a3f9"},
		{"api.a3f9", "a3f9"},
		{"prod-pg.0", "0"}, // statefulset ordinal shape
		{"nodot", "nodot"},
	}

	for _, c := range cases {
		got := replicaIDFromContainerName(c.in)
		if got != c.want {
			t.Errorf("replicaIDFromContainerName(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestCapturingWriter pins the truncation rule — once we've
// captured `limit` bytes, additional writes are accepted but
// silently dropped. We don't want a runaway stderr stream from
// a broken container filling the controller's memory.
func TestCapturingWriter(t *testing.T) {
	w := &capturingWriter{limit: 10}

	_, _ = w.Write([]byte("hello "))
	_, _ = w.Write([]byte("world this is too long"))

	got := w.String()

	if len(got) != 10 {
		t.Errorf("captured %d bytes, want exactly 10 (the limit)", len(got))
	}

	if got != "hello worl" {
		t.Errorf("captured content: %q, want first 10 bytes only", got)
	}
}
