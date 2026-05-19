// Webhook delivery tests for the on_deploy block. Two layers
// exercised:
//
//  1. postWithRetry — the standalone retry loop. Easier to assert
//     attempt count + error propagation against a fake than to
//     reach into apply() and disentangle a half-dozen unrelated
//     branches.
//
//  2. DeploymentHandler.apply integration — the success/failure
//     hooks actually fire when a rolling restart concludes. Uses
//     the existing fakeContainers + memStore setup; the only new
//     seam is the fakeWebhookPoster recording calls.
//
// Tests run with webhookBackoff overridden to zero so the retry
// loop doesn't actually wait 1+5+30 seconds per failure case.
// Production never modifies webhookBackoff.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWebhookPoster captures every Post call's arguments for
// assertion. Optionally injects errors per call index to drive
// the retry path (first N attempts fail, then succeed; or every
// attempt fails). Thread-safe because postWithRetry's goroutine
// posture means the test reads while the helper writes.
type fakeWebhookPoster struct {
	mu sync.Mutex

	calls []fakeWebhookCall

	// failures controls the per-call return: errors[i] is the
	// error to return on call i. nil/missing → success. Used to
	// simulate "fail twice then succeed" by setting [err, err]
	// and letting the third call default to nil/success.
	failures []error
}

type fakeWebhookCall struct {
	URL     string
	Payload WebhookPayload
}

func (f *fakeWebhookPoster) Post(ctx context.Context, url string, payload WebhookPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	idx := len(f.calls)
	f.calls = append(f.calls, fakeWebhookCall{URL: url, Payload: payload})

	if idx < len(f.failures) {
		return f.failures[idx]
	}

	return nil
}

func (f *fakeWebhookPoster) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.calls)
}

func (f *fakeWebhookPoster) lastCall() (fakeWebhookCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.calls) == 0 {
		return fakeWebhookCall{}, false
	}

	return f.calls[len(f.calls)-1], true
}

// withZeroWebhookBackoff swaps the package-level webhookBackoff
// for the duration of one test so retries don't wait real
// wall-clock time. Restored in a deferred closure.
func withZeroWebhookBackoff(t *testing.T) {
	t.Helper()

	prev := webhookBackoff
	webhookBackoff = []time.Duration{0, 0, 0}

	t.Cleanup(func() {
		webhookBackoff = prev
	})
}

// TestWebhook_PostedOnSuccess pins that a successful rolling
// restart (drift-driven recreate path) fires the success
// webhook with the operator's URL. Without this, the operator
// declares on_deploy.success and silently gets no signal — the
// regression mode is "feature looks wired but doesn't do
// anything", which is the worst kind of bug to catch in
// production.
func TestWebhook_PostedOnSuccess(t *testing.T) {
	withZeroWebhookBackoff(t)

	store := newMemStore()

	prevSpec := deploymentSpec{Image: "nginx:1.27"}
	prevHash := deploymentSpecHash(prevSpec, nil)
	pre, _ := json.Marshal(DeploymentStatus{Image: prevSpec.Image, SpecHash: prevHash})

	_ = store.PutStatus(context.Background(), KindDeployment, "test-api", pre)

	existing := deploymentSlot("test", "api", "nginx:1.27", "a001")

	cm := &fakeContainers{}
	cm.seedSlot(existing)

	poster := &fakeWebhookPoster{}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
		Webhooks:    poster,
	}

	// Spec drift (image change) triggers recreate → rolling restart.
	ev := putEvent(t, KindDeployment, "api", deploymentSpec{
		Image: "nginx:1.28",
		OnDeploy: &onDeployWireSpec{
			Success: "https://hooks.example.com/success",
			Failure: "https://hooks.example.com/failure",
		},
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// fireDeployWebhook posts in a goroutine — wait briefly for the
	// fire-and-forget delivery to complete. With zero backoff this
	// is essentially immediate.
	waitFor(t, func() bool { return poster.callCount() >= 1 })

	if got := poster.callCount(); got != 1 {
		t.Fatalf("want 1 webhook call, got %d", got)
	}

	call, _ := poster.lastCall()

	if call.URL != "https://hooks.example.com/success" {
		t.Errorf("URL: got %q, want success URL", call.URL)
	}

	if call.Payload.Status != "success" {
		t.Errorf("Status: got %q, want success", call.Payload.Status)
	}
}

// TestWebhook_PostedOnFailure asserts the failure URL fires when
// an early-return error path bubbles up. The deployment handler's
// apply() returns errors at several points (container manager
// failure, list failure, etc.); we drive one via fakeContainers
// returning a fatal error on ensure.
func TestWebhook_PostedOnFailure(t *testing.T) {
	withZeroWebhookBackoff(t)

	store := newMemStore()

	cm := &fakeContainers{ensureErr: errors.New("container manager exploded")}

	poster := &fakeWebhookPoster{}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
		Webhooks:    poster,
	}

	// First-apply path: ensureReplicaCount tries to spawn one
	// replica, the fake fails, the error bubbles up.
	ev := putEvent(t, KindDeployment, "api", deploymentSpec{
		Image: "nginx:1.27",
		OnDeploy: &onDeployWireSpec{
			Failure: "https://hooks.example.com/failure",
		},
	})

	if err := h.Handle(context.Background(), ev); err == nil {
		t.Fatal("expected error from handle, got nil")
	}

	waitFor(t, func() bool { return poster.callCount() >= 1 })

	if got := poster.callCount(); got != 1 {
		t.Fatalf("want 1 webhook call, got %d", got)
	}

	call, _ := poster.lastCall()

	if call.URL != "https://hooks.example.com/failure" {
		t.Errorf("URL: got %q, want failure URL", call.URL)
	}

	if call.Payload.Status != "failure" {
		t.Errorf("Status: got %q, want failure", call.Payload.Status)
	}

	if call.Payload.Error == "" {
		t.Error("Error: empty — failure payload must surface the cause")
	}
}

