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
