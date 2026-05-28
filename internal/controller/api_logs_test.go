package controller

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

	// Mirror docker's --timestamps shape: prefix each non-empty line
	// with an RFC3339Nano timestamp + single space, matching what
	// `container.LogsOptions{Timestamps: true}` produces. Real docker
	// uses the line's emit-time from the container's log driver; the
	// fake substitutes a synthetic timestamp because the test only
	// cares about the wire shape, not the value.
	if opts.Timestamps {
		body = prefixTimestamps(body)
	}

	return io.NopCloser(strings.NewReader(body)), nil
}

// prefixTimestamps emits each non-empty input line with an RFC3339Nano
// timestamp + space prefix. Mirrors docker's `--timestamps` output so
// the fake LogStreamer can stand in for the SDK in tests.
func prefixTimestamps(body string) string {
	if body == "" {
		return ""
	}

	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	now := time.Date(2026, 5, 28, 12, 34, 56, 789000000, time.UTC)

	var out strings.Builder

	for scanner.Scan() {
		ts := now.Format(time.RFC3339Nano)
		out.WriteString(ts)
		out.WriteByte(' ')
		out.WriteString(scanner.Text())
		out.WriteByte('\n')
	}

	return out.String()
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

// TestPodLogs_TimestampsOptIn pins the new `?timestamps=true` contract
// on the single-pod handler: the docker SDK's `--timestamps` prefix
// surfaces verbatim in the response body, and the LogStreamer sees
// Timestamps: true threaded through LogsOptions. The off-host poller
// (voodu-webui Go binary) relies on this to pin its watermark to
// docker's clock.
func TestPodLogs_TimestampsOptIn(t *testing.T) {
	api, _, _, logs := newLogsAPI(t)

	logs.logs["test-web.aaaa"] = "hello\nworld\n"

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/test-web.aaaa/logs?timestamps=true")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if len(logs.calls) != 1 {
		t.Fatalf("expected 1 Logs call, got %d", len(logs.calls))
	}

	if !logs.calls[0].Opts.Timestamps {
		t.Errorf("Timestamps not propagated to LogStreamer.Opts")
	}

	// Each non-empty line should start with a parseable RFC3339Nano
	// timestamp + space + body.
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), body)
	}

	for i, want := range []string{"hello", "world"} {
		assertTimestampedLine(t, lines[i], want)
	}
}

// TestPodLogs_TimestampsDefaultOff pins backward compat: without the
// `?timestamps=true` param the legacy clean-body shape stays unchanged
// and the LogStreamer sees Timestamps: false. `vd logs` and the
// existing Rails Job depend on this.
func TestPodLogs_TimestampsDefaultOff(t *testing.T) {
	api, _, _, logs := newLogsAPI(t)

	logs.logs["test-web.aaaa"] = "hello\nworld\n"

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/test-web.aaaa/logs")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if len(logs.calls) != 1 {
		t.Fatalf("expected 1 Logs call, got %d", len(logs.calls))
	}

	if logs.calls[0].Opts.Timestamps {
		t.Errorf("Timestamps should default to false")
	}

	if string(body) != "hello\nworld\n" {
		t.Errorf("body should be clean (no timestamp prefix), got %q", body)
	}
}

// TestLogsMulti_TimestampsOptIn covers the same opt-in on the multi-pod
// handler: every line under the multiplexed `[pod-name] ` prefix should
// then carry the docker RFC3339Nano timestamp between the prefix and
// the body.
func TestLogsMulti_TimestampsOptIn(t *testing.T) {
	api, _, pods, logs := newLogsAPI(t)

	pods.pods = []Pod{
		{Name: "x-web.a", Kind: "deployment", Scope: "x", ResourceName: "web"},
	}
	logs.logs["x-web.a"] = "alpha-1\nalpha-2\n"

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?scope=x&timestamps=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if len(logs.calls) != 1 {
		t.Fatalf("expected 1 Logs call, got %d", len(logs.calls))
	}

	if !logs.calls[0].Opts.Timestamps {
		t.Errorf("Timestamps not propagated through streamMultiplexedLogs")
	}

	// Pull every non-empty (non-heartbeat) line and assert shape:
	// `[x-web.a] <RFC3339Nano> <body>`.
	var seen []string

	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if line == "" {
			continue
		}

		seen = append(seen, line)
	}

	if len(seen) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(seen), body)
	}

	for i, want := range []string{"alpha-1", "alpha-2"} {
		assertMultiplexedTimestampedLine(t, seen[i], "x-web.a", want)
	}
}

// TestLogsMulti_TimestampsDefaultOff pins legacy CLI compat on the
// multi-pod handler: without the param the response is byte-equal to
// the pre-timestamps shape (`[pod-name] <body>`).
func TestLogsMulti_TimestampsDefaultOff(t *testing.T) {
	api, _, pods, logs := newLogsAPI(t)

	pods.pods = []Pod{
		{Name: "x-web.a", Kind: "deployment", Scope: "x", ResourceName: "web"},
	}
	logs.logs["x-web.a"] = "alpha-1\n"

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/logs?scope=x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if len(logs.calls) != 1 {
		t.Fatalf("expected 1 Logs call, got %d", len(logs.calls))
	}

	if logs.calls[0].Opts.Timestamps {
		t.Errorf("Timestamps should default to false")
	}

	if !strings.Contains(string(body), "[x-web.a] alpha-1") {
		t.Errorf("legacy multiplexed shape broken, got %q", body)
	}

	if strings.Contains(string(body), "2026-") {
		t.Errorf("body unexpectedly carries a timestamp prefix: %q", body)
	}
}

// assertTimestampedLine checks `<RFC3339Nano> <body>` for the single
// pod handler shape.
func assertTimestampedLine(t *testing.T, line, wantBody string) {
	t.Helper()

	sp := strings.IndexByte(line, ' ')
	if sp <= 0 {
		t.Fatalf("line missing timestamp/body separator: %q", line)
	}

	tsStr := line[:sp]
	body := line[sp+1:]

	if _, err := time.Parse(time.RFC3339Nano, tsStr); err != nil {
		t.Errorf("timestamp not RFC3339Nano: %q (%v)", tsStr, err)
	}

	if body != wantBody {
		t.Errorf("body: got %q, want %q", body, wantBody)
	}
}

// assertMultiplexedTimestampedLine checks
// `[<pod>] <RFC3339Nano> <body>` for the multi-pod handler shape.
func assertMultiplexedTimestampedLine(t *testing.T, line, wantPod, wantBody string) {
	t.Helper()

	prefix := "[" + wantPod + "] "
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("line missing pod prefix %q: %q", prefix, line)
	}

	assertTimestampedLine(t, strings.TrimPrefix(line, prefix), wantBody)
}
