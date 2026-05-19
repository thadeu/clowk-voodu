// Tests for the controller-side ProbeRegistry — the seam between
// manifest probe specs and the internal/probe package's runners.
// Verifies Start/Stop lifecycle, OnTrigger → restart wiring, and
// the wire→probe spec conversion.

package controller

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

	r.Start("x", spec)
	r.Start("x", spec)
	r.Start("x", spec)

	count := 0
	r.runners.Range(func(_, _ any) bool {
		count++
		return true
	})

	if count != 1 {
		t.Errorf("got %d runners, want 1 (Start should be idempotent)", count)
	}

	r.Stop("x")
}

// TestProbeRegistry_NilSpecNoOps covers the common case: most
// deployments don't declare probes. Start with nil spec should
// register no runner and emit no calls.
func TestProbeRegistry_NilSpecNoOps(t *testing.T) {
	restarter := &fakeContainerRestarter{}
	r := &ProbeRegistry{Restart: restarter}

	r.Start("x", nil)
	r.Start("y", &probesWireSpec{}) // probes block but no liveness inside

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

	r.Start("x", spec)

	// Give it a moment to make a few samples.
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})

	go func() {
		r.Stop("x")
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
	r.Stop("never-started")
	r.Stop("")
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

	r.Start("voodu-x.a1", spec)

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

	r.Stop("voodu-x.a1")
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
