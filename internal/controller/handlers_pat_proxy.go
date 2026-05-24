// handlers_pat_proxy.go owns the `/api/pat/v1/*` plane wiring —
// the network-exposable observability surface the Rails WebUI
// consumes. These handlers are thin wrappers over the existing
// internal handlers (`handleStats`, `handlePods`, etc.) — no
// business logic lives here.
//
// ── Invariant: PAT proxies stay passthrough ──────────────────────
//
// Every handler in this file is either:
//
//   1. a VERBATIM passthrough to its orchestration-plane twin
//      (e.g. `a.handleStats(w, r)`), OR
//   2. limited to translating INPUT (e.g. path param → query
//      param, like `handlePATPodRestart` resolving a container
//      name into a (kind, scope, name) triple).
//
// Response bodies MUST be byte-identical between the two planes —
// same field names, same envelope, same timestamps, same handler-
// added query params. This is what guarantees that adding
// `?detail=true` to handlePods automatically lights up for both the
// CLI and the WebUI without coordinated changes, and what stops the
// wire shapes from silently drifting as features land.
//
// If a future PAT response NEEDS to differ from the orchestration
// one, the divergence belongs in the underlying handler behind a
// query param (e.g. `?for=pat`), not here. Keeping the proxy dumb
// is what lets us treat "CLI sees X" and "WebUI sees X" as the same
// statement.
//
// Auth and rate limit middleware are applied at route registration
// (PATHandler method below), not here, so the proxy handlers stay
// trivially testable in isolation.

package controller

import (
	"fmt"
	"log"
	"net/http"

	"golang.org/x/time/rate"
)

// PATHandler builds the second mux (served on PATAddr) wired with
// the auth middleware + rate limiter + only the routes the WebUI
// needs. Returns an http.Handler ready to be assigned to a second
// http.Server in Start.
//
// Layout (all routes prefixed `/api/pat/v1/`):
//
//	GET    stats                 read    — host + pod stats
//	GET    pods                  read    — pod listing (with degraded)
//	GET    pods/{name}           read    — single pod detail
//	GET    pods/{name}/logs      read    — chunked log stream
//	POST   pods/{name}/restart   actions — rolling restart, rate-limited
//
// Routes registered with `METHOD /path` net/http 1.22 routing
// syntax — same convention as Handler() uses for the orchestration
// plane. Missing routes default-fall through to net/http's 404.
//
// The middleware chain is unwound at registration time so the test
// in pat_middleware_test.go can audit the exact composition:
// `auth(scope, [rateLimit(]handler[)])`. No hidden indirection.
func (a *API) PATHandler(logger *log.Logger, actionRate float64, actionBurst int) http.Handler {
	mux := http.NewServeMux()

	auth := newPATAuthorizer(a.Store, logger)
	limiter := newPATRateLimiter(rate.Limit(actionRate), actionBurst)

	// Read endpoints.
	mux.HandleFunc("GET /api/pat/v1/stats",
		auth.Middleware(ScopeRead, a.handlePATStats))

	mux.HandleFunc("GET /api/pat/v1/system",
		auth.Middleware(ScopeRead, a.handlePATSystem))

	mux.HandleFunc("GET /api/pat/v1/pods",
		auth.Middleware(ScopeRead, a.handlePATPods))

	mux.HandleFunc("GET /api/pat/v1/pods/{name}",
		auth.Middleware(ScopeRead, a.handlePATPodDescribe))

	mux.HandleFunc("GET /api/pat/v1/pods/{name}/logs",
		auth.Middleware(ScopeRead, a.handlePATPodLogs))

	// Multi-pod tail (server-side fan-out across kind/scope/name).
	// Same handler as the orchestration plane — verbatim passthrough,
	// per the invariant at the top of this file.
	mux.HandleFunc("GET /api/pat/v1/logs",
		auth.Middleware(ScopeRead, a.handlePATLogsMulti))

	// Time-series metrics (M2). Verbatim passthrough — chart
	// queries read NDJSON the background Sampler writes; no
	// per-PAT scoping (read scope is enough). The handler caps
	// MaxBuckets/range itself so a malformed query can't fan out.
	mux.HandleFunc("GET /api/pat/v1/metrics",
		auth.Middleware(ScopeRead, a.handlePATMetrics))

	// Action endpoints — scope=actions + per-PAT rate limit.
	mux.HandleFunc("POST /api/pat/v1/pods/{name}/restart",
		auth.Middleware(ScopeActions, limiter.Middleware(a.handlePATPodRestart)))

	// Reuse the existing log-requests middleware so PAT-plane
	// requests get the same access log line shape as the
	// orchestration plane. logRequests audited: never logs the
	// Authorization header (regression test in pat_middleware_test.go).
	return logRequests(mux)
}

