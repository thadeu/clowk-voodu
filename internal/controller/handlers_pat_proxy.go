// handlers_pat_proxy.go owns the `/api/pat/v1/*` plane wiring —
// the network-exposable observability surface the Rails WebUI
// consumes. These handlers are thin wrappers over the existing
// internal handlers (`handleStats`, `handlePods`, etc.) — no
// business logic lives here. The wrap layer:
//
//  1. Reshapes path params (the public URL uses /api/pat/v1/pods/{name},
//     the internal handler expects the same — pass-through).
//  2. Doesn't add response transformation (the JSON envelope shape
//     stays identical to the orchestration plane).
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

	mux.HandleFunc("GET /api/pat/v1/pods",
		auth.Middleware(ScopeRead, a.handlePATPods))

	mux.HandleFunc("GET /api/pat/v1/pods/{name}",
		auth.Middleware(ScopeRead, a.handlePATPodDescribe))

	mux.HandleFunc("GET /api/pat/v1/pods/{name}/logs",
		auth.Middleware(ScopeRead, a.handlePATPodLogs))

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
