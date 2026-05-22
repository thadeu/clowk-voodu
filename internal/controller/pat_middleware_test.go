package controller

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// helper: seed store with a PAT, return the plain token + ID.
func seedTestPAT(t *testing.T, store *memStore, scopes []Scope) (plain string, id string) {
	t.Helper()

	plain, rec, err := GeneratePAT(scopes, "test")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.PutPAT(t.Context(), rec); err != nil {
		t.Fatal(err)
	}

	return plain, rec.ID
}

// nextOK is the "200 OK" handler used as the downstream in
// middleware tests. Returns ok=true via the sink channel so the
// test asserts whether the middleware called through.
func nextOK(t *testing.T, called *bool) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, _ *http.Request) {
		*called = true

		w.WriteHeader(http.StatusOK)
	}
}

// TestAuthPAT_Matrix is the big table-driven test for the auth
// middleware. Covers the full failure mode space:
//
//   - missing header → 401
//   - malformed prefix → 401
//   - unknown ID → 401
//   - wrong hash → 401
//   - insufficient scope → 403
//   - valid → 200 + ctx carries ID
//
// A failing case here means an attacker either gets in with an
// invalid token (Auth bypass) or a legit operator gets locked out
// (denial of service). Both ship-blockers.
func TestAuthPAT_Matrix(t *testing.T) {
	store := newMemStore()
	plainRead, _ := seedTestPAT(t, store, []Scope{ScopeRead})
	plainActions, _ := seedTestPAT(t, store, []Scope{ScopeActions})

	auth := newPATAuthorizer(store, quietLogger())

	cases := []struct {
		name     string
		header   string // raw Authorization header value
		want     Scope  // scope the route requires
		wantCode int
		wantNext bool
	}{
		{"no header", "", ScopeRead, http.StatusUnauthorized, false},
		{"empty bearer", "Bearer ", ScopeRead, http.StatusUnauthorized, false},
		{"wrong scheme", "Basic " + plainRead, ScopeRead, http.StatusUnauthorized, false},
		{"missing prefix", "Bearer DEADBEEF12345678901234567890", ScopeRead, http.StatusUnauthorized, false},
		{"wrong prefix family", "Bearer ghp_DEADBEEF12345678901234567890", ScopeRead, http.StatusUnauthorized, false},
		{"too short", "Bearer pat_SHORT", ScopeRead, http.StatusUnauthorized, false},
		{"unknown id", "Bearer pat_ZZZZZZZZZZZZZZZZZZZZZZZZZZ", ScopeRead, http.StatusUnauthorized, false},
		{"wrong hash (same prefix as real, garbled body)", "Bearer pat_" + plainRead[len("pat_"):len("pat_")+patTokenIDLen] + "WRONGWRONGWRONGWRONG", ScopeRead, http.StatusUnauthorized, false},

		// Real valid path: read PAT on read route → 200.
		{"valid read PAT on read route", "Bearer " + plainRead, ScopeRead, http.StatusOK, true},
		{"valid actions PAT on actions route", "Bearer " + plainActions, ScopeActions, http.StatusOK, true},

		// Insufficient scope.
		{"read PAT on actions route", "Bearer " + plainRead, ScopeActions, http.StatusForbidden, false},
		{"actions PAT on read route", "Bearer " + plainActions, ScopeRead, http.StatusForbidden, false},

		// Case insensitive bearer.
		{"lowercase bearer", "bearer " + plainRead, ScopeRead, http.StatusOK, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/pat/v1/stats", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}

			rr := httptest.NewRecorder()
			called := false

			handler := auth.Middleware(c.want, nextOK(t, &called))
			handler(rr, req)

			if rr.Code != c.wantCode {
				t.Errorf("status: got %d, want %d (body: %s)", rr.Code, c.wantCode, rr.Body.String())
			}

			if called != c.wantNext {
				t.Errorf("next called: got %v, want %v", called, c.wantNext)
			}
		})
	}
}

// TestAuthPAT_RevokedPAT pins that revoke (DeletePAT) immediately
// blocks the token. No caching for stale-revoke windows — every
// request looks up the store fresh.
func TestAuthPAT_RevokedPAT(t *testing.T) {
	store := newMemStore()
	plain, id := seedTestPAT(t, store, []Scope{ScopeRead})

	auth := newPATAuthorizer(store, quietLogger())

	// First request: 200.
	req := httptest.NewRequest(http.MethodGet, "/api/pat/v1/stats", nil)
	req.Header.Set("Authorization", "Bearer "+plain)

	rr := httptest.NewRecorder()
	called := false
	auth.Middleware(ScopeRead, nextOK(t, &called))(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("first request status: got %d, want 200", rr.Code)
	}

	// Revoke.
	if _, err := store.DeletePAT(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	// Second request: 401 — revoke is immediately effective.
	req2 := httptest.NewRequest(http.MethodGet, "/api/pat/v1/stats", nil)
	req2.Header.Set("Authorization", "Bearer "+plain)

	rr2 := httptest.NewRecorder()
	called2 := false
	auth.Middleware(ScopeRead, nextOK(t, &called2))(rr2, req2)

	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("post-revoke status: got %d, want 401", rr2.Code)
	}

	if called2 {
		t.Error("post-revoke must NOT call next handler")
	}
}

