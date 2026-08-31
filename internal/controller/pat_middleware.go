// pat_middleware.go owns the HTTP middleware that gates the
// observability plane (`/api/pat/v1/*`). One factory, `authPAT`,
// produces wrapped handlers — apply it per-route so the routing
// table stays the source of truth for "which endpoint requires
// which scope."
//
// Verification flow per request:
//
//  1. Parse `Authorization: Voodu id=…, ts=…, nonce=…, sig=…`. The token
//     itself never arrives — the caller proves it holds one instead, so an
//     intercepted request is one request rather than a credential.
//  2. Reject a timestamp outside the clock window, then look the record
//     up by the id in the header. Malformed
//     tokens get 401 with a generic message — don't tell an
//     attacker which side was wrong.
//  3. Store.GetPAT(signed.ID) — fetch the stored record. nil = 401.
//  4. Rebuild the canonical string from the request and verify the HMAC,
//     keyed on the stored hash. Constant-time. Mismatch = 401.
//     Mismatch = 401.
//  5. p.HasScope(want) — scope gate. Insufficient = 403 with
//     the scope it lacks (safe to disclose; it's the public
//     contract).
//  6. Inject the PAT ID into ctx so downstream middleware (rate
//     limit) and handlers can read it. Call next.
//  7. Touch LastUsedAt best-effort, coalesced.
//
// Failure paths log via the supplied logger but never include the
// token, header, or hash bytes. Regression test (R3 in the plan)
// pins this so a future refactor can't accidentally log secrets.

package controller

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"
)

// patAuthFailMessage is the canonical 401 body. Intentionally
// generic — same wording regardless of whether the prefix was
// wrong, the ID was unknown, or the hash didn't match. An
// attacker probing the surface can't distinguish "this ID
// exists" from "this ID is wrong" by reading errors.
const patAuthFailMessage = "invalid or expired token"

// touchCoalesceWindow is the minimum gap between LastUsedAt
// writes per PAT. Without this, a WebUI polling 5x/sec would
// fire 5 etcd writes per second per PAT — pure waste, since
// the operator only ever sees LastUsedAt at minute granularity
// in `vd pat list`.
//
// var, not const, so tests can shrink to verify the coalescing
// without sleeping a full minute.
var touchCoalesceWindow = time.Minute

// patIDCtxKey is the context.Context key used to pass the
// verified PAT ID from authPAT down to subsequent middleware
// (rate limit) and handlers. Unexported type so external
// packages can't collide.
type patIDCtxKey struct{}

// PATIDFromContext retrieves the verified PAT ID from ctx. The
// boolean reports whether the value was present — used by rate
// limit middleware that REQUIRES an upstream auth pass.
//
// Exported because rate limit middleware lives in a sibling file
// (pat_ratelimit.go) and needs to read the key.
func PATIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(patIDCtxKey{}).(string)

	return id, ok
}

// patAuthorizer wires the auth middleware to a Store + logger +
// touch dampener. One instance per controller process; the
// `Middleware(scope)` method returns a per-scope wrapper applied
// at route registration.
type patAuthorizer struct {
	store     Store
	logger    *log.Logger
	touch     *touchCoalescer
	now       func() time.Time // test seam — swap to a fixed clock in unit tests
	nonces    *nonceCache      // replay refusal, bounded by the clock window
	touchSink func(id string)  // optional — tests subscribe to verify async touch fired
}

// newPATAuthorizer constructs the per-process wrapper. Caller
// passes the Store (typically the shared EtcdStore wired in
// server.go) and a logger; the touch dampener is built fresh
// per authorizer so two test instances stay isolated.
func newPATAuthorizer(store Store, logger *log.Logger) *patAuthorizer {
	if logger == nil {
		logger = log.Default()
	}

	return &patAuthorizer{
		store:  store,
		logger: logger,
		touch:  newTouchCoalescer(touchCoalesceWindow),
		now:    time.Now,
		nonces: newNonceCache(),
	}
}

