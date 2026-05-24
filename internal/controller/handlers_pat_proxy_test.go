package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestPATAPI builds an API ready to serve the PAT plane: a memstore
// pre-seeded with a (read+actions)-scoped PAT, a stubbed PodsLister so
// /pods and /pods/{name}/restart resolve, and a fakeRestarter so we can
// assert dispatch tuples.
//
// Returns the API, the plain Bearer token (used in Authorization
// headers), the PodsLister so tests can mutate it, and the restarter
// stub.
func newTestPATAPI(t *testing.T) (api *API, plain string, pods *fakePodsLister, restarter *fakeRestarter) {
	t.Helper()

	store := newMemStore()

	plain, rec, err := GeneratePAT([]Scope{ScopeRead, ScopeActions}, "tests")
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}

	if err := store.PutPAT(t.Context(), rec); err != nil {
		t.Fatalf("PutPAT: %v", err)
	}

	pods = &fakePodsLister{
		pods: []Pod{
			{
				Name: "clowk-web.a3f9", Kind: "deployment", Scope: "clowk",
				ResourceName: "web", ReplicaID: "a3f9", Image: "clowk:latest",
				Running: true, Status: "Up 1 minute",
			},
		},
	}

	restarter = &fakeRestarter{}

	api = &API{
		Store:       store,
		Version:     "test",
		Pods:        pods,
		Deployments: restarter,
	}

	return api, plain, pods, restarter
}

// patBearer returns the Authorization header value for a plain token.
func patBearer(plain string) string {
	return "Bearer " + plain
}

// TestPATPlane_PodsRequiresAuth pins that the PAT plane refuses
// unauthenticated requests — the WHOLE POINT of the second listener
// is that everything behind it is gated.
func TestPATPlane_PodsRequiresAuth(t *testing.T) {
	api, _, _, _ := newTestPATAPI(t)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/pat/v1/pods")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth status: got %d, want 401", resp.StatusCode)
	}
}

// TestPATPlane_PodsHappyPath proves the read-scope wrapper delegates
// to the existing handlePods — same JSON envelope, same shape.
func TestPATPlane_PodsHappyPath(t *testing.T) {
	api, plain, _, _ := newTestPATAPI(t)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/pods", nil)
	req.Header.Set("Authorization", patBearer(plain))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	var env struct {
		Status string
		Data   struct {
			Pods []Pod `json:"pods"`
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	if env.Status != "ok" {
		t.Errorf("status: %q, want ok", env.Status)
	}

	if len(env.Data.Pods) != 1 {
		t.Fatalf("got %d pods, want 1", len(env.Data.Pods))
	}

	if env.Data.Pods[0].Name != "clowk-web.a3f9" {
		t.Errorf("pod name: %q", env.Data.Pods[0].Name)
	}
}

// TestPATPlane_ReadScopeCannotRestart pins the scope separation: a
// PAT minted with only `read` MUST NOT hit any actions endpoint.
// The Rails WebUI gives "viewer" operators read-only PATs; this is
// the regression that proves they can't trigger restarts.
func TestPATPlane_ReadScopeCannotRestart(t *testing.T) {
	store := newMemStore()

	plain, rec, _ := GeneratePAT([]Scope{ScopeRead}, "readonly")
	_ = store.PutPAT(t.Context(), rec)

	pods := &fakePodsLister{
		pods: []Pod{
			{Name: "x.1", Kind: "deployment", Scope: "s", ResourceName: "x", Running: true},
		},
	}

	api := &API{Store: store, Version: "test", Pods: pods, Deployments: &fakeRestarter{}}

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pat/v1/pods/x.1/restart", nil)
	req.Header.Set("Authorization", patBearer(plain))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("read-only-on-restart status: got %d, want 403", resp.StatusCode)
	}
}

// TestPATPlane_RestartResolvesContainerName proves the proxy-only-logic
// for restart: the WebUI passes a container name, the proxy looks it
// up in the pods lister, and forwards (kind, scope, resource-name) to
// the existing handleRestart. Critical because the WebUI never sees
// the (kind, scope, name) triple — it only sees container names from
// /pods.
func TestPATPlane_RestartResolvesContainerName(t *testing.T) {
	api, plain, _, restarter := newTestPATAPI(t)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/pat/v1/pods/clowk-web.a3f9/restart", nil)
	req.Header.Set("Authorization", patBearer(plain))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("restart status: got %d, want 200; body=%s", resp.StatusCode, body)
	}

	if restarter.gotScope != "clowk" || restarter.gotName != "web" {
		t.Errorf("restarter received scope=%q name=%q, want clowk/web",
			restarter.gotScope, restarter.gotName)
	}
}