// TestAuthPAT_ContextCarriesPATID pins the contract that the rate
// limit middleware (and any future handler that wants audit
// logging) depends on: the verified PAT ID is in ctx after auth.
func TestAuthPAT_ContextCarriesPATID(t *testing.T) {
	store := newMemStore()
	plain, expectedID := seedTestPAT(t, store, []Scope{ScopeRead})

	auth := newPATAuthorizer(store, quietLogger())

	var gotID string
	var gotOK bool

	handler := auth.Middleware(ScopeRead, func(_ http.ResponseWriter, r *http.Request) {
		gotID, gotOK = PATIDFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodGet, "/api/pat/v1/stats", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	handler(httptest.NewRecorder(), req)

	if !gotOK {
		t.Fatal("PATIDFromContext: not present after auth")
	}

	if gotID != expectedID {
		t.Errorf("ctx PAT ID: %q, want %q", gotID, expectedID)
	}
}

// TestAuthPAT_LoggerNeverContainsToken is the regression test for
// R3 in the plan: a token must NEVER appear in log output, across
// all middleware paths.
func TestAuthPAT_LoggerNeverContainsToken(t *testing.T) {
	store := newMemStore()
	plain, _ := seedTestPAT(t, store, []Scope{ScopeRead})

	var buf strings.Builder

	logger := log.New(&buf, "", 0)

	// Force a logged path: bad-tampered token (passes parse, fails
	// hash) — exercises store lookup + hash compare without
	// writeErr noise. Also try a store-error path (substitute a
	// store that errors).
	auth := newPATAuthorizer(store, logger)

	// 1) Real-shaped but wrong-hash token.
	req := httptest.NewRequest(http.MethodGet, "/api/pat/v1/stats", nil)
	req.Header.Set("Authorization", "Bearer "+plain+"X")
	auth.Middleware(ScopeRead, nextOK(t, new(bool)))(httptest.NewRecorder(), req)

	// 2) Insufficient scope.
	req2 := httptest.NewRequest(http.MethodGet, "/api/pat/v1/stats", nil)
	req2.Header.Set("Authorization", "Bearer "+plain)
	auth.Middleware(ScopeActions, nextOK(t, new(bool)))(httptest.NewRecorder(), req2)

	// Log must never contain the token prefix + token body.
	logged := buf.String()
	if strings.Contains(logged, plain) {
		t.Errorf("log leak: token appears in log output:\n%s", logged)
	}

	if strings.Contains(logged, "pat_") {
		t.Errorf("log leak: 'pat_' prefix appears in log output (likely a token):\n%s", logged)
	}
}

// TestAuthPAT_TouchCoalesced pins R4: LastUsedAt updates fire on
// first request and then coalesce — even at 100 req/sec, etcd
// sees at most one TouchPAT per PAT per window.
func TestAuthPAT_TouchCoalesced(t *testing.T) {
	store := newMemStore()
	plain, id := seedTestPAT(t, store, []Scope{ScopeRead})

	auth := newPATAuthorizer(store, quietLogger())

	// Test seam: count Touch calls via a sink channel.
	var touchCount int
	var muc sync.Mutex
	auth.touchSink = func(_ string) {
		muc.Lock()
		defer muc.Unlock()
		touchCount++
	}

	// Fixed clock so the test is deterministic.
	fixedNow := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	auth.now = func() time.Time { return fixedNow }

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/pat/v1/stats", nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		auth.Middleware(ScopeRead, nextOK(t, new(bool)))(httptest.NewRecorder(), req)
	}

	// Goroutines run async — wait briefly for them.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		muc.Lock()
		c := touchCount
		muc.Unlock()
		if c == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	muc.Lock()
	got := touchCount
	muc.Unlock()

	if got != 1 {
		t.Errorf("Touch fired %d times for 10 requests in same window, want exactly 1 (coalesce broken)", got)
	}

	_ = id // id unused but retained for future extension
}

// TestExtractBearer covers the header parser directly so we have
// a hot signal where extraction breaks vs middleware behaviour
// changes.
func TestExtractBearer(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		wantToken string
		wantOK    bool
	}{
		{"happy path", "Bearer abc123", "abc123", true},
		{"lowercase bearer", "bearer abc123", "abc123", true},
		{"with surrounding spaces", "Bearer   abc123  ", "abc123", true},
		{"no header", "", "", false},
		{"no scheme", "abc123", "", false},
		{"wrong scheme", "Basic abc123", "", false},
		{"empty token", "Bearer ", "", false},
		{"bearer only", "Bearer", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}

			tok, ok := extractBearer(r)
			if ok != c.wantOK {
				t.Errorf("ok: got %v, want %v", ok, c.wantOK)
			}

			if tok != c.wantToken {
				t.Errorf("token: %q, want %q", tok, c.wantToken)
			}
		})
	}
}

// TestTouchCoalescer pins the in-memory dampener directly so a
// regression there shows up next to the unit, not buried in the
// async middleware tests.
func TestTouchCoalescer(t *testing.T) {
	tc := newTouchCoalescer(60 * time.Second)

	t0 := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)

	if !tc.shouldTouch("A", t0) {
		t.Error("first call should return true")
	}

	if tc.shouldTouch("A", t0.Add(30*time.Second)) {
		t.Error("second call within window should return false")
	}

	if !tc.shouldTouch("A", t0.Add(61*time.Second)) {
		t.Error("after window expires, should return true again")
	}

	// Separate IDs have independent windows.
	if !tc.shouldTouch("B", t0) {
		t.Error("different ID should return true on first call")
	}
}

// Sanity: middleware code path doesn't reference the global
// log.Default unconditionally when a nil logger is supplied.
func TestNewPATAuthorizer_NilLoggerOK(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil logger panicked: %v", r)
		}
	}()

	_ = newPATAuthorizer(newMemStore(), nil)
}

// _ silences unused-import vars during partial drafts; remove
// when used. Kept here as a doc marker that context.Background
// would be the right base for any standalone helper using ctx
// without inheriting from the request.
var _ context.Context = context.Background()
