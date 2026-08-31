package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"testing"
	"time"
)

// The signature is produced by three independent implementations — this one,
// voodu-webui's Ruby client, and voodu-webui's Go poller. They agree or they
// do not, and disagreement surfaces as an intermittent 401 in production that
// gets blamed on the network.
//
// So the vector is pinned identically in all three suites. Change the canonical
// string and all three fail, which is the intended cost of changing it.
const vectorPAT = "pat_Ab3-_xYz01234567890123456789"

// The key is the PAT's sha256 hex — the same string this controller stores as
// PAT.HashHex, which is what makes the scheme free to adopt.
func signVector(pat, canonical string) string {
	sum := sha256.Sum256([]byte(pat))

	mac := hmac.New(sha256.New, []byte(hex.EncodeToString(sum[:])))
	mac.Write([]byte(canonical))

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestSignatureVector_Simple(t *testing.T) {
	canonical := signatureCanonical("GET", "/api/pat/v1/pods", nil, nil,
		1788000000, "00000000000000000000000000000000")

	const want = "4sepjmSvGAV5neJHO_ABgV80oodUrAfMdyaCb2IEQYE"

	if got := signVector(vectorPAT, canonical); got != want {
		t.Errorf("signature drift.\n got %s\nwant %s\n\ncanonical:\n%q", got, want, canonical)
	}
}

func TestSignatureVector_QueryAndBody(t *testing.T) {
	query := url.Values{"scope": {"fsw"}, "since": {"a b"}}
	canonical := signatureCanonical("POST", "/api/pat/v1/pods/web-1/restart", query,
		[]byte(`{"force":true}`), 1788000000, "0123456789abcdef0123456789abcdef")

	const want = "P_JpjmCtmcQ2_04vWEldQoqU9uIZ_Z4U75LT4jbyi5A"

	if got := signVector(vectorPAT, canonical); got != want {
		t.Errorf("signature drift.\n got %s\nwant %s\n\ncanonical:\n%q", got, want, canonical)
	}
}

// A space in a query value is where Ruby and Go part company: one library
// escapes it `+`, the other `%20`. Pinned on its own so a failure says which
// rule broke rather than just "the vector changed".
func TestCanonicalQuery_EscapesSpaceAsPercent20(t *testing.T) {
	got := canonicalQuery(url.Values{"since": {"a b"}})

	if want := "since=a%20b"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Sorted by the encoded pair, so repeated keys are ordered too — url.Values
// sorts keys but leaves repeated values in insertion order.
func TestCanonicalQuery_SortsRepeatedValues(t *testing.T) {
	got := canonicalQuery(url.Values{"pod": {"web-2", "web-1"}, "a": {"1"}})

	if want := "a=1&pod=web-1&pod=web-2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNonceCache_RefusesAReplay(t *testing.T) {
	cache := newNonceCache()
	now := time.Now()

	if !cache.Use("abc", now) {
		t.Fatal("first use was refused")
	}

	if cache.Use("abc", now) {
		t.Error("a replayed nonce was accepted")
	}
}

// Anything older than the window cannot be replayed anyway, so the cache must
// not hold it — otherwise a busy controller grows a map forever.
func TestNonceCache_ForgetsPastTheWindow(t *testing.T) {
	cache := newNonceCache()
	start := time.Now()

	cache.Use("abc", start)

	if !cache.Use("abc", start.Add(sigSkew+time.Minute)) {
		t.Error("a nonce older than the window was still held")
	}
}

func TestParseSignatureHeader(t *testing.T) {
	cases := []struct {
		name   string
		header string
		ok     bool
	}{
		{"complete", "Voodu id=abc123, ts=1788000000, nonce=dead, sig=beef", true},
		{"bearer", "Bearer pat_something", false},
		{"empty", "", false},
		{"missing sig", "Voodu id=abc123, ts=1788000000, nonce=dead", false},
		{"missing id", "Voodu ts=1788000000, nonce=dead, sig=beef", false},
		{"unparseable ts", "Voodu id=abc123, ts=later, nonce=dead, sig=beef", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := parseSignatureHeader(c.header); ok != c.ok {
				t.Errorf("parsed=%v, want %v", ok, c.ok)
			}
		})
	}
}