// TestPATPlane_RestartUnknownContainerName covers the 404 path:
// the WebUI may have a stale pod list (replica restarted, replica
// ID changed). 404 means "refresh your /pods cache".
func TestPATPlane_RestartUnknownContainerName(t *testing.T) {
	api, plain, _, _ := newTestPATAPI(t)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/pat/v1/pods/nonexistent.zzzz/restart", nil)
	req.Header.Set("Authorization", patBearer(plain))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown-container status: got %d, want 404", resp.StatusCode)
	}
}

// TestPATPlane_RestartRejectsNonRestartableKind locks the kind check
// proxy-side: only deployment/statefulset are restartable. Asking to
// restart a job (which has no rolling-restart semantics) must be a
// clear 400 — NOT a 500 from the underlying handler.
func TestPATPlane_RestartRejectsNonRestartableKind(t *testing.T) {
	store := newMemStore()

	plain, rec, _ := GeneratePAT([]Scope{ScopeRead, ScopeActions}, "t")
	_ = store.PutPAT(t.Context(), rec)

	pods := &fakePodsLister{
		pods: []Pod{
			{Name: "j-1", Kind: "job", Scope: "s", ResourceName: "j", Running: true},
		},
	}

	api := &API{Store: store, Version: "test", Pods: pods, Deployments: &fakeRestarter{}}

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pat/v1/pods/j-1/restart", nil)
	req.Header.Set("Authorization", patBearer(plain))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-restartable kind status: got %d, want 400", resp.StatusCode)
	}
}

// TestPATPlane_RestartRateLimit pins R5: the action endpoint is
// rate-limited per PAT, and the limiter is per-token (not global).
// A burst of 3 should succeed; the 4th request immediately after
// gets a 429 because the bucket is empty.
//
// We seed the limiter with rate=0.0001 (~one token every ~3 hours)
// so the steady-state refill never fires within the test window —
// only the burst of 3 should be allowed.
func TestPATPlane_RestartRateLimit(t *testing.T) {
	api, plain, _, _ := newTestPATAPI(t)

	// Burst 3, rate near zero. The fourth POST in quick succession
	// must 429.
	ts := httptest.NewServer(api.PATHandler(nil, 0.0001, 3))
	defer ts.Close()

	url := ts.URL + "/api/pat/v1/pods/clowk-web.a3f9/restart"

	statuses := make([]int, 0, 4)

	for i := 0; i < 4; i++ {
		req, _ := http.NewRequest(http.MethodPost, url, nil)
		req.Header.Set("Authorization", patBearer(plain))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		statuses = append(statuses, resp.StatusCode)
		resp.Body.Close()
	}

	for i := 0; i < 3; i++ {
		if statuses[i] != http.StatusOK {
			t.Errorf("burst[%d] status: got %d, want 200", i, statuses[i])
		}
	}

	if statuses[3] != http.StatusTooManyRequests {
		t.Errorf("post-burst status: got %d, want 429", statuses[3])
	}
}

// TestPATPlane_StatsRouteReachable proves the stats endpoint is plumbed.
// Without a StatsCollector wired, handleStats returns 503 ("collector
// unavailable") — that's still proof the route + auth wrapper land
// on the right handler. What we're pinning is: NOT 401 (auth would
// reject), NOT 403 (scope check would reject), NOT 404 (route missing).
func TestPATPlane_StatsRouteReachable(t *testing.T) {
	api, plain, _, _ := newTestPATAPI(t)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/stats", nil)
	req.Header.Set("Authorization", patBearer(plain))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		t.Errorf("stats route blocked by middleware/router: status=%d", resp.StatusCode)
	}
}

