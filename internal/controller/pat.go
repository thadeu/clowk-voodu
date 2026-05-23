// pat.go owns the pure-data side of Personal Access Tokens — the
// credential the WebUI uses to talk to the controller's observability
// plane (`/api/pat/v1/*`).
//
// Three responsibilities, all I/O-free:
//
//  1. Token shape: prefix + ID + secret, base62-encoded so it's
//     copy-paste safe in a terminal or a Rails env var and visually
//     matches the GitHub-family token convention operators already know.
//  2. Hashing: sha256(plain) hex. We store the hash, never the plain
//     token. The plain is shown ONCE at creation time, by the CLI.
//  3. Scope vocabulary: `read` (GET endpoints) vs `actions` (POST
//     mutations like restart). The middleware (pat_middleware.go)
//     consults this when gating requests.
//
// HTTP transport, storage CRUD, middleware, and rate limiting live in
// sibling files; this one is the canonical reference for "what IS a
// PAT, what does its wire format look like, and how do we verify one
// at the byte level."

package controller

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Scope is the coarse permission tag attached to every PAT. The
// auth middleware asserts the incoming PAT carries the scope a
// route requires; insufficient-scope requests get 403.
//
// Two scopes today (deliberately coarse — finer granularity adds
// implementation cost without a clear consumer need yet). Future
// scopes (e.g. `logs:read`, `pods:restart`) can be added; the
// vocabulary stays additive — existing PATs keep working.
type Scope string

const (
	// ScopeRead grants the GET endpoints on the observability plane:
	// stats, pods, pod describe, pod logs. Safe for read-only
	// dashboards and monitoring integrations.
	ScopeRead Scope = "read"

	// ScopeActions grants POST mutation endpoints — today that's
	// pod restart; future mutations (exec, scale, etc.) inherit
	// the same gate. Action endpoints are additionally rate-limited
	// per PAT (see pat_ratelimit.go) so a compromised token can't
	// be used as a runaway DOS vector.
	ScopeActions Scope = "actions"
)

// patTokenPrefix is the public prefix on every emitted token. Two
// reasons it's explicit rather than implicit:
//
//  1. Secret scanners (gitleaks, GitHub push-protection, TruffleHog)
//     recognise the `pat_` family and can flag accidental commits.
//  2. Operators reading logs / configs see "pat_..." and immediately
//     know it's a credential, not a UUID or hash.
//
// Kept short on purpose — operators read this in terminals and
// paste it into Rails env vars. `pat_` is the same length as
// GitHub's `ghp_` family without competing for the same namespace.
const patTokenPrefix = "pat_"

// patTokenIDLen is the length of the ID segment (first chars after
// the prefix). 6 chars × 6 bits/char = 36 bits — collision probability
// stays comfortable (~birthday-bound 300k PATs before 1-in-a-billion
// collision), and 6 chars renders cleanly in CLI tables.
//
// The ID is the "username half" of the token: public, indexable,
// safe to log. We use it as the etcd key (see PATKey in keys.go) so
// lookup is a single Get rather than a brute-force scan.
const patTokenIDLen = 6

// patTokenSecretLen is the length of the secret segment (everything
// after the ID). 22 chars × 6 bits/char = 132 bits of entropy. Brute
// force at 10^9 attempts/sec = ~10^31 seconds — computationally
// infeasible for the lifetime of the protocol.
const patTokenSecretLen = 22

// patTokenBodyLen is the total length of the random tail (ID + secret)
// in chars. Total token length = len(prefix) + patTokenBodyLen = 32.
const patTokenBodyLen = patTokenIDLen + patTokenSecretLen

// patRandomBytes is the number of cryptographic random bytes we draw
// before encoding. base64 packs 3 bytes into 4 chars perfectly aligned,
// so 21 bytes → exactly 28 chars (= patTokenBodyLen), no padding, no
// truncation, no rejection sampling. Any change to patTokenBodyLen
// must keep the multiple-of-4 alignment or the encoded body length
// will drift.
const patRandomBytes = patTokenBodyLen / 4 * 3

