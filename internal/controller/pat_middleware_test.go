package controller

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
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
// signRequest signs a built request the way a real client does.
//
// Every test below needs this and none of them cares how it works, which is
// exactly why it is one function: six inline copies would drift, and a test
// that signs differently from the product proves nothing about the product.
func signRequest(req *http.Request, plain string) {
	signRequestAt(req, plain, time.Now().Unix())
}

// signRequestAt is for tests that pin the middleware's clock. Signing with
// time.Now() against a fixed clock is months of skew and a 401 that looks like
// the feature under test is broken.
func signRequestAt(req *http.Request, plain string, ts int64) {
	sum := sha256.Sum256([]byte(plain))
	id, _ := ParsePATToken(plain)
	// Random, not clock-derived. A nonce from time.Now() repeats inside a
	// tight loop, and the second request is then correctly refused as a
	// replay — which reads as a broken product and is a broken test.
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	nonce := hex.EncodeToString(buf)

	canonical := signatureCanonical(req.Method, req.URL.Path, req.URL.Query(), nil, ts, nonce)
	mac := hmac.New(sha256.New, []byte(hex.EncodeToString(sum[:])))
	mac.Write([]byte(canonical))

	req.Header.Set("Authorization", fmt.Sprintf("Voodu id=%s, ts=%d, nonce=%s, sig=%s",
		id, ts, nonce, base64.RawURLEncoding.EncodeToString(mac.Sum(nil))))
}

func TestAuthPAT_Matrix(t *testing.T) {
	store := newMemStore()
	plainRead, _ := seedTestPAT(t, store, []Scope{ScopeRead})
	plainActions, _ := seedTestPAT(t, store, []Scope{ScopeActions})
	auth := newPATAuthorizer(store, quietLogger())

	// build returns the Authorization header for a request the test is about
	// to make. Cases that want a BROKEN header return one directly.
	sign := func(plain string, mutate func(*signedRequest)) func(*http.Request) string {
		return func(req *http.Request) string {
			sum := sha256.Sum256([]byte(plain))
			id, _ := ParsePATToken(plain)
			parsed := signedRequest{
				ID:    id,
				TS:    time.Now().Unix(),
				Nonce: hex.EncodeToString([]byte(fmt.Sprintf("%016d", time.Now().UnixNano()))),
			}

			if mutate != nil {
				mutate(&parsed)
			}

			canonical := signatureCanonical(req.Method, req.URL.Path, req.URL.Query(), nil, parsed.TS, parsed.Nonce)
			mac := hmac.New(sha256.New, []byte(hex.EncodeToString(sum[:])))
			mac.Write([]byte(canonical))

			sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
			if parsed.Sig != "" {
				sig = parsed.Sig
			}

			return fmt.Sprintf("Voodu id=%s, ts=%d, nonce=%s, sig=%s", parsed.ID, parsed.TS, parsed.Nonce, sig)
		}
	}

	fixed := func(header string) func(*http.Request) string {
		return func(*http.Request) string { return header }
	}

	cases := []struct {
		name     string
		header   func(*http.Request) string
		want     Scope
		wantCode int
		wantNext bool
	}{
		{"no header", fixed(""), ScopeRead, http.StatusUnauthorized, false},

		// The old scheme is gone, not deprecated. A Bearer token is refused
		// even when the token itself is valid — which is the whole point:
		// a captured token is no longer a way in.
		{"bearer is no longer accepted", fixed("Bearer " + plainRead), ScopeRead, http.StatusUnauthorized, false},
		{"wrong scheme", fixed("Basic " + plainRead), ScopeRead, http.StatusUnauthorized, false},

		{"empty voodu", fixed("Voodu "), ScopeRead, http.StatusUnauthorized, false},
		{"missing sig", fixed("Voodu id=abc123, ts=1788000000, nonce=deadbeef"), ScopeRead, http.StatusUnauthorized, false},
		{"garbage ts", fixed("Voodu id=abc123, ts=nope, nonce=deadbeef, sig=x"), ScopeRead, http.StatusUnauthorized, false},

		{"unknown id", sign(plainRead, func(s *signedRequest) { s.ID = "ZZZZZZ" }), ScopeRead, http.StatusUnauthorized, false},
		{"forged signature", sign(plainRead, func(s *signedRequest) { s.Sig = "not-the-right-signature" }), ScopeRead, http.StatusUnauthorized, false},

		// Only possible under the new scheme, and the two reasons it exists.
		{"stale timestamp", sign(plainRead, func(s *signedRequest) { s.TS = time.Now().Add(-time.Hour).Unix() }), ScopeRead, http.StatusUnauthorized, false},
		{"timestamp from the future", sign(plainRead, func(s *signedRequest) { s.TS = time.Now().Add(time.Hour).Unix() }), ScopeRead, http.StatusUnauthorized, false},

		{"valid read PAT on read route", sign(plainRead, nil), ScopeRead, http.StatusOK, true},
		{"valid actions PAT on actions route", sign(plainActions, nil), ScopeActions, http.StatusOK, true},

		{"read PAT on actions route", sign(plainRead, nil), ScopeActions, http.StatusForbidden, false},
		{"actions PAT on read route", sign(plainActions, nil), ScopeRead, http.StatusForbidden, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/pat/v1/stats", nil)
			if header := c.header(req); header != "" {
				req.Header.Set("Authorization", header)
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
	signRequest(req, plain)

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
	signRequest(req2, plain)

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
	signRequest(req, plain)
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
	signRequest(req, plain+"X") // signed with the wrong key
	auth.Middleware(ScopeRead, nextOK(t, new(bool)))(httptest.NewRecorder(), req)

	// 2) Insufficient scope.
	req2 := httptest.NewRequest(http.MethodGet, "/api/pat/v1/stats", nil)
	signRequest(req2, plain)
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
		signRequestAt(req, plain, fixedNow.Unix())
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
