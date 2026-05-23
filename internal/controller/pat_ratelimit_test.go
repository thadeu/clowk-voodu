package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"golang.org/x/time/rate"
)

// rateLimitNextOK is the downstream handler returning 200 if
// invoked. Tests assert middleware reject paths by checking the
// "called" flag stays false.
func rateLimitNextOK(called *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	}
}

// withPATID returns an http.Request whose context carries the
// given PAT ID — mirrors what authPAT sets upstream. Tests use
// this to simulate the "auth already passed" precondition.
func withPATID(req *http.Request, id string) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), patIDCtxKey{}, id))
}

// TestRateLimit_BurstAndSteady pins the token-bucket behaviour:
//
//   - first `burst` requests pass instantly
//   - beyond burst, requests are rejected until the refill rate
//     produces a fresh token
//
// Configured with a stupid-low rate (effectively zero) so refill
// doesn't interfere with the burst-exhaustion assertion.
func TestRateLimit_BurstAndSteady(t *testing.T) {
	// Rate: ~0 tokens/sec. Burst: 3.
	// Three should pass, the fourth+ should fail (no refill yet).
	limiter := newPATRateLimiter(rate.Limit(0.01), 3)

	called := 0

	for i := 0; i < 5; i++ {
		req := withPATID(httptest.NewRequest(http.MethodPost, "/api/pat/v1/pods/x/restart", nil), "PATABCD1")
		rr := httptest.NewRecorder()
		dummy := false

		limiter.Middleware(func(w http.ResponseWriter, r *http.Request) {
			called++
			rateLimitNextOK(&dummy)(w, r)
		}).ServeHTTP(rr, req)

		switch i {
		case 0, 1, 2:
			if rr.Code != http.StatusOK {
				t.Errorf("burst[%d]: got %d, want 200", i, rr.Code)
			}
		default:
			if rr.Code != http.StatusTooManyRequests {
				t.Errorf("beyond burst[%d]: got %d, want 429", i, rr.Code)
			}
		}
	}

	if called != 3 {
		t.Errorf("next called %d times, want 3 (3 burst tokens consumed)", called)
	}
}

// TestRateLimit_PerPATIsolation pins that distinct PATs have
// independent buckets. A noisy WebUI exhausting its own bucket
// must NOT block a separate operator's CLI.
func TestRateLimit_PerPATIsolation(t *testing.T) {
	limiter := newPATRateLimiter(rate.Limit(0.01), 1)

	doReq := func(patID string) int {
		req := withPATID(httptest.NewRequest(http.MethodPost, "/r", nil), patID)
		rr := httptest.NewRecorder()
		dummy := false
		limiter.Middleware(rateLimitNextOK(&dummy)).ServeHTTP(rr, req)
		return rr.Code
	}

	if code := doReq("A"); code != http.StatusOK {
		t.Errorf("PAT A first req: got %d, want 200", code)
	}

	if code := doReq("A"); code != http.StatusTooManyRequests {
		t.Errorf("PAT A second req: got %d, want 429", code)
	}

	// PAT B fresh — independent budget.
	if code := doReq("B"); code != http.StatusOK {
		t.Errorf("PAT B first req (after A exhausted): got %d, want 200", code)
	}
}

// TestRateLimit_NoIDInContext is the defensive guard: if a
// programming error wires rate limit BEFORE auth (or skips auth
// entirely), the middleware must NOT silently allow through.
// Return 500 so the misconfig is loud.
func TestRateLimit_NoIDInContext(t *testing.T) {
	limiter := newPATRateLimiter(rate.Limit(10), 3)

	req := httptest.NewRequest(http.MethodPost, "/r", nil)
	rr := httptest.NewRecorder()
	called := false

	limiter.Middleware(rateLimitNextOK(&called)).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500 (no PAT in ctx is a programming error)", rr.Code)
	}

	if called {
		t.Error("next must NOT be called when PAT ID is missing")
	}
}

// TestRateLimit_ZeroRateIsUnlimited pins the "disable rate limit"
// escape hatch. Dev envs that don't want budgets pass rate=0.
func TestRateLimit_ZeroRateIsUnlimited(t *testing.T) {
	limiter := newPATRateLimiter(rate.Limit(0), 0)

	for i := 0; i < 50; i++ {
		req := withPATID(httptest.NewRequest(http.MethodPost, "/r", nil), "A")
		rr := httptest.NewRecorder()
		dummy := false
		limiter.Middleware(rateLimitNextOK(&dummy)).ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("req[%d]: got %d, want 200 (rate=0 should be unlimited)", i, rr.Code)
		}
	}
}

// TestRateLimit_LRUEviction pins memory boundedness. Past
// capacity, the oldest entry is evicted; an evicted PAT gets a
// fresh limiter on next use (its burst budget resets).
func TestRateLimit_LRUEviction(t *testing.T) {
	limiter := newPATRateLimiter(rate.Limit(0.01), 1)
	limiter.capacity = 3 // shrink for testability

	hit := func(patID string) int {
		req := withPATID(httptest.NewRequest(http.MethodPost, "/r", nil), patID)
		rr := httptest.NewRecorder()
		dummy := false
		limiter.Middleware(rateLimitNextOK(&dummy)).ServeHTTP(rr, req)
		return rr.Code
	}

	// Fill 3 PATs (capacity=3).
	for i := 0; i < 3; i++ {
		if hit("P"+strconv.Itoa(i)) != http.StatusOK {
			t.Fatalf("seeding P%d failed", i)
		}
	}

	// All three burst-exhausted now.
	for i := 0; i < 3; i++ {
		if hit("P"+strconv.Itoa(i)) != http.StatusTooManyRequests {
			t.Errorf("P%d second hit: expected 429 (burst exhausted)", i)
		}
	}

	// Adding a 4th evicts P0 (oldest).
	if hit("P3") != http.StatusOK {
		t.Error("P3 first hit: expected 200")
	}

	// P0 — evicted — gets a fresh limiter on next access. So this
	// hit succeeds despite the previous attempt's exhaustion.
	if hit("P0") != http.StatusOK {
		t.Error("P0 after eviction: expected 200 (fresh limiter)")
	}
}