// patEncoder is the stdlib base64 URL-safe encoding without padding —
// RFC 4648 §5. Alphabet: A-Z a-z 0-9 - _ (64 chars), which gives the
// same mixed-case visual style as JWT tokens, GitHub PATs, and other
// modern API credentials. URL-safe (no `+` `/` `=` to escape) so the
// raw token can sit inside a URL, env var, or shell arg without
// quoting trouble.
//
// Using the stdlib directly keeps the encoder side trivial — no
// custom alphabet, no rejection-sampling loop. The whole "what does
// a token look like" decision compresses to this single var.
var patEncoder = base64.RawURLEncoding

// PAT is one stored token record. The plain token is NEVER stored —
// only HashHex is on disk. Middleware verifies by sha256-ing the
// incoming Bearer and constant-time comparing against HashHex.
//
// JSON-serialised into etcd under `/pats/<id>` (one key per record).
// Wire shape is stable; adding fields is forward-compatible.
type PAT struct {
	// ID is the public 8-char prefix-after-`pat_` of the plain token.
	// Stable across the lifetime of the PAT; safe to log + show in
	// `vd pat list` output. The "username half" of the token. Doubles
	// as the etcd key (`PATKey(id)` → `/pats/<id>`).
	ID string `json:"id"`

	// HashHex is sha256(plain token) lowercase hex, 64 chars long.
	// The "password half" — never logged, never returned by any
	// endpoint except the create-time response.
	//
	// Comparison MUST go through crypto/subtle.ConstantTimeCompare
	// in the verify path so timing side-channels don't leak the
	// hash bytewise.
	HashHex string `json:"hash_hex"`

	// Scopes attached to the PAT. Stored normalised (deduped, stable
	// order) so two PATs declaring `[read, actions]` vs
	// `[actions, read]` round-trip to identical JSON.
	Scopes []Scope `json:"scopes"`

	// Name is an operator-supplied label. Optional, free-form.
	// Useful for `vd pat list` ("webui-staging", "monitoring-bot")
	// when one host has multiple PATs.
	Name string `json:"name,omitempty"`

	// CreatedAt is the wall-clock at generation (UTC). Stable
	// across the PAT's lifetime; surfaces in `vd pat list` as a
	// relative duration.
	CreatedAt time.Time `json:"created_at"`

	// LastUsedAt is updated best-effort by the auth middleware
	// (coalesced to once/min per PAT — see pat_middleware.go's
	// touch dampener — to avoid etcd write thrash). Useful for
	// operators auditing "which tokens are stale and can be revoked".
	// Empty (zero time) means "never used since creation".
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

// HasScope reports whether p carries the required scope. Used by
// the auth middleware on every request after token verification.
//
// Linear scan is fine here — scope lists have 1-2 entries in
// practice; a map allocation would cost more than the loop.
func (p *PAT) HasScope(want Scope) bool {
	for _, s := range p.Scopes {
		if s == want {
			return true
		}
	}

	return false
}

// GeneratePAT mints a fresh token. Returns:
//
//   - plainToken: the full `pat_<28 chars>` string. The operator
//     sees this exactly ONCE — in the response to `vd pat create`.
//     Lost = revoke + remint.
//   - record: the persistable PAT shape, ready for store.PutPAT.
//     Carries the sha256 of the plain token, NOT the plain bytes.
//
// `scopes` must be non-empty and contain only valid scope values;
// duplicates are silently deduped. `name` is operator-supplied and
// trimmed; empty is fine (anonymous PATs are valid).
func GeneratePAT(scopes []Scope, name string) (plainToken string, record PAT, err error) {
	if len(scopes) == 0 {
		return "", PAT{}, fmt.Errorf("pat: at least one scope required (read|actions)")
	}

	for _, s := range scopes {
		if !validScope(s) {
			return "", PAT{}, fmt.Errorf("pat: unknown scope %q (valid: read, actions)", string(s))
		}
	}

	raw := make([]byte, patRandomBytes)

	if _, rerr := rand.Read(raw); rerr != nil {
		return "", PAT{}, fmt.Errorf("pat: read random: %w", rerr)
	}

	body := patEncoder.EncodeToString(raw)
	// Defensive — 21 raw bytes always encode to exactly 28 chars
	// (base64 packs 3:4, no padding requested). If this trips,
	// patRandomBytes or patTokenBodyLen drifted out of sync.
	if len(body) != patTokenBodyLen {
		return "", PAT{}, fmt.Errorf("pat: encoded body length %d, want %d", len(body), patTokenBodyLen)
	}

	plain := patTokenPrefix + body

	return plain, PAT{
		ID:        body[:patTokenIDLen],
		HashHex:   HashPAT(plain),
		Scopes:    normalizeScopes(scopes),
		Name:      strings.TrimSpace(name),
		CreatedAt: time.Now().UTC(),
	}, nil
}

// HashPAT returns sha256(plain) as lowercase hex. The canonical
// representation for storage and verification.
//
// Why sha256 (not bcrypt): tokens here have 130 bits of entropy —
// the token IS its own salt. bcrypt's cost-10 slowdown (~10ms per
// verify) would limit the controller to <100 PAT verifications per
// second per core, useless for a polling WebUI that hits 5-10 req/sec.
// sha256 runs in microseconds with identical security properties for
// this token size.
func HashPAT(plain string) string {
	sum := sha256.Sum256([]byte(plain))

	return hex.EncodeToString(sum[:])
}

// ParsePATToken extracts the ID from a plain token string. Returns
// ok=false on malformed input (wrong prefix, wrong total length, or
// any character outside the base64url alphabet). Caller (auth
// middleware) does the etcd lookup with `id`, then hashes the full
// `plain` and constant-time compares.
//
// Cheap (no allocations beyond the substring slice) so it's safe to
// call on every request without amortisation.
func ParsePATToken(plain string) (id string, ok bool) {
	if !strings.HasPrefix(plain, patTokenPrefix) {
		return "", false
	}

	body := strings.TrimPrefix(plain, patTokenPrefix)
	if len(body) != patTokenBodyLen {
		return "", false
	}

	// Reject anything outside the base64url alphabet up front —
	// keeps log injection / weird-byte garbage from ever reaching
	// the hash + etcd lookup path.
	for i := 0; i < len(body); i++ {
		if !isPATChar(body[i]) {
			return "", false
		}
	}

	return body[:patTokenIDLen], true
}

// isPATChar returns true for chars in the base64url alphabet:
// digits, uppercase letters, lowercase letters, `-`, `_`. Mirror of
// what patEncoder (base64.RawURLEncoding) emits.
func isPATChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c == '-' || c == '_':
		return true
	default:
		return false
	}
}

