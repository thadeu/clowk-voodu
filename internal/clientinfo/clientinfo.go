// Package clientinfo resolves who is running the CLI, for the activity trail.
//
// WHY IT RUNS ON THE CLIENT AND NOWHERE ELSE. A remote `vd apply` does not
// execute on your laptop — the client reads the manifests, streams them over
// SSH, and the binary ON THE BOX makes the controller call. So a lookup done
// server-side returns the SERVER's public address, and the trail would say the
// action came from the machine it was performed on. That is worse than an
// empty column: it looks like an answer.
//
// The same is true of the file names. The client rewrites `-f infra.hcl` into
// `-f -` and pipes the parsed manifests, so the remote never learns what the
// file was called. Both facts exist only here, which is why both ride the same
// channel (the SSH env in internal/remote).
//
// EVERYTHING HERE IS BEST-EFFORT. A lookup that fails, times out, or returns
// nonsense yields a zero Info and the apply proceeds untouched. Recording who
// did something must never be the reason the something did not happen.
package clientinfo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Endpoint is the public-address lookup. ipinfo.io answers without a token for
// this volume and returns the address plus the coarse attribution below.
const Endpoint = "https://ipinfo.io/json"

// endpoint is the address actually used — a variable so tests can point it at
// a local server. Never reassigned outside a test.
var endpoint = Endpoint

// Timeout bounds the lookup. Short on purpose: this sits in front of an apply,
// and an operator on a bad connection must not wait on an audit nicety. Past
// this the apply goes out with no client info at all.
const Timeout = 1500 * time.Millisecond

// CacheTTL is how long a resolved address is reused.
//
// 15 minutes, so a session of a dozen applies costs ONE request rather than a
// dozen — the address does not change between them, and a self-hosted tool
// calling an outside service on every command is exactly what the operator
// chose self-hosting to avoid.
const CacheTTL = 15 * time.Minute

// EnvKey carries an encoded Info to the remote `vd` over the SSH env.
const EnvKey = "VOODU_CLIENT_INFO"

// Header carries it from that remote `vd` to the controller.
const Header = "X-Voodu-Client-Info"

// Info is the subset of the lookup that reaches the trail.
//
// DELIBERATELY NOT the whole response. ipinfo also returns `loc` (latitude and
// longitude), `postal` and `hostname`; none of that is kept. This lands in a
// 30-day file that is served over the PAT plane and mirrored into a WebUI
// database — city-level attribution answers "who ran this", and coordinates
// answer a question nobody asked about an operator's whereabouts.
type Info struct {
	IP      string `json:"ip,omitempty"`
	City    string `json:"city,omitempty"`
	Region  string `json:"region,omitempty"`
	Country string `json:"country,omitempty"`
	Org     string `json:"org,omitempty"`
}

func (i Info) Empty() bool { return i.IP == "" }

// Encode renders Info for transport. Base64 of compact JSON, because the value
// travels as a shell word in the forwarded command and as an HTTP header, and
// the org string carries spaces and commas ("AS28573 Claro S.A.").
func (i Info) Encode() string {
	if i.Empty() {
		return ""
	}

	b, err := json.Marshal(i)
	if err != nil {
		return ""
	}

	return base64.RawURLEncoding.EncodeToString(b)
}

// Decode is Encode's inverse. Anything malformed yields a zero Info rather than
// an error: this value crosses two hops and a bad one must not fail a request.
func Decode(raw string) Info {
	if raw == "" {
		return Info{}
	}

	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Info{}
	}

	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return Info{}
	}

	return info
}

// Lookup returns the client's public address, from cache when it is fresh.
//
// Never returns an error. Every failure path — no cache dir, no network, a
// slow endpoint, a changed response shape — ends at a zero Info.
func Lookup(ctx context.Context) Info {
	if cached, ok := readCache(); ok {
		return cached
	}

	info := fetch(ctx)
	if info.Empty() {
		return Info{}
	}

	writeCache(info)

	return info
}

type cacheFile struct {
	FetchedAt time.Time `json:"fetched_at"`
	Info      Info      `json:"info"`
}

// cachePath is under the user's cache directory, not the voodu root: this is a
// per-person fact about the machine running the CLI, and it does not belong in
// a project directory that gets committed or synced.
func cachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}

	return filepath.Join(dir, "voodu", "client_info.json")
}

func readCache() (Info, bool) {
	path := cachePath()
	if path == "" {
		return Info{}, false
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Info{}, false
	}

	var c cacheFile
	if err := json.Unmarshal(raw, &c); err != nil {
		return Info{}, false
	}

	if time.Since(c.FetchedAt) > CacheTTL || c.Info.Empty() {
		return Info{}, false
	}

	return c.Info, true
}

func writeCache(info Info) {
	path := cachePath()
	if path == "" {
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}

	raw, err := json.Marshal(cacheFile{FetchedAt: time.Now(), Info: info})
	if err != nil {
		return
	}

	// 0600: it names the operator's city and network. Written for this user,
	// readable by this user.
	_ = os.WriteFile(path, raw, 0o600)
}

func fetch(ctx context.Context) Info {
	if ctx == nil {
		ctx = context.Background()
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Info{}
	}

	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Info{}
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Info{}
	}

	var info Info

	// The response carries more fields than Info declares; the decoder drops
	// them, which is the filtering this package promises rather than a
	// convenience — nothing undeclared can leak into the trail by accident.
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return Info{}
	}

	return info
}
