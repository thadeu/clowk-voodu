// pat_ratelimit.go is the per-PAT rate limiter applied to action
// endpoints (today: pod restart; future: any POST mutation). Lives
// downstream of authPAT in the middleware chain — it reads the PAT
// ID from context, so an un-authed request can't reach this code.
//
// Why per-PAT and not per-IP / per-resource:
//
//   - Per-IP: easy to bypass with NAT / VPN rotation, and we
//     already have the stable identity (the PAT itself).
//   - Per-resource: would let one PAT thrash N resources; the
//     plan's goal is "noisy operator can't DOS the controller",
//     and the PAT is the identity that maps to operator intent.
//
// Memory: LRU-capped at patRateLimitLRUMax entries. A misbehaving
// WebUI minting + revoking many PATs could grow the map; LRU
// keeps it bounded without thrashing the auth path.

package controller

import (
	"container/list"
	"fmt"
	"net/http"
	"sync"

	"golang.org/x/time/rate"
)

// patRateLimitLRUMax caps the in-memory limiter map. Past this
// cap, the oldest-used limiter is evicted. 1000 covers any
// realistic single-host PAT count by 10× — operators with more
// than 100 active PATs on one VM have a different problem.
const patRateLimitLRUMax = 1000

// patRateLimiter is the per-process rate limit gate for action
// endpoints. One instance per controller; the `Middleware` method
// returns a per-handler wrapper applied at route registration
// (downstream of authPAT).
//
// Tunable knobs (per-PAT bucket):
//   - r: refill rate, in tokens per second. e.g. rate.Limit(10.0/60.0)
//     for 10/min.
//   - burst: maximum tokens stored. e.g. 3 to allow a quick triple-
//     restart before the steady-state limit kicks in.
//
// Values are passed in via Config flags (see server.go).
type patRateLimiter struct {
	r     rate.Limit
	burst int

	mu       sync.Mutex
	byID     map[string]*list.Element // PAT ID → list element
	order    *list.List               // LRU: front = most-recently-used
	capacity int
}

type rateLimitEntry struct {
	id  string
	lim *rate.Limiter
}

// newPATRateLimiter constructs the per-process limiter. `r` and
// `burst` come from operator-supplied flags (--pat-action-rate /
// --pat-action-burst).
//
// Zero/negative `r` is the "unlimited" sentinel — the middleware
// becomes a no-op pass-through. Useful for development envs that
// don't want to think about rate limits.
func newPATRateLimiter(r rate.Limit, burst int) *patRateLimiter {
	return &patRateLimiter{
		r:        r,
		burst:    burst,
		byID:     map[string]*list.Element{},
		order:    list.New(),
		capacity: patRateLimitLRUMax,
	}
}

// Middleware wraps `next` so requests are rejected with 429 when
// the calling PAT exceeds its burst+rate budget. The PAT ID is
// pulled from ctx — set by authPAT upstream. A request that
// somehow reaches this middleware without an authed PAT in ctx
// is rejected with 500 (programming error — log loud).
func (l *patRateLimiter) Middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Zero/negative rate = unlimited. Skip the bucket lookup
		// entirely so dev envs don't pay the map allocation.
		if l.r <= 0 {
			next(w, r)

			return
		}

		id, ok := PATIDFromContext(r.Context())
		if !ok {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("rate limit: no PAT in context (middleware ordering bug)"))

			return
		}

		if !l.allow(id) {
			writeErr(w, http.StatusTooManyRequests, fmt.Errorf("rate limit exceeded for this PAT (burst %d, rate %v/sec)", l.burst, l.r))

			return
		}

		next(w, r)
	}
}

// allow checks (and consumes) one token from the PAT's bucket.
// Creates a fresh limiter on first call for an ID; LRU-evicts
// the oldest when capacity is hit.
func (l *patRateLimiter) allow(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if el, ok := l.byID[id]; ok {
		// Bump to front (most-recently-used).
		l.order.MoveToFront(el)

		entry, _ := el.Value.(*rateLimitEntry)

		return entry.lim.Allow()
	}

	// First request for this PAT — create a fresh bucket.
	lim := rate.NewLimiter(l.r, l.burst)
	entry := &rateLimitEntry{id: id, lim: lim}
	el := l.order.PushFront(entry)
	l.byID[id] = el

	// Evict oldest if we crossed the cap. Triggered AFTER the
	// fresh insert so a single overflow drops one neighbour
	// (rather than self-evicting the just-inserted entry).
	if l.order.Len() > l.capacity {
		oldest := l.order.Back()
		if oldest != nil {
			l.order.Remove(oldest)

			oldEntry, _ := oldest.Value.(*rateLimitEntry)

			delete(l.byID, oldEntry.id)
		}
	}

	return lim.Allow()
}
