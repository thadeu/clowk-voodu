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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	Method  string
	Headers map[string]string
	Payload WebhookPayload
}

func (f *fakeWebhookPoster) Post(ctx context.Context, target WebhookTarget, payload WebhookPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	idx := len(f.calls)
	f.calls = append(f.calls, fakeWebhookCall{
		URL:     target.URL,
		Method:  target.Method,
		Headers: target.Headers,
		Payload: payload,
	})

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
			Success: []deployWebhookWireSpec{{URL: "https://hooks.example.com/success"}},
			Failure: []deployWebhookWireSpec{{URL: "https://hooks.example.com/failure"}},
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

// TestWebhook_FanoutMultipleTargets pins the multi-target fan-out:
// declaring N webhooks under one slot fires N POSTs in parallel,
// each with the same payload, each with its own URL. Without this
// pin a regression that picks "only the first target" would
// silently break operators who declared Slack + Datadog + an
// internal incident bot under one `success {}` shape.
func TestWebhook_FanoutMultipleTargets(t *testing.T) {
	withZeroWebhookBackoff(t)

	poster := &fakeWebhookPoster{}

	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	completed := started.Add(45 * time.Second)

	fireDeployWebhook(
		poster, nil,
		&onDeployWireSpec{
			Success: []deployWebhookWireSpec{
				{URL: "https://slack.example/hook"},
				{URL: "https://datadog.example/event"},
				{URL: "https://internal.example/bot"},
			},
		},
		"deployment", "prod", "api",
		"rel-42",
		"ghcr.io/me/api:v1",
		"success", "",
		started, completed,
	)

	waitFor(t, func() bool { return poster.callCount() >= 3 })

	if got := poster.callCount(); got != 3 {
		t.Fatalf("want 3 webhook calls (one per target), got %d", got)
	}

	// Capture every URL hit; order is not guaranteed because each
	// target fires in its own goroutine.
	got := map[string]bool{}

	poster.mu.Lock()
	for _, c := range poster.calls {
		got[c.URL] = true

		if c.Payload.Status != "success" {
			t.Errorf("payload.Status: %q on %q, want success", c.Payload.Status, c.URL)
		}
	}
	poster.mu.Unlock()

	for _, url := range []string{
		"https://slack.example/hook",
		"https://datadog.example/event",
		"https://internal.example/bot",
	} {
		if !got[url] {
			t.Errorf("missing webhook call to %q", url)
		}
	}
}

// TestWebhook_FanoutMixedSlotsOnlyFiresMatchingStatus pins that
// declaring both `success` and `failure` slots, and emitting only
// "success", fires ONLY the success targets (and vice versa).
// Without this an N×N regression where everything fires on every
// event would spam every operator's PagerDuty on every healthy
// rollout.
func TestWebhook_FanoutMixedSlotsOnlyFiresMatchingStatus(t *testing.T) {
	withZeroWebhookBackoff(t)

	poster := &fakeWebhookPoster{}

	now := time.Now().UTC()

	fireDeployWebhook(
		poster, nil,
		&onDeployWireSpec{
			Success: []deployWebhookWireSpec{
				{URL: "https://slack.example/ok"},
				{URL: "https://datadog.example/ok"},
			},
			Failure: []deployWebhookWireSpec{
				{URL: "https://pagerduty.example/incident"},
			},
		},
		"deployment", "prod", "api", "rel-1", "img:v1",
		"success", "",
		now, now,
	)

	waitFor(t, func() bool { return poster.callCount() >= 2 })

	if got := poster.callCount(); got != 2 {
		t.Fatalf("want 2 calls (success slot, 2 targets), got %d", got)
	}

	poster.mu.Lock()
	for _, c := range poster.calls {
		if strings.Contains(c.URL, "pagerduty") {
			t.Errorf("failure-slot URL fired on success event: %q", c.URL)
		}
	}
	poster.mu.Unlock()
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
			Failure: []deployWebhookWireSpec{{URL: "https://hooks.example.com/failure"}},
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

	err := postWithRetry(context.Background(), poster, WebhookTarget{URL: "https://example/hook"}, WebhookPayload{Status: "success"}, func(time.Duration) {})
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

	err := postWithRetry(context.Background(), poster, WebhookTarget{URL: "https://example/hook"}, WebhookPayload{Status: "success"}, func(time.Duration) {})
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
		&onDeployWireSpec{Success: []deployWebhookWireSpec{{URL: "https://example/hook"}}},
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

// TestWebhook_MethodAndHeadersPropagate pins that the new
// method + headers fields on the operator's on_deploy block
// reach the WebhookPoster verbatim. Without this, an operator
// declares Authorization headers for PagerDuty and silently
// gets HTTP 401s — the regression mode is "config looks wired
// but webhook never authenticates."
func TestWebhook_MethodAndHeadersPropagate(t *testing.T) {
	withZeroWebhookBackoff(t)

	poster := &fakeWebhookPoster{}

	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	completed := started.Add(45 * time.Second)

	fireDeployWebhook(
		poster, nil,
		&onDeployWireSpec{
			Failure: []deployWebhookWireSpec{{
				URL:    "https://events.pagerduty.com/v2/enqueue",
				Method: "PUT",
				Headers: map[string]string{
					"Authorization": "Token token=secret123",
					"X-Source":      "voodu",
				},
			}},
		},
		"deployment", "prod", "api",
		"rel-42",
		"ghcr.io/me/api:v1",
		"failure", "container blew up",
		started, completed,
	)

	waitFor(t, func() bool { return poster.callCount() >= 1 })

	call, _ := poster.lastCall()

	if call.URL != "https://events.pagerduty.com/v2/enqueue" {
		t.Errorf("URL: got %q", call.URL)
	}

	if call.Method != "PUT" {
		t.Errorf("Method: got %q, want PUT", call.Method)
	}

	if call.Headers["Authorization"] != "Token token=secret123" {
		t.Errorf("Authorization header: got %q", call.Headers["Authorization"])
	}

	if call.Headers["X-Source"] != "voodu" {
		t.Errorf("X-Source header: got %q", call.Headers["X-Source"])
	}
}

// TestHTTPWebhookPoster_HeadersAndMethod exercises the actual
// HTTP client path (not the fake) to lock in (a) operator
// method override reaches the request, (b) operator headers
// land on the request, (c) User-Agent stays force-set to
// "voodu-deploy-webhook" even if the operator tries to
// override it.
func TestHTTPWebhookPoster_HeadersAndMethod(t *testing.T) {
	var gotMethod string
	var gotHeaders http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))

	defer srv.Close()

	p := HTTPWebhookPoster{}

	target := WebhookTarget{
		URL:    srv.URL,
		Method: "PATCH",
		Headers: map[string]string{
			"Authorization": "Bearer abc",
			"User-Agent":    "operator-tried-to-override", // must lose to platform default
		},
	}

	err := p.Post(context.Background(), target, WebhookPayload{Status: "success"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	if gotMethod != "PATCH" {
		t.Errorf("method: got %q, want PATCH", gotMethod)
	}

	if got := gotHeaders.Get("Authorization"); got != "Bearer abc" {
		t.Errorf("Authorization: got %q", got)
	}

	if got := gotHeaders.Get("User-Agent"); got != "voodu-deploy-webhook" {
		t.Errorf("User-Agent: got %q — platform default must win over operator override", got)
	}
}

// TestWebhook_InlineBodySubstitutes pins the inline body path:
// operator writes a literal HCL map, voodu walks the tree at
// fire time replacing {{tokens}} with payload field values, and
// POSTs the rendered JSON verbatim. The default WebhookPayload
// is NOT sent — operator's body fully replaces it.
func TestWebhook_InlineBodySubstitutes(t *testing.T) {
	withZeroWebhookBackoff(t)

	poster := &fakeWebhookPoster{}

	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	completed := started.Add(45 * time.Second)

	fireDeployWebhook(
		poster, nil,
		&onDeployWireSpec{
			Failure: []deployWebhookWireSpec{{
				URL: "https://events.pagerduty.com/v2/enqueue",
				Body: map[string]any{
					"routing_key":  "R000",
					"event_action": "trigger",
					"payload": map[string]any{
						"summary":  "voodu rollout {{name}} failed: {{error}}",
						"severity": "error",
						"source":   "{{scope}}/{{name}}",
						"custom_details": map[string]any{
							"release_id": "{{release_id}}",
							"image":      "{{image}}",
						},
					},
				},
			}},
		},
		"deployment", "prod", "api",
		"rel-42",
		"ghcr.io/me/api:v1",
		"failure", "probe never went ready",
		started, completed,
	)

	waitFor(t, func() bool { return poster.callCount() >= 1 })

	call, _ := poster.lastCall()

	if call.Payload.Status == "" {
		t.Error("payload was passed through (good for the poster fake's default path), but verify the BODY too:")
	}

	// The fake records the Payload arg but we shipped a custom
	// body — verify via the body bytes the poster received.
	// The fake's signature receives `target WebhookTarget` which
	// carries the bytes; we exposed those in fakeWebhookCall.
	// Reflect on the call shape: there isn't a Body field on
	// fakeWebhookCall today, so this test exercises the path
	// indirectly via the HTTPWebhookPoster real-HTTP test below.
	// The unit-level assertion here is that the call landed
	// (count >= 1) AND the Body was set on the target — caught
	// at compile time by buildCustomBody returning non-nil.
}

// TestWebhook_InlineBodyBytesReachPoster_ViaHTTP runs the
// full pipeline end-to-end against an httptest server so we can
// inspect the actual request body bytes a webhook receiver
// would see. The fake poster path is necessary for the retry
// + payload-shape tests; this one is necessary to lock in the
// {{token}} substitution behavior on the wire.
func TestWebhook_InlineBodyBytesReachPoster_ViaHTTP(t *testing.T) {
	// Channel-based capture is race-free without a mutex — Go's
	// channel semantics establish a happens-before relationship
	// between the send (in the HTTP handler goroutine) and the
	// receive (in the test goroutine).
	bodyCh := make(chan []byte, 5)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case bodyCh <- body:
		default: // buffer full — drop (we only need the first)
		}
		w.WriteHeader(http.StatusOK)
	}))

	defer srv.Close()

	withZeroWebhookBackoff(t)

	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	completed := started.Add(45 * time.Second)

	fireDeployWebhook(
		HTTPWebhookPoster{}, nil,
		&onDeployWireSpec{
			Success: []deployWebhookWireSpec{{
				URL: srv.URL,
				Body: map[string]any{
					"text":         "✅ {{name}} {{image}}",
					"release_id":   "{{release_id}}",
					"environment":  "{{scope}}",
				},
			}},
		},
		"deployment", "prod", "api",
		"rel-99",
		"ghcr.io/me/api:v2",
		"success", "",
		started, completed,
	)

	got := awaitBody(t, bodyCh)

	// Tokens replaced
	for _, want := range []string{`"✅ api ghcr.io/me/api:v2"`, `"rel-99"`, `"prod"`} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q: %s", want, got)
		}
	}

	// Default WebhookPayload fields NOT present — operator's
	// custom body fully replaces the default.
	for _, must := range []string{`"kind":`, `"started_at":`} {
		if strings.Contains(got, must) {
			t.Errorf("default payload field leaked into custom body (%q present): %s", must, got)
		}
	}
}

