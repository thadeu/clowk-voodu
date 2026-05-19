// Runner tests pin the threshold tracking, phase transitions,
// OnTrigger semantics, and StopOnReady behaviour. Every test uses
// a fake HTTP/TCP/Exec so no network or container is involved —
// the suite runs in milliseconds.
//
// Key invariants:
//   - Consecutive-failure counter resets on any success, and vice versa.
//   - PhaseUnhealthy fires once on the threshold-crossing sample;
//     subsequent failed samples emit Events but don't re-fire OnTrigger.
//   - InitialDelay is respected before the FIRST sample.
//   - StopOnReady=true → runner self-stops on PhaseHealthy.

package probe

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedHTTP returns a sequence of canned (status, err) per call.
// Bounds-checked: if the test calls more times than scripted, returns
// the last entry.
type scriptedHTTP struct {
	mu     sync.Mutex
	script []struct {
		status int
		err    error
	}
	called int32
}

func (s *scriptedHTTP) Get(ctx context.Context, url string, headers map[string]string) (int, error) {
	atomic.AddInt32(&s.called, 1)

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.script) == 0 {
		return 200, nil
	}

	idx := int(atomic.LoadInt32(&s.called)) - 1
	if idx >= len(s.script) {
		idx = len(s.script) - 1
	}

	return s.script[idx].status, s.script[idx].err
}

// scriptedHTTPFromStatuses builds a scriptedHTTP whose responses
// cycle through the given status codes (no err). Convenience for the
// common "succeed then fail" test patterns.
func scriptedHTTPFromStatuses(statuses ...int) *scriptedHTTP {
	s := &scriptedHTTP{}
	for _, st := range statuses {
		s.script = append(s.script, struct {
			status int
			err    error
		}{st, nil})
	}

	return s
}

// drainUntil collects events from the runner until n events have
// arrived OR the deadline expires. Returns whatever it has at that
// point; the test asserts on the slice.
func drainUntil(t *testing.T, r *Runner, n int, deadline time.Duration) []Event {
	t.Helper()

	out := make([]Event, 0, n)
	timer := time.NewTimer(deadline)
	defer timer.Stop()

	for len(out) < n {
		select {
		case ev, ok := <-r.Events():
			if !ok {
				return out
			}

			out = append(out, ev)
		case <-timer.C:
			return out
		}
	}

	return out
}

// makeRunner is a small constructor with sensible test defaults: tight
// period so tests don't have to wait, low thresholds so transitions
// land in a few samples.
func makeRunner(http HTTPGetter, opts Options) *Runner {
	spec := Spec{
		Action:           Action{HTTPGet: &HTTPGetAction{Path: "/healthz", Port: 8080}},
		Period:           5 * time.Millisecond,
		Timeout:          50 * time.Millisecond,
		FailureThreshold: 3,
		SuccessThreshold: 1,
	}

	opts.HTTP = http

	return New(spec, "voodu-x-web.a3f9", "127.0.0.1", opts)
}

