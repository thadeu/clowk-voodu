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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
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

	// --- on_probe extensions (omitempty on the wire so on_deploy
	// payloads stay byte-identical to pre-existing receivers) ---

	// Pod is the full ordinal-stable container name (e.g.
	// `myorg-web-1`, `data-pg-2`). Populated for on_probe events
	// only; empty for on_deploy events. Useful for receiver-side
	// routing ("page only when pod-0 fails") and `vd logs <pod>`
	// follow-up.
	Pod string `json:"pod,omitempty"`

	// Probe identifies which probe transitioned: "liveness",
	// "readiness", or "startup". Receivers format alerts
	// differently per probe — liveness fail means container will
	// restart, readiness fail means caddy drops it from rotation,
	// startup fail means it never came up.
	Probe string `json:"probe,omitempty"`

	// Transition is "failure" or "recovery" for on_probe. Distinct
	// from Status (which is the on_deploy "success"/"failure"
	// vocabulary) so receivers can branch on either without
	// ambiguity.
	Transition string `json:"transition,omitempty"`

	// TransitionID is a deterministic dedup key derived from the
	// transition's intrinsic identity (scope|name|pod|probe|
	// to_phase|timestamp-truncated-to-1s). Receivers dedupe on
	// this when retries or backpressure produce duplicate fires.
	TransitionID string `json:"transition_id,omitempty"`

	// Reason carries the probe runner's last Result.Reason —
	// HTTP code / exec exit / TCP connect failure text. Separate
	// from Error (which is the deploy-level error) so receivers
	// know which context they're in. Empty for on_deploy events.
	Reason string `json:"reason,omitempty"`

	// Timestamp is the RFC3339 wall-clock of the transition.
	// Populated for on_probe events; on_deploy uses StartedAt /
	// CompletedAt instead.
	Timestamp string `json:"timestamp,omitempty"`
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

	identityRoot := fmt.Sprintf("%s/%s/%s on_deploy", kind, scope, name)
	fireWebhooks(poster, logf, targets, payload, identityRoot, status)
}

// fireWebhooks is the shared fan-out + per-target retry primitive,
// extracted from fireDeployWebhook so on_probe (and any future hook
// kind) reuse the same retry/template/log-correlation behaviour.
// Caller is responsible for picking the right slot (success/failure
// for on_deploy, failure/recovery for on_probe) and constructing
// the kind-specific payload.
//
// `identityRoot` is the resource-scoped log prefix
// (e.g. "deployment/prod/api on_deploy" or
// "statefulset/data/db/data-db-2 on_probe"). `slotLabel` is the
// per-target log tag — "success" / "failure" / "recovery" — paired
// with an index suffix when the slot has multiple targets.
func fireWebhooks(
	poster WebhookPoster,
	logf func(string, ...any),
	targets []deployWebhookWireSpec,
	payload WebhookPayload,
	identityRoot, slotLabel string,
) {
	if poster == nil || len(targets) == 0 {
		return
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
		ident := slotLabel
		if total > 1 {
			ident = fmt.Sprintf("%s[%d/%d]", slotLabel, i+1, total)
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
				logf("%s webhook (%s) body build failed: %v", identityRoot, ident, err)
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
					logf("%s webhook (%s) dropped after retries: %v", identityRoot, ident, err)
				}
			}
		}(wt, ident)
	}
}

// ProbeTransitionEvent is the input shape for probe-triggered
// webhook firing. The probe registry constructs one of these for
// every healthy↔unhealthy edge the runner detects, then calls
// fireProbeWebhook with the cached on_probe spec for the resource.
//
// Translation from the probe-runner's internal phase vocabulary to
// the operator-facing "failure" / "recovery" labels happens at the
// registry layer — by the time this event lands here, Transition is
// already normalised.
type ProbeTransitionEvent struct {
	Kind       string    // "deployment" | "statefulset"; filled by handler (registry doesn't know kind)
	Scope      string    // resource scope
	Name       string    // resource name
	Pod        string    // ordinal-stable container name
	Probe      string    // "liveness" | "readiness" | "startup"
	Transition string    // "failure" | "recovery"
	Reason     string    // probe Result.Reason; may be empty
	At         time.Time // wall-clock of the transition

	// OnProbe is the per-runner cached webhook spec, threaded
	// through from runnerEntry so the handler's OnProbeTransition
	// impl doesn't have to re-load the manifest from the store.
	// Nil for resources without on_probe declared (in which case
	// fireProbeWebhook short-circuits).
	OnProbe *onProbeWireSpec
}

