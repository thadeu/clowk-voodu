package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeLogStreamer answers the LogStreamer interface from a per-name
// canned-text map. Records every call so tests can assert which
// container the API resolved to. Concurrent-safe so we don't tickle
// the race detector when /logs writes the body on the same goroutine
// the test is reading from.
type fakeLogStreamer struct {
	mu     sync.Mutex
	logs   map[string]string
	calls  []fakeLogCall
	openOK bool
	openEr error
}

type fakeLogCall struct {
	Name string
	Opts LogsOptions
}

func (f *fakeLogStreamer) Logs(name string, opts LogsOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, fakeLogCall{Name: name, Opts: opts})

	if f.openEr != nil {
		return nil, f.openEr
	}

	body := f.logs[name]

	return io.NopCloser(strings.NewReader(body)), nil
}

func newLogsAPI(t *testing.T) (*API, *memStore, *fakePodsLister, *fakeLogStreamer) {
	t.Helper()

	store := newMemStore()
	pods := &fakePodsLister{}
	logs := &fakeLogStreamer{logs: map[string]string{}}

	return &API{Store: store, Version: "test", Pods: pods, Logs: logs}, store, pods, logs
}

// TestLogs_PicksLatestRunningWhenRunOmitted locks the default-target
// rule: with multiple replicas of the same identity, /logs streams the
// running one, and it picks the most recent CreatedAt as the
// tiebreaker. The streamed body must arrive verbatim — no JSON
// envelope, no decoration.
func TestLogs_PicksLatestRunningWhenRunOmitted(t *testing.T) {
	api, _, pods, logs := newLogsAPI(t)

	pods.pods = []Pod{
		{
			Name: "test-web.older", Kind: "deployment", Scope: "test",
			ResourceName: "web", ReplicaID: "older", CreatedAt: "2026-04-25T00:00:00Z",
			Running: true,
		},
		{
			Name: "test-web.newer", Kind: "deployment", Scope: "test",
			ResourceName: "web", ReplicaID: "newer", CreatedAt: "2026-04-25T00:01:00Z",
			Running: true,
		},
		{
			Name: "test-web.stopped", Kind: "deployment", Scope: "test",
			ResourceName: "web", ReplicaID: "stopped", CreatedAt: "2026-04-25T00:02:00Z",
			Running: false,
		},
	}

	logs.logs["test-web.newer"] = "hello from newer\n"

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?kind=deployment&scope=test&name=web")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from newer\n" {
		t.Errorf("body: got %q, want hello from newer", string(body))
	}

	if got := resp.Header.Get("X-Voodu-Container"); got != "test-web.newer" {
		t.Errorf("X-Voodu-Container header: got %q, want test-web.newer", got)
	}

	if got := resp.Header.Get("X-Voodu-Run"); got != "newer" {
		t.Errorf("X-Voodu-Run header: got %q, want newer", got)
	}

	if len(logs.calls) != 1 || logs.calls[0].Name != "test-web.newer" {
		t.Errorf("expected one Logs call for test-web.newer, got %+v", logs.calls)
	}
}

// TestLogs_PrefersRunningOverNewerStopped guards against an off-by-one
// in the tiebreaker: a stopped container with a fresher CreatedAt
// should NOT outrank a running one. Operators tailing `voodu logs -f`
// expect the live process, not the most recent corpse.
func TestLogs_PrefersRunningOverNewerStopped(t *testing.T) {
	api, _, pods, logs := newLogsAPI(t)

	pods.pods = []Pod{
		{
			Name: "test-web.stopped", Kind: "deployment", Scope: "test",
			ResourceName: "web", ReplicaID: "stopped", CreatedAt: "2026-04-25T00:02:00Z",
			Running: false,
		},
		{
			Name: "test-web.live", Kind: "deployment", Scope: "test",
			ResourceName: "web", ReplicaID: "live", CreatedAt: "2026-04-25T00:00:00Z",
			Running: true,
		},
	}

	logs.logs["test-web.live"] = "alive\n"

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?kind=deployment&scope=test&name=web")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if got := resp.Header.Get("X-Voodu-Container"); got != "test-web.live" {
		t.Errorf("running container should win even with older CreatedAt; got %q", got)
	}
}