// TestRunner_HealthyAfterFirstSuccess: success_threshold=1 → first
// successful sample transitions to PhaseHealthy. Pins the happy path.
func TestRunner_HealthyAfterFirstSuccess(t *testing.T) {
	http := scriptedHTTPFromStatuses(200, 200, 200)

	r := makeRunner(http, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go r.Run(ctx)

	events := drainUntil(t, r, 2, time.Second)
	cancel()
	<-r.Stopped()

	if len(events) < 1 {
		t.Fatalf("got %d events", len(events))
	}

	if events[0].Phase != PhaseHealthy {
		t.Errorf("first event phase: %s want healthy", events[0].Phase)
	}

	if !events[0].Transition {
		t.Error("first transition (unknown→healthy) should be flagged")
	}
}

// TestRunner_UnhealthyAfterFailureThreshold: 3 consecutive 500s on
// failure_threshold=3 → transition to PhaseUnhealthy AND OnTrigger
// fires exactly once.
func TestRunner_UnhealthyAfterFailureThreshold(t *testing.T) {
	http := scriptedHTTPFromStatuses(500, 500, 500, 500, 500)

	var triggers int32

	r := makeRunner(http, Options{
		OnTrigger: func(Result) { atomic.AddInt32(&triggers, 1) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go r.Run(ctx)

	// Wait for enough samples to cross the threshold + a margin.
	events := drainUntil(t, r, 5, time.Second)
	cancel()
	<-r.Stopped()

	// First 2 samples: still PhaseUnknown (consecFail = 1, 2). On the
	// 3rd, PhaseUnhealthy + transition. Subsequent samples stay in
	// PhaseUnhealthy, no re-transition.
	var transitions int

	for i, ev := range events {
		if ev.Transition {
			transitions++
		}

		// Sample i+1: after 3 fails, we MUST be unhealthy.
		if i >= 2 && ev.Phase != PhaseUnhealthy {
			t.Errorf("event[%d].Phase = %s, want unhealthy (after %d fails)", i, ev.Phase, i+1)
		}
	}

	if transitions != 1 {
		t.Errorf("expected exactly 1 phase transition, got %d", transitions)
	}

	got := atomic.LoadInt32(&triggers)
	if got != 1 {
		t.Errorf("expected OnTrigger exactly once, got %d", got)
	}
}

// TestRunner_CounterResetsOnSuccess: a single success in the middle
// of a fail streak resets the failure counter — we don't accumulate.
func TestRunner_CounterResetsOnSuccess(t *testing.T) {
	// fail, fail, OK, fail, fail → never reaches 3 consecutive fails
	http := scriptedHTTPFromStatuses(500, 500, 200, 500, 500, 500)

	var triggers int32

	r := makeRunner(http, Options{
		OnTrigger: func(Result) { atomic.AddInt32(&triggers, 1) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go r.Run(ctx)

	events := drainUntil(t, r, 5, time.Second)
	cancel()
	<-r.Stopped()

	// After the success, consecFail resets. Next two fails are a
	// new streak — only 2 deep when we cut off at 5 samples.
	if got := atomic.LoadInt32(&triggers); got != 0 {
		t.Errorf("OnTrigger should NOT have fired (streak interrupted): got %d", got)
	}

	// We should still have seen some events, just no trigger.
	if len(events) < 4 {
		t.Errorf("expected at least 4 events, got %d", len(events))
	}
}

// TestRunner_OnTriggerFiresOnce: even with many consecutive failures
// after the threshold, OnTrigger fires only on the TRANSITION. This is
// the "don't spam docker restart" invariant.
func TestRunner_OnTriggerFiresOnce(t *testing.T) {
	// 10 consecutive failures
	statuses := make([]int, 10)
	for i := range statuses {
		statuses[i] = 500
	}

	http := scriptedHTTPFromStatuses(statuses...)

	var triggers int32

	r := makeRunner(http, Options{
		OnTrigger: func(Result) { atomic.AddInt32(&triggers, 1) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go r.Run(ctx)

	// Drain plenty of events.
	_ = drainUntil(t, r, 8, time.Second)
	cancel()
	<-r.Stopped()

	if got := atomic.LoadInt32(&triggers); got != 1 {
		t.Errorf("OnTrigger should fire exactly once on transition, got %d", got)
	}
}

// TestRunner_InitialDelay: the FIRST sample doesn't fire until after
// InitialDelay. Boot-time grace for slow apps.
func TestRunner_InitialDelay(t *testing.T) {
	http := scriptedHTTPFromStatuses(200, 200, 200)

	spec := Spec{
		Action:           Action{HTTPGet: &HTTPGetAction{Path: "/", Port: 80}},
		InitialDelay:     50 * time.Millisecond,
		Period:           5 * time.Millisecond,
		Timeout:          50 * time.Millisecond,
		FailureThreshold: 3,
		SuccessThreshold: 1,
	}

	r := New(spec, "container", "127.0.0.1", Options{HTTP: http})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	go r.Run(ctx)

	// During the first 30ms, no event should arrive yet (initial
	// delay is 50ms).
	select {
	case ev := <-r.Events():
		t.Errorf("event arrived before InitialDelay: %+v", ev)
	case <-time.After(30 * time.Millisecond):
		// OK — no event during the early window.
	}

	// After waiting past the delay, events do arrive.
	select {
	case <-r.Events():
		// good
	case <-time.After(200 * time.Millisecond):
		t.Error("no event after InitialDelay")
	}

	cancel()
	<-r.Stopped()
}

// TestRunner_StopOnReady: a startup probe (StopOnReady=true) exits
// the loop on the first PhaseHealthy. Caller is then expected to
// hand off to a regular liveness Runner.
func TestRunner_StopOnReady(t *testing.T) {
	// Two fails (insufficient to trigger), then a success that
	// flips to healthy — runner should stop.
	http := scriptedHTTPFromStatuses(500, 500, 200, 200, 200)

	spec := Spec{
		Action:           Action{HTTPGet: &HTTPGetAction{Path: "/", Port: 80}},
		Period:           5 * time.Millisecond,
		Timeout:          50 * time.Millisecond,
		FailureThreshold: 30, // very high so we don't accidentally trigger unhealthy
		SuccessThreshold: 1,
	}

	r := New(spec, "container", "127.0.0.1", Options{HTTP: http, StopOnReady: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go r.Run(ctx)

	select {
	case <-r.Stopped():
		// good — runner exited on its own
	case <-time.After(time.Second):
		t.Fatal("StopOnReady runner did not stop within 1s")
	}

	// Final phase should be healthy.
	if r.Phase() != PhaseHealthy {
		t.Errorf("final phase: %s, want healthy", r.Phase())
	}

	cancel()
}

// TestRunner_SuccessThresholdGreaterThanOne: when SuccessThreshold=3,
// the runner stays unknown/unhealthy until 3 consecutive successes.
// Pins the readiness use case where flapping shouldn't gate the pod
// in / out repeatedly.
func TestRunner_SuccessThresholdGreaterThanOne(t *testing.T) {
	http := scriptedHTTPFromStatuses(200, 200, 200, 200)

	spec := Spec{
		Action:           Action{HTTPGet: &HTTPGetAction{Path: "/", Port: 80}},
		Period:           5 * time.Millisecond,
		Timeout:          50 * time.Millisecond,
		FailureThreshold: 3,
		SuccessThreshold: 3,
	}

	r := New(spec, "c", "127.0.0.1", Options{HTTP: http})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go r.Run(ctx)

	events := drainUntil(t, r, 4, time.Second)
	cancel()
	<-r.Stopped()

	if len(events) < 3 {
		t.Fatalf("got %d events", len(events))
	}

	// Sample 1, 2: PhaseUnknown (haven't hit success threshold yet).
	if events[0].Phase != PhaseUnknown || events[1].Phase != PhaseUnknown {
		t.Errorf("first 2 phases: %s, %s — should be unknown", events[0].Phase, events[1].Phase)
	}

	// Sample 3: cross success threshold, transition to healthy.
	if events[2].Phase != PhaseHealthy {
		t.Errorf("3rd phase: %s, want healthy", events[2].Phase)
	}

	if !events[2].Transition {
		t.Error("3rd event must mark transition unknown→healthy")
	}
}

// TestRunner_PhaseObservable: Phase() returns the current phase
// concurrent with the Run goroutine; mutex protects from races.
func TestRunner_PhaseObservable(t *testing.T) {
	http := scriptedHTTPFromStatuses(200, 200, 200)

	r := makeRunner(http, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go r.Run(ctx)

	// Drain a few events to ensure samples have happened.
	_ = drainUntil(t, r, 2, time.Second)

	if got := r.Phase(); got != PhaseHealthy {
		t.Errorf("Phase()=%s, want healthy", got)
	}

	cancel()
	<-r.Stopped()
}

// TestRunner_ExecProbeWithoutRunnerFails: leaving Options.Exec nil
// while configuring an exec probe must NOT crash — it should produce
// failed Results. Defensive shape: production never has this combo,
// but a test typo shouldn't panic.
func TestRunner_ExecProbeWithoutRunnerFails(t *testing.T) {
	spec := Spec{
		Action: Action{Exec: &ExecAction{Command: []string{"true"}}},
		Period: 5 * time.Millisecond,
	}

	r := New(spec, "c", "", Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go r.Run(ctx)

	select {
	case ev := <-r.Events():
		if ev.Result.OK {
			t.Error("exec probe with nil ExecRunner should fail")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no event arrived")
	}

	cancel()
	<-r.Stopped()
}

// fakeExecCounting is an exec runner that returns a fixed exit code
// and records call count. Used for runner-level exec tests.
type fakeExecCounting struct {
	code    int
	stderr  string
	err     error
	calls   int32
	gotCmd  [][]string
	mu      sync.Mutex
}

func (f *fakeExecCounting) Exec(ctx context.Context, container string, cmd []string) (int, string, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.gotCmd = append(f.gotCmd, cmd)
	f.mu.Unlock()
	return f.code, f.stderr, f.err
}

// TestRunner_ExecProbeRunsCommand pins the exec wiring end-to-end at
// the Runner level — the fakeExec gets called with the right command,
// and the exit code translates to success/failure correctly.
func TestRunner_ExecProbeRunsCommand(t *testing.T) {
	exec := &fakeExecCounting{code: 0}

	spec := Spec{
		Action:           Action{Exec: &ExecAction{Command: []string{"pg_isready", "-q"}}},
		Period:           5 * time.Millisecond,
		Timeout:          50 * time.Millisecond,
		SuccessThreshold: 1,
	}

	r := New(spec, "voodu-pg.0", "", Options{Exec: exec})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go r.Run(ctx)

	_ = drainUntil(t, r, 2, time.Second)
	cancel()
	<-r.Stopped()

	if atomic.LoadInt32(&exec.calls) == 0 {
		t.Fatal("ExecRunner not called")
	}

	exec.mu.Lock()
	cmd := exec.gotCmd[0]
	exec.mu.Unlock()

	if len(cmd) != 2 || cmd[0] != "pg_isready" || cmd[1] != "-q" {
		t.Errorf("command: %v", cmd)
	}
}

// TestRunner_HTTPNetworkErrorIsFailure pins a net.OpError-style err
// path: even when the fake HTTP returns an error (no status code), it
// counts as failure and contributes to the threshold.
func TestRunner_HTTPNetworkErrorIsFailure(t *testing.T) {
	http := &scriptedHTTP{script: []struct {
		status int
		err    error
	}{
		{0, errors.New("connection refused")},
		{0, errors.New("connection refused")},
		{0, errors.New("connection refused")},
		{0, errors.New("connection refused")},
	}}

	var triggers int32

	r := makeRunner(http, Options{
		OnTrigger: func(Result) { atomic.AddInt32(&triggers, 1) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go r.Run(ctx)

	_ = drainUntil(t, r, 4, time.Second)
	cancel()
	<-r.Stopped()

	if atomic.LoadInt32(&triggers) != 1 {
		t.Errorf("network errors should cross threshold: %d triggers", triggers)
	}
}