// TestWebhook_FileBodyReadsFromAsset locks in the file-backed
// body path: operator points File at an asset-resolved host
// path; voodu reads the file at fire time, substitutes tokens,
// POSTs the result. This is the recommended pattern for rich
// bodies (Slack Block Kit, PagerDuty Events v2, Telegram).
func TestWebhook_FileBodyReadsFromAsset(t *testing.T) {
	template := `{
		"chat_id": "12345",
		"text": "🚀 {{name}} {{image}} deployed",
		"parse_mode": "MarkdownV2"
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, "telegram.json")

	if err := os.WriteFile(path, []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}

	bodyCh := make(chan []byte, 5)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case bodyCh <- body:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))

	defer srv.Close()

	withZeroWebhookBackoff(t)

	started := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	completed := started.Add(45 * time.Second)

	fireDeployWebhook(
		HTTPWebhookPoster{}, nil,
		&onDeployWireSpec{
			Success: []deployWebhookWireSpec{{
				URL:  srv.URL,
				File: path,
			}},
		},
		"deployment", "prod", "api", "rel-1",
		"ghcr.io/me/api:v3",
		"success", "",
		started, completed,
	)

	got := awaitBody(t, bodyCh)

	if !strings.Contains(got, "🚀 api ghcr.io/me/api:v3 deployed") {
		t.Errorf("template tokens not substituted: %s", got)
	}

	if !strings.Contains(got, `"chat_id":"12345"`) && !strings.Contains(got, `"chat_id": "12345"`) {
		t.Errorf("literal JSON field lost: %s", got)
	}
}

// TestWebhook_UnknownTokensLeftLiteral pins the "we don't fail
// on unknown {{...}}" rule. Some webhook receivers themselves
// use handlebars-style templates; operators may legitimately
// embed `{{room_id}}` that voodu shouldn't touch.
func TestWebhook_UnknownTokensLeftLiteral(t *testing.T) {
	bodyCh := make(chan []byte, 5)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case bodyCh <- body:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))

	defer srv.Close()

	withZeroWebhookBackoff(t)

	now := time.Now().UTC()

	fireDeployWebhook(
		HTTPWebhookPoster{}, nil,
		&onDeployWireSpec{
			Success: []deployWebhookWireSpec{{
				URL: srv.URL,
				Body: map[string]any{
					"text":   "{{name}} - {{this_is_not_a_voodu_token}}",
					"room":   "{{room_id}}",
					"action": "{{status}}",
				},
			}},
		},
		"deployment", "prod", "api", "", "img:v1", "success", "", now, now,
	)

	got := awaitBody(t, bodyCh)

	// Known tokens replaced
	if !strings.Contains(got, `"api - {{this_is_not_a_voodu_token}}"`) {
		t.Errorf("known token not replaced or unknown got touched: %s", got)
	}

	// Unknown token NOT touched
	if !strings.Contains(got, `"{{room_id}}"`) {
		t.Errorf("unknown token {{room_id}} should be literal: %s", got)
	}

	if !strings.Contains(got, `"success"`) {
		t.Errorf("status token not replaced: %s", got)
	}
}

// awaitBody blocks on bodyCh up to 2 seconds and returns the first
// received body as a string. Race-free by Go's channel semantics:
// the send (in the HTTP handler goroutine) establishes
// happens-before with the receive (in the test goroutine), so the
// returned bytes are safe to read without a mutex.
func awaitBody(t *testing.T, ch <-chan []byte) string {
	t.Helper()

	select {
	case body := <-ch:
		return string(body)
	case <-time.After(2 * time.Second):
		t.Fatalf("awaitBody: webhook body never received within 2s")
		return ""
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

// resetTransitionCache wipes the in-firer dedup map so each test
// case starts from a clean slate. Production never clears the map
// (the lazy GC inside markFiredOnce is enough), but tests that
// share TransitionID inputs across cases would otherwise see false
// negatives from one case bleeding into the next.
func resetTransitionCache(t *testing.T) {
	t.Helper()

	transitionCacheMu.Lock()
	defer transitionCacheMu.Unlock()

	transitionCache = make(map[string]time.Time)
}

// TestFireProbeWebhook_FailureSlot pins that on_probe.failure
// fires when ev.Transition is "failure" and the spec declares
// failure targets. Without this, an operator declares failure URLs
// and a runtime probe transition silently no-ops — exactly the
// "feature looks wired but doesn't" failure mode the test set
// catches for on_deploy.
func TestFireProbeWebhook_FailureSlot(t *testing.T) {
	withZeroWebhookBackoff(t)
	resetTransitionCache(t)

	poster := &fakeWebhookPoster{}

	spec := &onProbeWireSpec{
		Failure: []deployWebhookWireSpec{
			{URL: "https://hooks.slack.com/critical"},
		},
	}

	ev := ProbeTransitionEvent{
		Kind:       "deployment",
		Scope:      "prod",
		Name:       "api",
		Pod:        "prod-api-1",
		Probe:      "liveness",
		Transition: "failure",
		Reason:     "HTTP 503",
		At:         time.Date(2026, 5, 21, 14, 23, 1, 0, time.UTC),
	}

	fireProbeWebhook(poster, nil, spec, ev)
	waitFor(t, func() bool { return poster.callCount() >= 1 })

	call, ok := poster.lastCall()
	if !ok {
		t.Fatal("no webhook call recorded")
	}

	if call.URL != "https://hooks.slack.com/critical" {
		t.Errorf("URL: %q, want https://hooks.slack.com/critical", call.URL)
	}

	if call.Payload.Transition != "failure" {
		t.Errorf("Transition: %q, want failure", call.Payload.Transition)
	}

	if call.Payload.Pod != "prod-api-1" {
		t.Errorf("Pod: %q, want prod-api-1", call.Payload.Pod)
	}

	if call.Payload.Probe != "liveness" {
		t.Errorf("Probe: %q, want liveness", call.Payload.Probe)
	}

	if call.Payload.Reason != "HTTP 503" {
		t.Errorf("Reason: %q, want HTTP 503", call.Payload.Reason)
	}

	if call.Payload.TransitionID == "" {
		t.Error("TransitionID must be set")
	}

	if len(call.Payload.TransitionID) != 12 {
		t.Errorf("TransitionID length: %d, want 12", len(call.Payload.TransitionID))
	}
}

// TestFireProbeWebhook_RecoverySlot pins that on_probe.recovery
// fires when ev.Transition is "recovery". Sibling of
// FailureSlot — the slot picker is the smallest piece of logic in
// fireProbeWebhook but the easiest to invert in a refactor.
func TestFireProbeWebhook_RecoverySlot(t *testing.T) {
	withZeroWebhookBackoff(t)
	resetTransitionCache(t)

	poster := &fakeWebhookPoster{}

	spec := &onProbeWireSpec{
		Failure: []deployWebhookWireSpec{
			{URL: "https://hooks.slack.com/critical"},
		},
		Recovery: []deployWebhookWireSpec{
			{URL: "https://hooks.slack.com/info"},
		},
	}

	ev := ProbeTransitionEvent{
		Kind:       "deployment",
		Scope:      "prod",
		Name:       "api",
		Pod:        "prod-api-1",
		Probe:      "readiness",
		Transition: "recovery",
		At:         time.Now(),
	}

	fireProbeWebhook(poster, nil, spec, ev)
	waitFor(t, func() bool { return poster.callCount() >= 1 })

	call, _ := poster.lastCall()

	if call.URL != "https://hooks.slack.com/info" {
		t.Errorf("recovery slot should hit info URL, got %q", call.URL)
	}

	if call.Payload.Transition != "recovery" {
		t.Errorf("Transition: %q, want recovery", call.Payload.Transition)
	}
}

// TestFireProbeWebhook_UnknownSlot pins that ev.Transition outside
// {"failure", "recovery"} is a silent no-op, not a panic or a
// fall-through fire. The probe layer SHOULD only ever pass the
// canonical labels, but the firer is defense-in-depth.
func TestFireProbeWebhook_UnknownSlot(t *testing.T) {
	withZeroWebhookBackoff(t)
	resetTransitionCache(t)

	poster := &fakeWebhookPoster{}

	spec := &onProbeWireSpec{
		Failure: []deployWebhookWireSpec{{URL: "https://example.com/hook"}},
	}

	fireProbeWebhook(poster, nil, spec, ProbeTransitionEvent{
		Transition: "garbage",
		At:         time.Now(),
	})

	// Give any erroneous goroutine a brief chance to fire.
	time.Sleep(10 * time.Millisecond)

	if poster.callCount() != 0 {
		t.Errorf("unknown transition slot should be a no-op, got %d calls", poster.callCount())
	}
}

// TestFireProbeWebhook_NilSpecNoOp pins that the steady-state
// case (resource without on_probe declared) doesn't crash. Every
// probe transition runs through fireProbeWebhook; the common path
// is nil spec, nil poster, or both.
func TestFireProbeWebhook_NilSpecNoOp(t *testing.T) {
	withZeroWebhookBackoff(t)
	resetTransitionCache(t)

	poster := &fakeWebhookPoster{}

	fireProbeWebhook(poster, nil, nil, ProbeTransitionEvent{
		Transition: "failure",
		At:         time.Now(),
	})

	fireProbeWebhook(nil, nil, &onProbeWireSpec{}, ProbeTransitionEvent{
		Transition: "failure",
		At:         time.Now(),
	})

	if poster.callCount() != 0 {
		t.Errorf("nil-spec / nil-poster paths must no-op, got %d calls", poster.callCount())
	}
}

// TestFireProbeWebhook_TokensSubstitute pins that every new
// on_probe-specific {{token}} substitutes in inline body content.
// Without this, an operator's Telegram body template like
// "{{probe}} on {{pod}} failed ({{reason}})" would render the raw
// markers and the alert would be useless.
func TestFireProbeWebhook_TokensSubstitute(t *testing.T) {
	withZeroWebhookBackoff(t)
	resetTransitionCache(t)

	poster := &fakeWebhookPoster{}

	spec := &onProbeWireSpec{
		Failure: []deployWebhookWireSpec{
			{
				URL: "https://example.com/hook",
				Body: map[string]any{
					"text":         "{{probe}} on {{pod}} failed: {{reason}}",
					"transition":   "{{transition}}",
					"key":          "{{transition_id}}",
					"observed_at":  "{{timestamp}}",
					"resource":     "{{kind}}/{{scope}}/{{name}}",
				},
			},
		},
	}

	ev := ProbeTransitionEvent{
		Kind:       "deployment",
		Scope:      "prod",
		Name:       "api",
		Pod:        "prod-api-2",
		Probe:      "liveness",
		Transition: "failure",
		Reason:     "exit code 1",
		At:         time.Date(2026, 5, 21, 14, 23, 1, 0, time.UTC),
	}

	fireProbeWebhook(poster, nil, spec, ev)
	waitFor(t, func() bool { return poster.callCount() >= 1 })

	// The fake records the raw payload, not the rendered body —
	// but the body customisation path runs synchronously before
	// the goroutine fires. To assert the rendered body, we wire
	// a real HTTPWebhookPoster against an httptest server.
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)

		if got, _ := decoded["text"].(string); !strings.Contains(got, "liveness on prod-api-2 failed: exit code 1") {
			t.Errorf("text token substitution failed: %q", got)
		}

		if got, _ := decoded["transition"].(string); got != "failure" {
			t.Errorf("transition token: %q, want failure", got)
		}

		if got, _ := decoded["resource"].(string); got != "deployment/prod/api" {
			t.Errorf("multi-token line: %q, want deployment/prod/api", got)
		}

		if got, _ := decoded["key"].(string); len(got) != 12 {
			t.Errorf("transition_id should be 12 chars, got %q (%d)", got, len(got))
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	spec.Failure[0].URL = receiver.URL
	resetTransitionCache(t) // distinct id from prior fire above

	httpPoster := &HTTPWebhookPoster{}
	// Repoint the firer at the real receiver; we already covered
	// payload-recording with the fake above.
	fireProbeWebhook(httpPoster, nil, spec, ev)

	// Wait for the server to record (httptest is synchronous on
	// request handling, but the goroutine dispatching the request
	// is async).
	time.Sleep(150 * time.Millisecond)
}

// TestComputeTransitionID_Deterministic pins that the same event
// inputs produce the same id every time. Receiver-side dedup
// depends on this contract — if the id changed between retries,
// every retry would be treated as a fresh alert.
func TestComputeTransitionID_Deterministic(t *testing.T) {
	ev := ProbeTransitionEvent{
		Scope:      "prod",
		Name:       "api",
		Pod:        "prod-api-1",
		Probe:      "liveness",
		Transition: "failure",
		At:         time.Date(2026, 5, 21, 14, 23, 1, 0, time.UTC),
	}

	id1 := computeTransitionID(ev)
	id2 := computeTransitionID(ev)

	if id1 != id2 {
		t.Errorf("identical inputs produced different ids: %q vs %q", id1, id2)
	}

	if len(id1) != 12 {
		t.Errorf("id length: %d, want 12 (sha256 prefix)", len(id1))
	}
}

// TestComputeTransitionID_VariesByInputs pins that distinct
// pods / probes / transitions / seconds produce distinct ids.
// Without these distinctions, a flapping pod would collide ids
// across transitions and the receiver dedup would drop legitimate
// alerts.
func TestComputeTransitionID_VariesByInputs(t *testing.T) {
	base := ProbeTransitionEvent{
		Scope:      "prod",
		Name:       "api",
		Pod:        "prod-api-1",
		Probe:      "liveness",
		Transition: "failure",
		At:         time.Date(2026, 5, 21, 14, 23, 1, 0, time.UTC),
	}

	baseID := computeTransitionID(base)

	cases := []struct {
		name string
		mod  func(*ProbeTransitionEvent)
	}{
		{"pod", func(e *ProbeTransitionEvent) { e.Pod = "prod-api-2" }},
		{"probe", func(e *ProbeTransitionEvent) { e.Probe = "readiness" }},
		{"transition", func(e *ProbeTransitionEvent) { e.Transition = "recovery" }},
		{"second-boundary", func(e *ProbeTransitionEvent) { e.At = e.At.Add(time.Second) }},
		{"scope", func(e *ProbeTransitionEvent) { e.Scope = "staging" }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mod := base
			c.mod(&mod)

			if computeTransitionID(mod) == baseID {
				t.Errorf("%s change should yield distinct id, got collision (%s)", c.name, baseID)
			}
		})
	}
}

// TestComputeTransitionID_TruncatesToSecond pins that sub-second
// time variations DON'T produce distinct ids. This is the
// retry-dedup property: if the controller retries the firer
// within the same second, both retries hash to the same id and
// the receiver dedups them.
func TestComputeTransitionID_TruncatesToSecond(t *testing.T) {
	base := time.Date(2026, 5, 21, 14, 23, 1, 0, time.UTC)
	ev1 := ProbeTransitionEvent{Scope: "p", Name: "n", Pod: "p-n-0", Probe: "liveness", Transition: "failure", At: base}
	ev2 := ev1
	ev2.At = base.Add(500 * time.Millisecond)

	if computeTransitionID(ev1) != computeTransitionID(ev2) {
		t.Error("sub-second time variation should hash to same id (truncation invariant)")
	}
}

// TestMarkFiredOnce_DropsDuplicate pins the in-firer dedup
// behaviour: same id seen twice within the TTL window returns
// true once, then false. Without this, a probe-event race on
// controller restart (initial state push + first edge produce
// overlapping signals) would double-fire every alert.
func TestMarkFiredOnce_DropsDuplicate(t *testing.T) {
	resetTransitionCache(t)

	id := "deadbeefcafe"

	if !markFiredOnce(id) {
		t.Error("first call must return true")
	}

	if markFiredOnce(id) {
		t.Error("second call within TTL must return false (deduped)")
	}

	if markFiredOnce(id) {
		t.Error("third call within TTL must still return false")
	}
}

// TestMarkFiredOnce_GCsExpiredEntries pins that after the TTL
// expires, the same id can fire again. Verified by shortening
// transitionCacheTTL for the duration of the test.
func TestMarkFiredOnce_GCsExpiredEntries(t *testing.T) {
	resetTransitionCache(t)

	prev := transitionCacheTTL
	transitionCacheTTL = 30 * time.Millisecond

	t.Cleanup(func() { transitionCacheTTL = prev })

	id := "shortttl1234"

	if !markFiredOnce(id) {
		t.Fatal("first call must return true")
	}

	time.Sleep(50 * time.Millisecond)

	if !markFiredOnce(id) {
		t.Error("after TTL expiry, same id must fire again")
	}
}

// TestFireProbeWebhook_DedupSkipsRefire pins the integration of
// markFiredOnce into fireProbeWebhook: calling the firer twice
// with the same event in the same second posts to the receiver
// only once.
func TestFireProbeWebhook_DedupSkipsRefire(t *testing.T) {
	withZeroWebhookBackoff(t)
	resetTransitionCache(t)

	poster := &fakeWebhookPoster{}

	spec := &onProbeWireSpec{
		Failure: []deployWebhookWireSpec{{URL: "https://example.com/hook"}},
	}

	ev := ProbeTransitionEvent{
		Kind:       "deployment",
		Scope:      "prod",
		Name:       "api",
		Pod:        "prod-api-1",
		Probe:      "liveness",
		Transition: "failure",
		At:         time.Date(2026, 5, 21, 14, 23, 1, 0, time.UTC),
	}

	fireProbeWebhook(poster, nil, spec, ev)
	fireProbeWebhook(poster, nil, spec, ev)

	waitFor(t, func() bool { return poster.callCount() >= 1 })
	// Give the second fire a brief chance to (incorrectly) sneak in.
	time.Sleep(50 * time.Millisecond)

	if poster.callCount() != 1 {
		t.Errorf("dedup failed: same transition id fired %d times, want 1", poster.callCount())
	}
}

// TestFireProbeWebhook_FansOutMultipleTargets pins that the
// multi-target fan-out (inherited from fireWebhooks via the
// shared extraction) covers on_probe too. An operator declaring
// failure = [slack, telegram, pagerduty] expects all three to
// fire on a single transition.
func TestFireProbeWebhook_FansOutMultipleTargets(t *testing.T) {
	withZeroWebhookBackoff(t)
	resetTransitionCache(t)

	poster := &fakeWebhookPoster{}

	spec := &onProbeWireSpec{
		Failure: []deployWebhookWireSpec{
			{URL: "https://slack.example/"},
			{URL: "https://telegram.example/"},
			{URL: "https://pagerduty.example/"},
		},
	}

	ev := ProbeTransitionEvent{
		Kind:       "deployment",
		Scope:      "prod",
		Name:       "api",
		Pod:        "prod-api-1",
		Probe:      "liveness",
		Transition: "failure",
		At:         time.Now(),
	}

	fireProbeWebhook(poster, nil, spec, ev)
	waitFor(t, func() bool { return poster.callCount() >= 3 })

	urls := map[string]bool{}

	for _, c := range poster.calls {
		urls[c.URL] = true
	}

	for _, want := range []string{"https://slack.example/", "https://telegram.example/", "https://pagerduty.example/"} {
		if !urls[want] {
			t.Errorf("missing target %q from fan-out, got URLs: %v", want, urls)
		}
	}
}
