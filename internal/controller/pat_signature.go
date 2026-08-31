package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Verifying that a caller holds a PAT without it ever sending one.
//
// The PAT used to arrive whole in `Authorization: Bearer`, so a single
// intercepted request was total control of this controller — deploy, exec,
// logs. Now the caller signs what the request IS and the token stays on their
// side. An intercepted request is one request, good for a few minutes, and
// cannot be turned from a read into a restart.
//
// THE KEY IS WHAT WE ALREADY STORE. Callers hold the plain token and hash it;
// we hold that hash as PAT.HashHex. Both sides compute the same key, so no PAT
// had to be re-issued to turn this on.
//
// The trade-off, stated once: HashHex used to be useless to anyone who read it
// off disk, having no preimage. It can now forge requests to this controller.
// Whoever has this disk already has this controller, so the loss is small — but
// it is real, and it is why HashHex is no less secret than it ever was.
//
// This protects the CREDENTIAL, not the content: bodies and responses still
// travel in the clear. Only TLS fixes that.
//
// Must agree byte for byte with the two client implementations:
// voodu-webui app/services/voodu/signature.rb and
// voodu-webui gems/poller/src/client/signature.go. All three pin the same
// vector, because two of them disagreeing on query encoding surfaces as an
// intermittent 401 that gets blamed on the network.
const (
	sigScheme  = "Voodu"
	sigVersion = "v1"

	// Somebody else's clock is not ours to trust to the second.
	sigSkew = 5 * time.Minute

	// Bounded by the window: anything older cannot be replayed anyway. The cap
	// is a backstop against a flood of unique nonces pinning memory.
	sigNonceCap = 20000
)

// signedRequest is the parsed Authorization header.
type signedRequest struct {
	ID    string
	TS    int64
	Nonce string
	Sig   string
}

// parseSignatureHeader reads `Voodu id=…, ts=…, nonce=…, sig=…`.
//
// Returns ok=false for anything malformed rather than a reason: telling a
// caller which field was wrong tells an attacker the same thing.
func parseSignatureHeader(header string) (signedRequest, bool) {
	rest, found := strings.CutPrefix(header, sigScheme+" ")
	if !found {
		return signedRequest{}, false
	}

	parsed := signedRequest{}

	for _, part := range strings.Split(rest, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return signedRequest{}, false
		}

		switch key {
		case "id":
			parsed.ID = value
		case "ts":
			ts, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return signedRequest{}, false
			}

			parsed.TS = ts
		case "nonce":
			parsed.Nonce = value
		case "sig":
			parsed.Sig = value
		}
	}

	if parsed.ID == "" || parsed.TS == 0 || parsed.Nonce == "" || parsed.Sig == "" {
		return signedRequest{}, false
	}

	return parsed, true
}

// signatureCanonical rebuilds the string the caller signed.
//
// Body is passed in rather than read here: the middleware has to buffer it
// anyway so the handler can still read it, and hashing a consumed body is a
// bug that only shows up on the endpoints that have one.
func signatureCanonical(method, path string, query url.Values, body []byte, ts int64, nonce string) string {
	sum := sha256.Sum256(body)

	return strings.Join([]string{
		sigVersion,
		strings.ToUpper(method),
		path,
		canonicalQuery(query),
		hex.EncodeToString(sum[:]),
		strconv.FormatInt(ts, 10),
		nonce,
	}, "\n")
}

// Sorted by name then value, each side percent-encoded with RFC 3986
// unreserved characters left alone. Specified rather than delegated to
// url.Values.Encode(), which leaves repeated values in insertion order and
// escapes a space as `+`.
func canonicalQuery(query url.Values) string {
	if len(query) == 0 {
		return ""
	}

	pairs := make([]string, 0, len(query))

	for key, values := range query {
		for _, value := range values {
			pairs = append(pairs, escapeRFC3986(key)+"="+escapeRFC3986(value))
		}
	}

	sort.Strings(pairs)

	return strings.Join(pairs, "&")
}

func escapeRFC3986(value string) string {
	escaped := url.QueryEscape(value)
	escaped = strings.ReplaceAll(escaped, "+", "%20")

	return strings.NewReplacer("%7E", "~", "%2A", "*").Replace(escaped)
}

// verifySignature is the constant-time comparison. keyHex is PAT.HashHex.
func verifySignature(keyHex, canonical, provided string) bool {
	mac := hmac.New(sha256.New, []byte(keyHex))
	mac.Write([]byte(canonical))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

// drainBody buffers the request body so it can be both hashed and handled.
func drainBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}

	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()

	if err != nil {
		return nil, err
	}

	r.Body = io.NopCloser(strings.NewReader(string(body)))

	return body, nil
}

// nonceCache refuses a nonce it has already seen inside the clock window.
//
// In memory rather than etcd: this is one write per request against the store
// that coordinates the cluster, to stop a replay that must land within five
// minutes on the same node. A multi-node PAT plane would need shared state —
// there is one node today, and this comment is the warning for the day there
// is not.
type nonceCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newNonceCache() *nonceCache {
	return &nonceCache{seen: make(map[string]time.Time)}
}

// Use returns false when the nonce was already spent.
func (n *nonceCache) Use(nonce string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Evict on read. Anything older than the window cannot be replayed, so the
	// map never needs to grow past the traffic of one window.
	for key, at := range n.seen {
		if now.Sub(at) > sigSkew {
			delete(n.seen, key)
		}
	}

	if _, spent := n.seen[nonce]; spent {
		return false
	}

	if len(n.seen) >= sigNonceCap {
		return false
	}

	n.seen[nonce] = now

	return true
}