// fireProbeWebhook fires the operator-supplied on_probe webhooks
// for a single probe transition. Picks the failure or recovery
// slot based on ev.Transition, computes a deterministic
// TransitionID, and short-circuits when the same id was already
// fired within the dedup window.
//
// nil poster or nil spec → no-op (steady state for resources
// without on_probe declared).
func fireProbeWebhook(poster WebhookPoster, logf func(string, ...any), spec *onProbeWireSpec, ev ProbeTransitionEvent) {
	if poster == nil || spec == nil {
		return
	}

	var targets []deployWebhookWireSpec

	switch ev.Transition {
	case "failure":
		targets = spec.Failure
	case "recovery":
		targets = spec.Recovery
	default:
		return
	}

	if len(targets) == 0 {
		return
	}

	transitionID := computeTransitionID(ev)

	// In-firer dedup: same transition emitted multiple times
	// within the dedup window fires the webhook only once.
	// Defends against probe-event races on controller restart
	// (initial state push + first edge can produce overlapping
	// signals) and any future caller that double-invokes.
	if !markFiredOnce(transitionID) {
		return
	}

	payload := WebhookPayload{
		Kind:         ev.Kind,
		Scope:        ev.Scope,
		Name:         ev.Name,
		Pod:          ev.Pod,
		Probe:        ev.Probe,
		Transition:   ev.Transition,
		TransitionID: transitionID,
		Reason:       ev.Reason,
		Timestamp:    ev.At.UTC().Format(time.RFC3339),
	}

	identityRoot := fmt.Sprintf("%s/%s/%s/%s on_probe", ev.Kind, ev.Scope, ev.Name, ev.Pod)
	fireWebhooks(poster, logf, targets, payload, identityRoot, ev.Transition)
}

// computeTransitionID returns a deterministic 12-char dedup key
// derived from the transition's intrinsic identity. Time is
// truncated to 1 second so retries within the same second produce
// the same id (receiver-side dedup safe), while transitions across
// the second boundary get distinct ids.
//
// sha256 prefix is overkill for collision resistance at the rates
// probes fire (one per period per pod), but the cost is negligible
// and the deterministic-from-inputs property is what matters.
func computeTransitionID(ev ProbeTransitionEvent) string {
	sec := ev.At.Truncate(time.Second).Unix()
	src := fmt.Sprintf("%s|%s|%s|%s|%s|%d", ev.Scope, ev.Name, ev.Pod, ev.Probe, ev.Transition, sec)
	sum := sha256.Sum256([]byte(src))

	return hex.EncodeToString(sum[:])[:12]
}

// transitionCacheTTL is the in-firer dedup window. 60s is long
// enough to absorb retries from the postWithRetry loop (~36s
// worst-case wall-clock) without holding entries indefinitely.
// var (not const) so tests can shrink it.
var transitionCacheTTL = 60 * time.Second

// transitionCache is the in-firer dedup map. Sync-protected via
// the mutex; entries are garbage-collected lazily on each
// markFiredOnce call (cheap because the cache is bounded by the
// rate of unique transitions, which is small).
var (
	transitionCacheMu sync.Mutex
	transitionCache   = make(map[string]time.Time)
)

// markFiredOnce returns true if the transitionID hasn't been seen
// in the last transitionCacheTTL. Concurrent callers see at most
// one true return per id per window.
func markFiredOnce(transitionID string) bool {
	now := time.Now()
	cutoff := now.Add(-transitionCacheTTL)

	transitionCacheMu.Lock()
	defer transitionCacheMu.Unlock()

	// Lazy GC of stale entries on every call. The map size is
	// proportional to the number of unique transitions within the
	// TTL window; for typical probe periods (1-30s) this stays in
	// the dozens range across an entire fleet.
	for id, t := range transitionCache {
		if t.Before(cutoff) {
			delete(transitionCache, id)
		}
	}

	if _, ok := transitionCache[transitionID]; ok {
		return false
	}

	transitionCache[transitionID] = now

	return true
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

		// on_probe tokens — empty values just collapse to "" via
		// strings.NewReplacer, so on_deploy bodies that use the new
		// tokens accidentally won't error out, just render as empty
		// substrings.
		"{{pod}}", payload.Pod,
		"{{probe}}", payload.Probe,
		"{{transition}}", payload.Transition,
		"{{transition_id}}", payload.TransitionID,
		"{{reason}}", payload.Reason,
		"{{timestamp}}", payload.Timestamp,
	}

	return strings.NewReplacer(replacements...).Replace(s)
}