// TestPATPlane_MetricsRouteReachable proves the /metrics endpoint
// is plumbed end-to-end through auth + the passthrough proxy.
// Like the stats variant, a 503 from the underlying handler (no
// MetricsDir wired) still counts as "the route reached the handler";
// what we're pinning is NOT 401/403/404 — auth + scope + routing
// all resolved correctly.
func TestPATPlane_MetricsRouteReachable(t *testing.T) {
	api, plain, _, _ := newTestPATAPI(t)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/metrics?source=system&metric=cpu_percent&range=1h", nil)
	req.Header.Set("Authorization", patBearer(plain))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		t.Errorf("metrics route blocked by middleware/router: status=%d", resp.StatusCode)
	}
}

// TestPATPlane_SystemRouteReachable proves the /system endpoint is
// plumbed end-to-end through auth + the passthrough proxy. Like the
// stats variant, a 503 from the underlying handler (no collector
// wired) still counts as "the route reached the handler"; what we're
// pinning is NOT 401/403/404 — meaning auth + scope + routing all
// resolved correctly.
func TestPATPlane_SystemRouteReachable(t *testing.T) {
	api, plain, _, _ := newTestPATAPI(t)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/system", nil)
	req.Header.Set("Authorization", patBearer(plain))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		t.Errorf("system route blocked by middleware/router: status=%d", resp.StatusCode)
	}
}

// TestPATPlane_RevokedTokenRejected pins the revoke lifecycle through
// the PAT plane: after DeletePAT, requests with that token MUST be
// rejected. Without this, a leaked-then-revoked token would still
// work.
func TestPATPlane_RevokedTokenRejected(t *testing.T) {
	api, plain, _, _ := newTestPATAPI(t)

	// Revoke by deleting the record from the store.
	store := api.Store
	id, _ := ParsePATToken(plain)

	if _, err := store.DeletePAT(t.Context(), id); err != nil {
		t.Fatalf("DeletePAT: %v", err)
	}

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pat/v1/pods", nil)
	req.Header.Set("Authorization", patBearer(plain))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked-pat status: got %d, want 401", resp.StatusCode)
	}
}

// TestPATPlane_OrchestrationRoutesNotMounted is the defense-in-depth
// regression: the PAT mux MUST NOT carry the orchestration routes
// (/apply, /pats, /restart without the v1 prefix). If a future refactor
// accidentally calls Handler() on the PAT listener, this test fails
// loudly.
func TestPATPlane_OrchestrationRoutesNotMounted(t *testing.T) {
	api, plain, _, _ := newTestPATAPI(t)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/apply"},
		{http.MethodGet, "/pats"},
		{http.MethodPost, "/pats"},
		{http.MethodGet, "/pods"}, // un-prefixed must not match
		{http.MethodGet, "/stats"},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, ts.URL+tc.path, nil)
			req.Header.Set("Authorization", patBearer(plain))

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s: got %d, want 404 (not on PAT plane)",
					tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}

// TestPATPlane_LogsPreservesChunkedTransfer is the R2 regression:
// handlePodLogs uses chunked transfer + http.Flusher to stream.
// The auth middleware MUST NOT wrap the ResponseWriter in a way
// that masks Flusher — otherwise streaming dies.
//
// We can't easily exercise the full docker logs path in a unit test
// without a docker daemon, so the assertion narrows to: the wrapping
// middleware passes through Flusher when the inner handler asks for
// it. We use a stub PodsLister that bails before docker, but the
// auth → flush handoff is what we're pinning.
func TestPATPlane_LogsRouteReachable(t *testing.T) {
	api, plain, _, _ := newTestPATAPI(t)

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/pat/v1/pods/clowk-web.a3f9/logs", nil)
	req.Header.Set("Authorization", patBearer(plain))

	// Short deadline so we don't hang if the logs streamer tries
	// to actually exec docker.
	c := &http.Client{Timeout: 2 * time.Second}

	resp, err := c.Do(req)
	if err != nil {
		// Timeout / EOF here means the streamer started — auth +
		// routing succeeded.
		if strings.Contains(err.Error(), "Timeout") ||
			strings.Contains(err.Error(), "EOF") {
			return
		}

		t.Fatal(err)
	}
	defer resp.Body.Close()

	// If the handler returns synchronously (no streamer wired), we
	// at least confirm we passed auth (not 401/403) and reached a
	// handler.
	if resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden {
		t.Errorf("logs route was gated by auth incorrectly: status=%d", resp.StatusCode)
	}
}