// Middleware returns an http.HandlerFunc that wraps `next` with
// PAT auth + the given scope assertion. Apply per-route at
// registration time:
//
//	mux.HandleFunc("/api/pat/v1/stats",
//	    auth.Middleware(ScopeRead, handleStats))
//
// Failing path responses are JSON-shaped per the rest of the
// controller (envelope `{"status": "error", "error": "..."}`).
func (a *patAuthorizer) Middleware(want Scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		signed, ok := parseSignatureHeader(r.Header.Get("Authorization"))
		if !ok {
			writeErr(w, http.StatusUnauthorized, errAuth())

			return
		}

		// Clock first: it is the cheapest check and the only failure an
		// operator can act on, so it should not be masked by a lookup.
		now := a.now()
		if skew := now.Sub(time.Unix(signed.TS, 0)); skew > sigSkew || skew < -sigSkew {
			writeErr(w, http.StatusUnauthorized, errAuth())

			return
		}

		pat, err := a.store.GetPAT(r.Context(), signed.ID)
		if err != nil {
			// Storage error is internal — don't leak it to the
			// caller. Log with a stable prefix so operators can
			// search for "/api/pat auth" in journald.
			a.logger.Printf("/api/pat auth: store lookup id=%s failed: %v", signed.ID, err)
			writeErr(w, http.StatusInternalServerError, errAuth())

			return
		}

		if pat == nil {
			writeErr(w, http.StatusUnauthorized, errAuth())

			return
		}

		// The body is buffered so it can be both hashed and handled — a
		// signature that did not cover it would let an interceptor rewrite
		// what a request does while keeping it valid.
		body, err := drainBody(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, errAuth())

			return
		}

		canonical := signatureCanonical(r.Method, r.URL.Path, r.URL.Query(), body, signed.TS, signed.Nonce)

		if !verifySignature(pat.HashHex, canonical, signed.Sig) {
			writeErr(w, http.StatusUnauthorized, errAuth())

			return
		}

		// Last, and only once the signature held: an attacker must not be able
		// to burn nonces they cannot sign for.
		if !a.nonces.Use(signed.Nonce, now) {
			writeErr(w, http.StatusUnauthorized, errAuth())

			return
		}

		if !pat.HasScope(want) {
			writeErr(w, http.StatusForbidden, errInsufficientScope(want))

			return
		}

		// Touch LastUsedAt best-effort. Coalesced — see
		// touchCoalesceWindow. Async so the request path doesn't
		// wait on etcd; failure logs and continues.
		if a.touch.shouldTouch(signed.ID, a.now()) {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				if err := a.store.TouchPAT(ctx, signed.ID, a.now()); err != nil {
					a.logger.Printf("/api/pat auth: TouchPAT id=%s failed: %v", signed.ID, err)
				}

				if a.touchSink != nil {
					a.touchSink(signed.ID)
				}
			}()
		}

		// Inject the PAT ID for downstream middleware (rate limit)
		// and handlers that want to attribute actions to the
		// authenticated identity (audit logging, for example).
		ctx := context.WithValue(r.Context(), patIDCtxKey{}, signed.ID)

		next(w, r.WithContext(ctx))
	}
}

// errAuth and errInsufficientScope are the canonical error
// constructors. Using funcs (not vars) so each call gets a fresh
// error value for callers that might wrap.

type patError struct{ msg string }

func (e patError) Error() string { return e.msg }

func errAuth() error { return patError{msg: patAuthFailMessage} }

func errInsufficientScope(want Scope) error {
	return patError{msg: "insufficient scope: requires " + string(want)}
}

// touchCoalescer dampens TouchPAT writes. shouldTouch returns
// true on first call for an id and not again within window.
//
// Memory: one entry per (active) PAT ID. The map is unbounded
// in theory; in practice it's bounded by the number of PATs in
// the store, which is itself bounded by operator action. No
// LRU eviction needed at the scales we care about.
type touchCoalescer struct {
	mu     sync.Mutex
	last   map[string]time.Time
	window time.Duration
}

func newTouchCoalescer(window time.Duration) *touchCoalescer {
	return &touchCoalescer{
		last:   map[string]time.Time{},
		window: window,
	}
}

func (tc *touchCoalescer) shouldTouch(id string, now time.Time) bool {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	prev, ok := tc.last[id]
	if ok && now.Sub(prev) < tc.window {
		return false
	}

	tc.last[id] = now

	return true
}