// TestLogs_RunQueryPinsExactReplica asserts that --run pins the exact
// replica id, even when a newer / running candidate exists.
func TestLogs_RunQueryPinsExactReplica(t *testing.T) {
	api, _, pods, logs := newLogsAPI(t)

	pods.pods = []Pod{
		{
			Name: "test-job.abcd", Kind: "job", Scope: "test",
			ResourceName: "migrate", ReplicaID: "abcd", CreatedAt: "2026-04-24T10:00:00Z",
			Running: false,
		},
		{
			Name: "test-job.beef", Kind: "job", Scope: "test",
			ResourceName: "migrate", ReplicaID: "beef", CreatedAt: "2026-04-25T10:00:00Z",
			Running: false,
		},
	}

	logs.logs["test-job.abcd"] = "old run output\n"

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?kind=job&scope=test&name=migrate&run=abcd")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "old run output\n" {
		t.Errorf("body: got %q", string(body))
	}

	if got := resp.Header.Get("X-Voodu-Run"); got != "abcd" {
		t.Errorf("X-Voodu-Run: got %q, want abcd", got)
	}
}

// TestLogs_RunNotFoundReturns404 makes sure an operator who typos the
// run id sees a clean 404 instead of a stream of someone else's logs.
func TestLogs_RunNotFoundReturns404(t *testing.T) {
	api, _, pods, _ := newLogsAPI(t)

	pods.pods = []Pod{
		{
			Name: "test-job.beef", Kind: "job", Scope: "test",
			ResourceName: "migrate", ReplicaID: "beef", Running: false,
		},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?kind=job&scope=test&name=migrate&run=ffff")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestLogs_PassesTailAndFollowOptions confirms the query-string knobs
// reach the LogStreamer untouched. The CLI relies on this contract to
// implement -f and --tail.
func TestLogs_PassesTailAndFollowOptions(t *testing.T) {
	api, _, pods, logs := newLogsAPI(t)

	pods.pods = []Pod{
		{
			Name: "test-job.x", Kind: "job", Scope: "test",
			ResourceName: "migrate", ReplicaID: "x", Running: true,
		},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?kind=job&scope=test&name=migrate&follow=true&tail=42")
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()

	if len(logs.calls) != 1 {
		t.Fatalf("expected 1 Logs call, got %d", len(logs.calls))
	}

	got := logs.calls[0].Opts
	if !got.Follow {
		t.Errorf("Follow not propagated")
	}

	if got.Tail != 42 {
		t.Errorf("Tail: got %d, want 42", got.Tail)
	}
}

// TestLogs_NoMatchingPodsReturns404 asserts the resolver fails cleanly
// when the kind/scope/name doesn't match any voodu container — usually
// "you applied the manifest but no run / replica has spawned yet".
func TestLogs_NoMatchingPodsReturns404(t *testing.T) {
	api, _, pods, _ := newLogsAPI(t)

	pods.pods = []Pod{
		{Name: "other.x", Kind: "job", Scope: "other", ResourceName: "thing"},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?kind=job&scope=test&name=missing")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestLogs_RejectsBadKind covers the kind validator at the surface so
// the controller doesn't waste a docker round-trip on garbage input.
func TestLogs_RejectsBadKind(t *testing.T) {
	api, _, _, _ := newLogsAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?kind=potato&name=x")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestLogs_RejectsNegativeTail covers the basic input validation —
// negative tails are nonsensical and would surprise the operator
// further down the chain.
func TestLogs_RejectsNegativeTail(t *testing.T) {
	api, _, _, _ := newLogsAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?kind=job&scope=test&name=migrate&tail=-1")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestLogs_StreamerNotConfiguredReturns503 catches the misconfiguration
// where /logs is hit on an API that wasn't given a LogStreamer (eg a
// reduced-functionality test server). 503 is the right shape — it
// signals "this controller can do logs in principle, just not now".
func TestLogs_StreamerNotConfiguredReturns503(t *testing.T) {
	store := newMemStore()
	api := &API{Store: store, Version: "test", Pods: &fakePodsLister{}}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?kind=job&scope=test&name=migrate")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", resp.StatusCode)
	}
}
