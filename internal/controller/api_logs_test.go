package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeLogStreamer answers the LogStreamer interface from a per-name
// canned-text map. Records every call so tests can assert which
// container the API was asked to stream. Concurrent-safe so we don't
// tickle the race detector when /pods/{name}/logs writes the body on
// the same goroutine the test is reading from.
type fakeLogStreamer struct {
	mu     sync.Mutex
	logs   map[string]string
	calls  []fakeLogCall
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

// TestPodLogs_StreamsBodyVerbatim covers the core happy path:
// GET /pods/{name}/logs streams the LogStreamer body byte-for-byte
// with no envelope wrapping, and tags the response with X-Voodu-Container.
func TestPodLogs_StreamsBodyVerbatim(t *testing.T) {
	api, _, _, logs := newLogsAPI(t)

	logs.logs["test-web.aaaa"] = "hello\nworld\n"

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/test-web.aaaa/logs")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello\nworld\n" {
		t.Errorf("body: got %q", string(body))
	}

	if got := resp.Header.Get("X-Voodu-Container"); got != "test-web.aaaa" {
		t.Errorf("X-Voodu-Container: got %q, want test-web.aaaa", got)
	}

	if len(logs.calls) != 1 || logs.calls[0].Name != "test-web.aaaa" {
		t.Errorf("expected single Logs call for test-web.aaaa, got %+v", logs.calls)
	}
}

// TestPodLogs_PassesFollowAndTail confirms the query-string knobs
// reach the LogStreamer untouched. The CLI relies on this contract
// to implement -f and --tail.
func TestPodLogs_PassesFollowAndTail(t *testing.T) {
	api, _, _, logs := newLogsAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/test-web.aaaa/logs?follow=true&tail=42")
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

// TestPodLogs_RejectsNegativeTail covers basic input validation —
// negative tails are nonsensical and would surprise the operator
// further down the chain.
func TestPodLogs_RejectsNegativeTail(t *testing.T) {
	api, _, _, _ := newLogsAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/test-web.aaaa/logs?tail=-1")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestPodLogs_RejectsHostileName guards against names with slashes
// or whitespace from confusing docker / escaping the path.
func TestPodLogs_RejectsHostileName(t *testing.T) {
	api, _, _, _ := newLogsAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/.hidden/logs")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestPodLogs_StreamerNotConfiguredReturns503 catches the
// misconfiguration where /pods/{name}/logs is hit on an API that
// wasn't given a LogStreamer (eg a reduced-functionality test
// server). 503 is the right shape — it signals "this controller
// can do logs in principle, just not now".
func TestPodLogs_StreamerNotConfiguredReturns503(t *testing.T) {
	store := newMemStore()
	api := &API{Store: store, Version: "test", Pods: &fakePodsLister{}}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/test-web.aaaa/logs")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", resp.StatusCode)
	}
}

// TestLogsMulti_FansOutAndPrefixes pins the core contract of GET
// /logs: server-side fan-out across every matching pod, with each
// line prefixed by `[pod-name] ` so the multiplexed stream stays
// attributable. Same semantics the CLI's multi-target render uses
// client-side today, just shifted to the server so the WebUI and any
// other consumer get it for free.
func TestLogsMulti_FansOutAndPrefixes(t *testing.T) {
	api, _, pods, logs := newLogsAPI(t)

	pods.pods = []Pod{
		{Name: "x-web.a", Kind: "deployment", Scope: "x", ResourceName: "web"},
		{Name: "x-web.b", Kind: "deployment", Scope: "x", ResourceName: "web"},
	}
	logs.logs["x-web.a"] = "alpha-1\nalpha-2\n"
	logs.logs["x-web.b"] = "beta-1\n"

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?scope=x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// Order between pods is best-effort (concurrent fan-out) — assert
	// presence rather than strict ordering.
	for _, want := range []string{"[x-web.a] alpha-1", "[x-web.a] alpha-2", "[x-web.b] beta-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q:\n%s", want, got)
		}
	}

	// X-Voodu-Containers exposes which pods were matched so callers
	// can distinguish "no matches" from "transport failed".
	if h := resp.Header.Get("X-Voodu-Containers"); h != "x-web.a,x-web.b" {
		t.Errorf("X-Voodu-Containers: got %q, want x-web.a,x-web.b", h)
	}
}

