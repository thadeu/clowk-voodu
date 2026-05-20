// webhooks.go owns the deploy-webhook delivery path: a tiny POST
// pipeline triggered when a deployment's rolling-restart completes
// or fails. The contract is intentionally minimal and best-effort.
//
//   - The handler enqueues at most ONE POST per terminal event
//     (success OR failure, never both for the same release).
//   - Delivery retries 3 times with exponential backoff (1s, 5s,
//     30s). After the third failure we log and drop — the deploy
//     itself NEVER fails because the webhook side did.
//   - Per-attempt timeout caps each HTTP request at 10s so a
//     misconfigured endpoint can't pin the reconciler.
//
// Why this lives behind an interface rather than a direct
// `http.Post`: tests need to assert call shape (URL, payload,
// retry count) without exercising the real net/http stack. The
// production wiring is HTTPWebhookPoster; tests pass a fake.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// WebhookPayload is the JSON body POSTed to the operator's
// configured URL. Shape is Slack-incoming-webhook compatible in
// the sense that consumers expecting "any JSON" will get a
// well-typed blob; teams using Slack block kit feed this through
// their own URL transformer.
//
// Fields are stable wire contract. Adding new fields is safe
// (consumers ignore unknown keys); renaming or removing is a
// breaking change. Keep the set narrow on purpose — the operator
// can join richer info from `vd describe` if they need more.
type WebhookPayload struct {
	// Kind is the resource kind whose rollout triggered the
	// hook. Today only "deployment" emits webhooks; the field
	// exists so a future statefulset/job hook can share the
	// schema.
	Kind string `json:"kind"`

	// Scope and Name jointly identify the resource. Together
	// they form the AppID the operator sees in `vd describe`.
	Scope string `json:"scope"`
	Name  string `json:"name"`

	// ReleaseID is the 9-char release record ID this rollout
	// produced. Empty for env-change-only restarts that aren't
	// tied to a release record (rare; the apply path mints one
	// for every meaningful state change today).
	ReleaseID string `json:"release_id,omitempty"`

	// Image is the resolved image tag the rollout brought up.
	// Mirrors what `vd release <ref>` shows for the same record.
	Image string `json:"image,omitempty"`

	// Status is "success" or "failure". Operators key their
	// channel routing / Slack colour off this.
	Status string `json:"status"`

	// StartedAt and CompletedAt bracket the rollout in
	// wall-clock time. RFC3339 strings for transport — every
	// JSON consumer (Slack, Discord, generic curl-based bots)
	// handles them without extra parsing.
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`

	// Error carries the first failure message that aborted the
	// rollout. Empty for successful runs. Same shape the release
	// record's Error field uses.
	Error string `json:"error,omitempty"`
}

// WebhookPoster is the seam between the handler and the HTTP
// world. Production wires HTTPWebhookPoster; tests pass a fake
// that records calls and (optionally) returns canned errors so
// the retry path is reachable from a unit test.
//
// The `target` carries URL + method + headers — anything the
// caller wants the HTTP layer to honour beyond the standard
// payload body.
type WebhookPoster interface {
	Post(ctx context.Context, target WebhookTarget, payload WebhookPayload) error
}

// WebhookTarget bundles the per-endpoint HTTP request shape.
// Mirrors the controller-side deployWebhookWireSpec but lives
// here so the Poster interface stays free of "wire" naming —
// fakes and adapters reach for this type directly.
type WebhookTarget struct {
	// URL is the absolute endpoint to hit. Required.
	URL string

	// Method is the HTTP verb. Empty → "POST" (the default;
	// applied at request-build time, not at parse time).
	// Parser already validates the operator-supplied value
	// against {POST,PUT,PATCH,DELETE} so by the time we land
	// here it's safe to use verbatim.
	Method string

	// Headers stack on top of the platform-set defaults
	// (Content-Type: application/json; User-Agent:
	// voodu-deploy-webhook). Operator-set values OVERRIDE the
	// Content-Type default; User-Agent is force-overwritten
	// after the operator's headers apply, so source-of-call
	// debugging on the receiver side stays reliable.
	Headers map[string]string

	// Body is the request body bytes the poster should send. nil
	// → poster serialises the WebhookPayload arg as JSON
	// (default behaviour, back-compat with pre-customisation
	// webhooks). Non-nil → poster sends Body verbatim and
	// ignores the WebhookPayload arg for body purposes (operator
	// declared inline body or file template; substitution
	// already done upstream).
	Body []byte
}

// HTTPWebhookPoster is the production poster. Each Post is a
// single, self-contained HTTP request — no connection pooling
// beyond what http.DefaultTransport already does. Cheap to
// create; the handler owns one instance per controller.
type HTTPWebhookPoster struct {
	// Client lets callers inject a custom http.Client (e.g. a
	// mock for integration tests, or one with a fixed retry
	// disabled). Empty means "use a fresh client with the per-
	// attempt timeout below."
	Client *http.Client
}

// webhookAttemptTimeout caps a single HTTP request. The retry
// loop above tries this 3 times, so a fully-failing endpoint
// blocks deploy completion for at most ~36s (10s + 1s + 10s +
// 5s + 10s + 30s = 66s in the worst pathological case where
// every attempt actually waits the full 10s before timing out).
// In practice a healthy endpoint replies in <1s and the total
// is far smaller.
const webhookAttemptTimeout = 10 * time.Second

// Post executes a single attempt. The handler's retry loop
// (postWithRetry) wraps this; Post itself does NOT retry — it
// returns the raw network/HTTP error so the loop can decide
// whether to back off and re-try.
//
// We treat any non-2xx response as a failure so a misconfigured
// endpoint returning 500 actually triggers retries. 3xx isn't
// specially handled (the http.Client follows redirects by
// default); 4xx is a permanent failure but we still retry —
// caller policy is "always try 3 times" for simplicity, and an
// endpoint flapping between 4xx/2xx is rare enough not to
// special-case.
func (p HTTPWebhookPoster) Post(ctx context.Context, target WebhookTarget, payload WebhookPayload) error {
	body := target.Body

	if body == nil {
		// Default behaviour: serialise the platform payload.
		// Operator declared neither inline body nor file
		// template — they get the standard JSON shape.
		var err error

		body, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal webhook payload: %w", err)
		}
	}

	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: webhookAttemptTimeout}
	}

	method := strings.ToUpper(strings.TrimSpace(target.Method))
	if method == "" {
		method = http.MethodPost
	}

	req, err := http.NewRequestWithContext(ctx, method, target.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}

	// Default headers first so operator overrides take precedence
	// (except User-Agent — see below).
	req.Header.Set("Content-Type", "application/json")

	// Operator-supplied headers. May override Content-Type for
	// receivers that require a specific media type (e.g. Datadog
	// metric submission flavours).
	for k, v := range target.Headers {
		req.Header.Set(k, v)
	}

	// User-Agent is force-set AFTER operator headers so receivers
	// can always trust the source-of-call debug signal. Operators
	// who want their own UA on a particular receiver should run a
	// transformer between voodu and that receiver.
	req.Header.Set("User-Agent", "voodu-deploy-webhook")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook responded %s", resp.Status)
	}

	return nil
}

// webhookBackoff is the retry schedule for webhook delivery.
// Spec: 3 attempts with backoff 1s, 5s, 30s. Interpretation: each
// slot is the wall-clock pause BEFORE its attempt; attempt 1 has
// no pre-sleep, so 1s + 5s + 30s would cover 4 attempts. We honour
// the spec's "3 attempts" cap by stopping after entry 2 (30s is
// reserved for a future "give up after this much delay" backstop;
// today's loop never reaches it).
//
// Total maximum wall-clock for an always-failing endpoint with
// the 10s per-attempt timeout:
//
//	10s (attempt 1) + 1s + 10s (attempt 2) + 5s + 10s (attempt 3) ≈ 36s
//
// var (not const) so tests can substitute a no-sleep schedule.
// Production never writes to it.
var webhookBackoff = []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second}

// webhookMaxAttempts caps the retry loop at the spec's "3
// attempts" budget. Kept as a separate var (not derived from
// len(webhookBackoff)) so tests can shrink the schedule
// independently of the attempt count.
const webhookMaxAttempts = 3

// postWithRetry runs the 3-attempt loop. Returns nil on the
// first successful Post; returns the LAST attempt's error if
// every retry exhausted. Caller (the handler) treats the
// returned error as "log and move on" — webhook failure must
// not bubble up to fail the deploy.
//
// `sleep` is the time.Sleep seam — tests pass a recorder so
// they can assert WITHOUT actually sleeping the wall clock.
// Production passes time.Sleep directly.
func postWithRetry(ctx context.Context, poster WebhookPoster, target WebhookTarget, payload WebhookPayload, sleep func(time.Duration)) error {
	if sleep == nil {
		sleep = time.Sleep
	}

	var lastErr error

	for i := 0; i < webhookMaxAttempts; i++ {
		err := poster.Post(ctx, target, payload)
		if err == nil {
			return nil
		}

		lastErr = err

		// No sleep after the final attempt — we're about to
		// drop. Between attempts, look up the backoff slot
		// (safe even if the slice is shorter than max attempts
		// since the guard above pairs them by index).
		if i < webhookMaxAttempts-1 && i < len(webhookBackoff) {
			sleep(webhookBackoff[i])
		}
	}

	return lastErr
}

// fireDeployWebhook is the handler's one-call helper. It builds
// the shared payload, picks the slot based on status, and fans
// out one goroutine per declared target so multi-destination
// success / failure hooks fire in parallel.
//
// nil poster or empty slot → no-op. This keeps the handler call
// site terse: every rolling-restart success path calls
// fireDeployWebhook regardless of whether the operator declared
// on_deploy, and the no-op gate makes the absent-block case free.
//
// Why goroutines: a single webhook can take ~36s in the worst
// case (3 timed-out attempts). With N targets, sequential
// delivery would scale linearly — N×36s of wall-clock latency
// the operator wouldn't notice (the apply path already
// returned), but log lines would dribble out across minutes.
// Per-target goroutines give bounded total wall-clock (max
// across targets, not sum) and independent retry budgets — a
// slow PagerDuty doesn't delay Slack.
//
// Per-target identity in log lines: `target=<index>/<total>`
// when total > 1, so operators grepping "on_deploy webhook"
// can correlate retries to a specific declared block in the
// HCL surface.
func fireDeployWebhook(poster WebhookPoster, logf func(string, ...any), spec *onDeployWireSpec, kind, scope, name, releaseID, image, status, errMsg string, startedAt, completedAt time.Time) {
	if poster == nil || spec == nil {
		return
	}

	var targets []deployWebhookWireSpec
	switch status {
	case "success":
		targets = spec.Success
	case "failure":
		targets = spec.Failure
	}

	if len(targets) == 0 {
		return
	}

	payload := WebhookPayload{
		Kind:        kind,
		Scope:       scope,
		Name:        name,
		ReleaseID:   releaseID,
		Image:       image,
		Status:      status,
		StartedAt:   startedAt.UTC().Format(time.RFC3339),
		CompletedAt: completedAt.UTC().Format(time.RFC3339),
		Error:       errMsg,
	}

	total := len(targets)

	for i := range targets {
		target := targets[i]

		if target.URL == "" {
			continue
		}

		// Build per-target identity for log correlation. Single-
		// target is the common case; the index is noise when
		// total == 1, so we suppress it.
		ident := status
		if total > 1 {
			ident = fmt.Sprintf("%s[%d/%d]", status, i+1, total)
		}

		wt := WebhookTarget{
			URL:     target.URL,
			Method:  target.Method,
			Headers: target.Headers,
		}

		// Body materialisation. Three branches:
		//
		//   target.Body set  → inline operator-supplied body. Walk
		//                      the map tree substituting {{...}}
		//                      tokens, then json.Marshal.
		//   target.File set  → asset already resolved to a host path
		//                      at apply time. Read the file, parse
		//                      as JSON, walk + substitute, marshal.
		//   neither          → leave wt.Body nil; poster falls back
		//                      to marshalling the default
		//                      WebhookPayload. Back-compat path.
		if body, err := buildCustomBody(&target, payload); err != nil {
			if logf != nil {
				logf("deployment/%s/%s on_deploy webhook (%s) body build failed: %v", scope, name, ident, err)
			}
			// Falls through with wt.Body nil; poster sends the
			// default payload as a safety net. Drop-on-floor is
			// worse than half-defaulting because the operator
			// would never know their custom body was ignored.
		} else if body != nil {
			wt.Body = body
		}

		// Capture wt + ident in this iteration's scope so the
		// goroutine sees its own target — Go's range-loop reuses
		// the loop variable address across iterations, so a naive
		// closure over `target` would race on the wire spec.
		go func(wt WebhookTarget, ident string) {
			// Fresh context — the apply caller's ctx may already be
			// done (HTTP response written) by the time we get here.
			// We don't share its cancellation; the per-attempt
			// timeout is enough to bound runtime.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			if err := postWithRetry(ctx, poster, wt, payload, nil); err != nil {
				if logf != nil {
					logf("deployment/%s/%s on_deploy webhook (%s) dropped after retries: %v", scope, name, ident, err)
				}
			}
		}(wt, ident)
	}
}

// buildCustomBody returns the operator-customised body bytes when
// target declares inline body or file template, with all
// `{{field}}` tokens substituted against the live payload.
// Returns (nil, nil) when neither is declared (caller falls back
// to the default WebhookPayload marshal).
//
// Error paths:
//   - file read failure (file vanished between apply and fire)
//   - JSON parse failure on the file content
// In both cases the caller logs + sends the default payload as
// a safety net (see fireDeployWebhook).
func buildCustomBody(target *deployWebhookWireSpec, payload WebhookPayload) ([]byte, error) {
	var tree map[string]any

	switch {
	case len(target.Body) > 0:
		// Inline body. The map[string]any tree decoded from
		// HCL is unsafe to mutate (it lives on the wire spec
		// stored in etcd and possibly cached); deep-clone via
		// JSON round-trip before substituting.
		raw, err := json.Marshal(target.Body)
		if err != nil {
			return nil, fmt.Errorf("clone inline body: %w", err)
		}

		if err := json.Unmarshal(raw, &tree); err != nil {
			return nil, fmt.Errorf("decode inline body: %w", err)
		}

	case target.File != "":
		raw, err := os.ReadFile(target.File)
		if err != nil {
			return nil, fmt.Errorf("read body file %s: %w", target.File, err)
		}

		if err := json.Unmarshal(raw, &tree); err != nil {
			return nil, fmt.Errorf("parse body file %s: %w", target.File, err)
		}

	default:
		return nil, nil
	}

	substituteWebhookTokens(tree, payload)

	out, err := json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("marshal substituted body: %w", err)
	}

	return out, nil
}

// substituteWebhookTokens walks the JSON tree in place, replacing
// `{{field}}` markers in string values with their live payload
// equivalent. Recurses into nested maps and lists. Unknown tokens
// are left literal (no replacement) — operators may legitimately
// have `{{...}}` text in body content (some receivers themselves
// use handlebars-style templates).
//
// Token surface (case-sensitive):
//
//	{{kind}}         {{scope}}         {{name}}
//	{{release_id}}   {{image}}
//	{{status}}       {{error}}
//	{{started_at}}   {{completed_at}}
func substituteWebhookTokens(node any, payload WebhookPayload) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			switch child := v.(type) {
			case string:
				n[k] = applyWebhookTokens(child, payload)
			default:
				substituteWebhookTokens(child, payload)
			}
		}

	case []any:
		for i, v := range n {
			switch child := v.(type) {
			case string:
				n[i] = applyWebhookTokens(child, payload)
			default:
				substituteWebhookTokens(child, payload)
			}
		}
	}
}

// applyWebhookTokens replaces every known {{token}} in s with its
// payload value. strings.NewReplacer would handle the bulk
// replacements in one pass but we keep the explicit map so
// adding a new token is one line, not a Replacer rebuild.
func applyWebhookTokens(s string, payload WebhookPayload) string {
	if !strings.Contains(s, "{{") {
		return s
	}

	replacements := []string{
		"{{kind}}", payload.Kind,
		"{{scope}}", payload.Scope,
		"{{name}}", payload.Name,
		"{{release_id}}", payload.ReleaseID,
		"{{image}}", payload.Image,
		"{{status}}", payload.Status,
		"{{error}}", payload.Error,
		"{{started_at}}", payload.StartedAt,
		"{{completed_at}}", payload.CompletedAt,
	}

	return strings.NewReplacer(replacements...).Replace(s)
}