// handlePATStats is the proxy for the host/pod stats endpoint.
// Calls into the existing handleStats (which already produces
// the JSON envelope every other endpoint uses).
func (a *API) handlePATStats(w http.ResponseWriter, r *http.Request) {
	a.handleStats(w, r)
}

// handlePATSystem is the proxy for the host-level snapshot
// (CPU/memory/disk/net/uptime/kernel via gopsutil). Verbatim
// passthrough per the invariant at the top of this file —
// the WebUI sees byte-identical JSON to what a CLI `vd system`
// would consume against the orchestration plane.
func (a *API) handlePATSystem(w http.ResponseWriter, r *http.Request) {
	a.handleSystem(w, r)
}

// handlePATPods is the proxy for the pods list. Includes the
// `degraded` array (deployments/statefulsets with reconcile errors)
// — same shape as `vd get pods` consumes.
func (a *API) handlePATPods(w http.ResponseWriter, r *http.Request) {
	a.handlePods(w, r)
}

// handlePATPodDescribe is the proxy for the per-pod detail.
// `{name}` path param is consumed by handlePodDescribe directly
// (it reads r.PathValue("name")), so no rewriting needed here.
func (a *API) handlePATPodDescribe(w http.ResponseWriter, r *http.Request) {
	a.handlePodDescribe(w, r)
}

// handlePATPodLogs is the proxy for streaming pod logs. The
// underlying handler uses chunked transfer + Flusher; the
// authPAT middleware preserves the underlying ResponseWriter
// (no buffering wrapper) so Flush continues to work end-to-end.
func (a *API) handlePATPodLogs(w http.ResponseWriter, r *http.Request) {
	a.handlePodLogs(w, r)
}

// handlePATMetrics is the proxy for the time-series chart endpoint.
// Verbatim passthrough per the invariant at the top of this file —
// the WebUI sees byte-identical JSON to what a CLI `vd metrics`
// would consume against the orchestration plane.
func (a *API) handlePATMetrics(w http.ResponseWriter, r *http.Request) {
	a.handleMetrics(w, r)
}

// handlePATLogsMulti is the proxy for the server-side multi-pod
// log tail. Same chunked-transfer / Flusher contract as the
// single-pod variant — the underlying handler multiplexes per-pod
// streams and writes `[pod-name] ` prefixed lines to the response,
// flushing after each line so the WebUI sees them as they land.
func (a *API) handlePATLogsMulti(w http.ResponseWriter, r *http.Request) {
	a.handleLogsMulti(w, r)
}

// handlePATPodRestart is the proxy for triggering a rolling
// restart. The internal handleRestart expects query params
// (?kind=...&scope=...&name=...); the public PAT URL embeds
// the container name in the path. We resolve container → kind
// + scope + name via the pods lookup, then forward.
//
// This is the only proxy that does ANY work beyond pass-through:
// the WebUI deals in container names (what it sees in /pods),
// not in (kind, scope, resource-name) triples, so we translate.
func (a *API) handlePATPodRestart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("path: container name is required"))
		return
	}

	// Look up the container's (kind, scope, resource-name) by
	// asking the pods lister. The WebUI just got this name from
	// /api/pat/v1/pods, so it must exist in the lister's view.
	lister := a.Pods
	if lister == nil {
		lister = DockerPodsLister{}
	}

	pods, err := lister.ListPods()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("list pods for restart: %w", err))
		return
	}

	var match *Pod

	for i := range pods {
		if pods[i].Name == name {
			match = &pods[i]
			break
		}
	}

	if match == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no pod %q", name))
		return
	}

	if match.Kind != string(KindDeployment) && match.Kind != string(KindStatefulset) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("restart not supported for kind %q (only deployment/statefulset)", match.Kind))
		return
	}

	// Rewrite the request: add ?kind=&scope=&name= query and call
	// the existing handleRestart. URL is local to this request,
	// no shared state mutation.
	q := r.URL.Query()
	q.Set("kind", match.Kind)
	q.Set("scope", match.Scope)
	q.Set("name", match.ResourceName)

	r.URL.RawQuery = q.Encode()

	a.handleRestart(w, r)
}