// TestLogsMulti_FiltersByScopeKindName pins the filter vocabulary
// matching /pods — same query params produce the same matching set.
func TestLogsMulti_FiltersByScopeKindName(t *testing.T) {
	api, _, pods, logs := newLogsAPI(t)

	pods.pods = []Pod{
		{Name: "x-web.a", Kind: "deployment", Scope: "x", ResourceName: "web"},
		{Name: "x-api.b", Kind: "deployment", Scope: "x", ResourceName: "api"},
		{Name: "y-web.c", Kind: "deployment", Scope: "y", ResourceName: "web"},
	}
	logs.logs["x-web.a"] = "Aweb\n"
	logs.logs["x-api.b"] = "Aapi\n"
	logs.logs["y-web.c"] = "Yweb\n"

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	// scope=x → both x-* pods, no y
	resp, _ := http.Get(ts.URL + "/logs?scope=x")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	for _, want := range []string{"[x-web.a] Aweb", "[x-api.b] Aapi"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("scope=x missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "Yweb") {
		t.Errorf("scope=x leaked y-web line:\n%s", body)
	}

	// scope=x&name=web → only x-web
	resp, _ = http.Get(ts.URL + "/logs?scope=x&name=web")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "[x-web.a] Aweb") {
		t.Errorf("scope=x&name=web should include x-web.a:\n%s", body)
	}
	if strings.Contains(string(body), "Aapi") {
		t.Errorf("scope=x&name=web leaked api line:\n%s", body)
	}
}

// TestLogsMulti_NoMatchReturnsEmpty200 pins that a zero-match filter
// returns 200 with an empty body and X-Voodu-Containers empty —
// distinct from "transport failed" (would be 5xx) and "controller not
// wired" (503).
func TestLogsMulti_NoMatchReturnsEmpty200(t *testing.T) {
	api, _, pods, _ := newLogsAPI(t)
	pods.pods = []Pod{}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?scope=does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("body should be empty, got: %q", body)
	}

	if h := resp.Header.Get("X-Voodu-Containers"); h != "" {
		t.Errorf("X-Voodu-Containers: got %q, want empty", h)
	}
}

// TestLogsMulti_PerPodOpenFailureContinues pins the resilient
// behaviour: if ONE pod's stream fails to open (container vanished
// between match and stream), the others still produce output and
// the bad one surfaces inline as `[pod] [stream error] msg`.
func TestLogsMulti_PerPodOpenFailureContinues(t *testing.T) {
	api, _, pods, logs := newLogsAPI(t)

	pods.pods = []Pod{
		{Name: "good.a", Scope: "x", Kind: "deployment", ResourceName: "good"},
		{Name: "bad.b", Scope: "x", Kind: "deployment", ResourceName: "bad"},
	}

	// good has logs; bad isn't registered → fakeLogStreamer returns
	// the empty-string body for it, which is also OK; switch to a
	// custom streamer that errors on "bad.b" specifically.
	logs.logs["good.a"] = "ok-line\n"

	// Replace with a streamer that errors on "bad.b".
	api.Logs = &erroringStreamer{
		good: map[string]string{"good.a": "ok-line\n"},
		fail: map[string]error{"bad.b": fmt.Errorf("container not found")},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?scope=x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, "[good.a] ok-line") {
		t.Errorf("good pod's line missing:\n%s", got)
	}

	if !strings.Contains(got, "[bad.b] [stream error] container not found") {
		t.Errorf("bad pod's error not surfaced inline:\n%s", got)
	}
}

// erroringStreamer is a custom LogStreamer that picks between
// canned bodies and canned errors per pod name. Local to the
// test above so the shared fakeLogStreamer stays simple.
type erroringStreamer struct {
	good map[string]string
	fail map[string]error
}

func (e *erroringStreamer) Logs(name string, _ LogsOptions) (io.ReadCloser, error) {
	if err, ok := e.fail[name]; ok {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(e.good[name])), nil
}
