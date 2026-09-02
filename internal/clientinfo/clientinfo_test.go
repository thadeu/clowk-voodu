package clientinfo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The filtering this package promises is the decoder itself: the lookup
// returns coordinates and a postal code, and neither may reach the trail. This
// is the test that fails if somebody adds a field "while they are in there".
func TestDecodeKeepsOnlyTheDeclaredFields(t *testing.T) {
	body := `{
	  "ip": "189.4.22.10", "city": "Sao Paulo", "region": "Sao Paulo",
	  "country": "BR", "org": "AS28573 Claro S.A.",
	  "loc": "-23.5475,-46.6361", "postal": "01000", "hostname": "b1-4-22-10.example"
	}`

	var info Info
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatal(err)
	}

	round, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}

	for _, forbidden := range []string{"loc", "postal", "hostname", "-23.54", "01000"} {
		if strings.Contains(string(round), forbidden) {
			t.Fatalf("%q survived into the recorded shape: %s", forbidden, round)
		}
	}

	if info.City != "Sao Paulo" || info.Org != "AS28573 Claro S.A." {
		t.Fatalf("useful fields were dropped: %+v", info)
	}
}

// The value crosses a shell word and an HTTP header, and the org string holds
// spaces and a comma.
func TestEncodeRoundTripsThroughAShellSafeString(t *testing.T) {
	in := Info{IP: "189.4.22.10", City: "Sao Paulo", Org: "AS28573 Claro S.A."}

	encoded := in.Encode()

	if strings.ContainsAny(encoded, " ,\"'") {
		t.Fatalf("encoded value is not shell/header safe: %q", encoded)
	}

	if got := Decode(encoded); got != in {
		t.Fatalf("round trip: %+v, want %+v", got, in)
	}
}

// Two hops carry this value. A mangled one must degrade, never explode.
func TestDecodeIsTotal(t *testing.T) {
	for _, raw := range []string{"", "!!!not base64!!!", "bm90IGpzb24"} {
		if got := Decode(raw); !got.Empty() {
			t.Errorf("Decode(%q) = %+v, want empty", raw, got)
		}
	}
}

func TestEmptyInfoEncodesToNothing(t *testing.T) {
	if got := (Info{}).Encode(); got != "" {
		t.Fatalf("empty Info encoded to %q", got)
	}
}

// THE property that makes this safe to put in front of an apply: every failure
// is silent and yields nothing.
func TestLookupSurvivesAnUnreachableEndpoint(t *testing.T) {
	withCacheDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	defer srv.Close()

	withEndpoint(t, srv.URL)

	if got := Lookup(context.Background()); !got.Empty() {
		t.Fatalf("a failing endpoint produced %+v", got)
	}
}

func TestLookupSurvivesAnEndpointThatHangs(t *testing.T) {
	withCacheDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(Timeout * 3)
	}))

	defer srv.Close()

	withEndpoint(t, srv.URL)

	start := time.Now()
	got := Lookup(context.Background())

	if !got.Empty() {
		t.Fatalf("a hanging endpoint produced %+v", got)
	}

	// The apply is waiting behind this. A generous ceiling — the point is that
	// it is bounded at all, not the exact number.
	if elapsed := time.Since(start); elapsed > Timeout*2 {
		t.Fatalf("lookup took %s, past its own timeout", elapsed)
	}
}

// The reason for the cache: a session of a dozen applies must cost one request.
func TestLookupHitsTheEndpointOnceWithinTheTTL(t *testing.T) {
	withCacheDir(t)

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ip":"189.4.22.10","city":"Sao Paulo","country":"BR"}`))
	}))

	defer srv.Close()

	withEndpoint(t, srv.URL)

	first := Lookup(context.Background())
	second := Lookup(context.Background())

	if first.IP != "189.4.22.10" || second != first {
		t.Fatalf("first=%+v second=%+v", first, second)
	}

	if n := calls.Load(); n != 1 {
		t.Fatalf("endpoint called %d times, want 1 — the cache did not hold", n)
	}
}

func TestAStaleCacheIsRefetched(t *testing.T) {
	withCacheDir(t)

	stale := cacheFile{FetchedAt: time.Now().Add(-CacheTTL - time.Minute), Info: Info{IP: "1.1.1.1"}}
	raw, _ := json.Marshal(stale)

	// cachePath(), never a hand-built path: os.UserCacheDir is
	// $HOME/Library/Caches on darwin and $XDG_CACHE_HOME elsewhere, so a
	// hardcoded path writes somewhere the code never reads — and the test
	// passes because no cache was found, which is the opposite of what it
	// claims to prove.
	path := cachePath()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"2.2.2.2"}`))
	}))

	defer srv.Close()

	withEndpoint(t, srv.URL)

	if got := Lookup(context.Background()); got.IP != "2.2.2.2" {
		t.Fatalf("stale cache was served: %+v", got)
	}
}

// The cache names the operator's city and network; it is not world-readable.
func TestTheCacheFileIsPrivateToTheUser(t *testing.T) {
	withCacheDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"189.4.22.10","city":"Sao Paulo"}`))
	}))

	defer srv.Close()

	withEndpoint(t, srv.URL)

	Lookup(context.Background())

	st, err := os.Stat(cachePath())
	if err != nil {
		t.Fatal(err)
	}

	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("cache mode is %o, want 600", perm)
	}
}

// withCacheDir isolates the on-disk cache. Both env vars, because
// os.UserCacheDir reads XDG_CACHE_HOME on unix and $HOME/Library/Caches on
// darwin — setting one leaves the other platform writing into the real cache.
func withCacheDir(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("HOME", dir)
}

// withEndpoint points the lookup at a test server for the duration of one test.
func withEndpoint(t *testing.T, url string) {
	t.Helper()

	original := endpoint
	endpoint = url

	t.Cleanup(func() { endpoint = original })
}