// ParseScopes parses a comma-separated scope list ("read,actions" or
// "read, actions") into a normalised []Scope. Used by the CLI's
// `--scope=read,actions` flag and by future API endpoints that
// accept scope strings.
//
// Returns an error when:
//   - input contains an unknown scope (fail loud — misconfigured
//     CLI flags shouldn't silently produce too-permissive PATs)
//   - input parses to zero scopes (empty/whitespace-only input)
//
// Duplicates within the input are deduped silently — `read,read`
// is equivalent to `read`.
func ParseScopes(s string) ([]Scope, error) {
	parts := strings.Split(s, ",")

	out := make([]Scope, 0, len(parts))
	seen := map[Scope]bool{}

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		sc := Scope(p)
		if !validScope(sc) {
			return nil, fmt.Errorf("pat: unknown scope %q (valid: read, actions)", p)
		}

		if seen[sc] {
			continue
		}

		seen[sc] = true
		out = append(out, sc)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("pat: no scopes parsed from %q (expected comma-separated read/actions)", s)
	}

	return normalizeScopes(out), nil
}

// validScope returns true for known scope values. Kept private so
// the only entry points are the typed constants above + ParseScopes
// (which normalises strings).
func validScope(s Scope) bool {
	switch s {
	case ScopeRead, ScopeActions:
		return true
	default:
		return false
	}
}

// normalizeScopes returns a deduped, deterministically-ordered copy
// of the input. Order is `read` before `actions` so the JSON wire
// shape is stable across two semantically-equivalent inputs.
//
// Hard-coded ordering rather than sort.Slice because the set is
// tiny (two scopes today, maybe four in a year). When the set grows,
// switch to sort.Strings on the underlying strings.
func normalizeScopes(in []Scope) []Scope {
	hasRead, hasActions := false, false

	for _, s := range in {
		switch s {
		case ScopeRead:
			hasRead = true
		case ScopeActions:
			hasActions = true
		}
	}

	out := make([]Scope, 0, 2)

	if hasRead {
		out = append(out, ScopeRead)
	}

	if hasActions {
		out = append(out, ScopeActions)
	}

	return out
}