// TestWebhook_RetriesOnTransientFailure exercises the retry loop
// directly via postWithRetry — easier to reason about than going
// through apply() because the retry happens inside the
// fire-and-forget goroutine. Three failures with the third
// succeeding pins the 3-attempt budget (initial try + 2 retries =
// 3 calls before giving up).
func TestWebhook_RetriesOnTransientFailure(t *testing.T) {
	withZeroWebhookBackoff(t)

	transient := errors.New("503 service unavailable")

	poster := &fakeWebhookPoster{
		// Fail the first two attempts; the third succeeds.
		failures: []error{transient, transient},
	}

	err := postWithRetry(context.Background(), poster, "https://example/hook", WebhookPayload{Status: "success"}, func(time.Duration) {})
	if err != nil {
		t.Fatalf("postWithRetry: got %v, want nil after recovery", err)
	}

	if got := poster.callCount(); got != 3 {
		t.Errorf("call count: got %d, want 3 (initial + 2 retries)", got)
	}
}

// TestWebhook_GivesUpAfterMaxRetries asserts the retry loop
// stops at exactly 3 attempts even when every attempt fails, and
// that the returned error is the LAST attempt's error (operators
// debugging a webhook outage care about the most recent failure,
// not the first one). The handler's apply() ignores this error
// — the deploy must NEVER fail because the webhook side did.
func TestWebhook_GivesUpAfterMaxRetries(t *testing.T) {
	withZeroWebhookBackoff(t)

	persistent := errors.New("connection refused")

	poster := &fakeWebhookPoster{
		failures: []error{persistent, persistent, persistent, persistent, persistent},
	}

	err := postWithRetry(context.Background(), poster, "https://example/hook", WebhookPayload{Status: "success"}, func(time.Duration) {})
	if err == nil {
		t.Fatal("postWithRetry: got nil, want error after all retries exhausted")
	}

	if !errors.Is(err, persistent) {
		t.Errorf("returned error: got %v, want %v", err, persistent)
	}

	if got := poster.callCount(); got != 3 {
		t.Errorf("call count: got %d, want exactly 3 (initial + 2 retries, drop after)", got)
	}
}

// TestWebhook_PayloadShape locks the JSON wire contract. The
// payload field names and types are what operators' Slack/Discord
// formatting rules key off of — renaming `status` to `state`, or
// emitting `release_id` as an integer instead of a string, would
// silently break every existing webhook consumer. This test pins
// the canonical shape so any unintended rename fails CI loudly.
func TestWebhook_PayloadShape(t *testing.T) {
	withZeroWebhookBackoff(t)

	poster := &fakeWebhookPoster{}

	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	completed := started.Add(45 * time.Second)

	fireDeployWebhook(
		poster, nil,
		&onDeployWireSpec{Success: "https://example/hook"},
		"deployment", "prod", "api",
		"rel-42",
		"ghcr.io/me/api:v1",
		"success", "",
		started, completed,
	)

	waitFor(t, func() bool { return poster.callCount() >= 1 })

	call, _ := poster.lastCall()

	want := WebhookPayload{
		Kind:        "deployment",
		Scope:       "prod",
		Name:        "api",
		ReleaseID:   "rel-42",
		Image:       "ghcr.io/me/api:v1",
		Status:      "success",
		StartedAt:   "2026-05-01T12:00:00Z",
		CompletedAt: "2026-05-01T12:00:45Z",
	}

	if call.Payload != want {
		t.Errorf("payload mismatch:\n got  %+v\n want %+v", call.Payload, want)
	}

	// Re-marshal to JSON to assert the wire field names (struct
	// equality above checks values, but a field rename with a
	// matching JSON tag would slip through).
	body, err := json.Marshal(call.Payload)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{
		`"kind":"deployment"`,
		`"scope":"prod"`,
		`"name":"api"`,
		`"release_id":"rel-42"`,
		`"image":"ghcr.io/me/api:v1"`,
		`"status":"success"`,
		`"started_at":"2026-05-01T12:00:00Z"`,
		`"completed_at":"2026-05-01T12:00:45Z"`,
	} {
		if !strings.Contains(string(body), key) {
			t.Errorf("JSON missing expected key fragment %q in body: %s", key, body)
		}
	}
}

// waitFor polls a predicate up to 2 seconds, returning when it
// flips true. Used to bridge the fire-and-forget goroutine in
// fireDeployWebhook into a synchronous test assertion. Polling is
// cheap (1ms tick), so a flaky test would surface as "the
// webhook never fired" rather than as a wall-clock flake.
func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if predicate() {
			return
		}

		time.Sleep(1 * time.Millisecond)
	}

	t.Fatalf("waitFor: predicate never returned true within 2s")
}

// Compile-time check that the fake satisfies the interface.
var _ WebhookPoster = (*fakeWebhookPoster)(nil)
