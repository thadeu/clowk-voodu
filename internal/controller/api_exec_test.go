package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeExecer captures the (name, command, opts) the API hands it
// and writes a canned reply to the supplied stdout, then returns a
// fixed exit code. Concurrent-safe so the hijacked conn can be read
// from the test goroutine while Exec is mid-flight.
type fakeExecer struct {
	mu sync.Mutex

	gotName    string
	gotCommand []string
	gotOpts    ExecOptions

	// reply is written to opts.Stdout once Exec is invoked. Lets
	// tests assert the bridge actually flows server → client.
	reply string

	// exitCode is what Exec returns. Tests for non-zero exit (e.g.
	// "cmd not found") set this to confirm the path doesn't crash.
	exitCode int
}

func (f *fakeExecer) Exec(name string, command []string, opts ExecOptions) (int, error) {
	f.mu.Lock()
	f.gotName = name
	f.gotCommand = command
	f.gotOpts = opts
	f.mu.Unlock()

	if f.reply != "" && opts.Stdout != nil {
		_, _ = io.WriteString(opts.Stdout, f.reply)
	}

	return f.exitCode, nil
}

// TestPodExec_HijacksConnectionAndDispatchesCommand exercises the
// happy path: a POST to /pods/{name}/exec with a JSON body carrying
// the command lands at the fake Execer with the right (name,
// command) tuple and the canned reply round-trips back over the
// hijacked connection.
//
// We dial httptest's server directly (not via http.DefaultClient)
// because http.Client closes the connection on response — we need
// to keep it open to read the post-header stream that the hijack
// writes.
func TestPodExec_HijacksConnectionAndDispatchesCommand(t *testing.T) {
	exec := &fakeExecer{reply: "hello from exec\n"}

	api := &API{Store: newMemStore(), Version: "test", Execer: exec}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	// Strip the http:// prefix so we can dial raw TCP.
	host := strings.TrimPrefix(ts.URL, "http://")

	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Close()

	body := `{"command":["bash","-l"]}`

	req := fmt.Sprintf(
		"POST /pods/test-web.aaaa/exec HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		host, len(body), body,
	)

	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(conn)

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	// Confirm the fake saw the right command and the body streamed
	// through.
	exec.mu.Lock()
	defer exec.mu.Unlock()

	if exec.gotName != "test-web.aaaa" {
		t.Errorf("name=%q want test-web.aaaa", exec.gotName)
	}

	if strings.Join(exec.gotCommand, " ") != "bash -l" {
		t.Errorf("command=%v want [bash -l]", exec.gotCommand)
	}

	// Read the canned reply that fakeExecer wrote into opts.Stdout
	// (which is the hijacked conn). The response body uses close-
	// delimited framing (no Content-Length, no Transfer-Encoding)
	// so we read until EOF; the bufio.Reader behind ReadResponse
	// also holds whatever was buffered after the headers.
	tail, _ := io.ReadAll(io.MultiReader(resp.Body, br))
	if !strings.Contains(string(tail), "hello from exec") {
		t.Errorf("body should contain canned reply, got %q", string(tail))
	}
}

// TestPodExec_HonoursTTYInteractiveAndWinsize locks in the wire
// shape: tty/interactive/cols/rows from the query string land on
// the Execer's opts intact. With the PTY bridge in place, the
// server now respects ?tty=true and forwards window dimensions to
// pty.Setsize so interactive shells (psql, redis-cli, vim, htop)
// start at the operator's terminal size.
func TestPodExec_HonoursTTYInteractiveAndWinsize(t *testing.T) {
	exec := &fakeExecer{}

	api := &API{Store: newMemStore(), Version: "test", Execer: exec}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	host := strings.TrimPrefix(ts.URL, "http://")

	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Close()

	body := `{"command":["bash"]}`

	req := fmt.Sprintf(
		"POST /pods/test.aaaa/exec?tty=true&interactive=true&cols=120&rows=42 HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		host, len(body), body,
	)

	_, _ = conn.Write([]byte(req))

	br := bufio.NewReader(conn)
	_, _ = http.ReadResponse(br, nil)

	exec.mu.Lock()
	defer exec.mu.Unlock()

	if !exec.gotOpts.TTY {
		t.Errorf("TTY should propagate when ?tty=true; got %v", exec.gotOpts.TTY)
	}

	if !exec.gotOpts.Interactive {
		t.Errorf("Interactive should propagate when ?interactive=true")
	}

	if exec.gotOpts.Cols != 120 || exec.gotOpts.Rows != 42 {
		t.Errorf("winsize: got %dx%d, want 120x42", exec.gotOpts.Cols, exec.gotOpts.Rows)
	}
}

// TestPodExec_RejectsHostileWinsize confirms that a malformed cols
// or rows value (negative, non-numeric, overflow) silently degrades
// to zero rather than panicking or leaking the bad input down to
// pty.Setsize. Defense-in-depth: the kernel would clamp anyway, but
// the parser stays predictable.
func TestPodExec_RejectsHostileWinsize(t *testing.T) {
	exec := &fakeExecer{}

	api := &API{Store: newMemStore(), Version: "test", Execer: exec}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	host := strings.TrimPrefix(ts.URL, "http://")

	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Close()

	body := `{"command":["bash"]}`

	req := fmt.Sprintf(
		"POST /pods/test.aaaa/exec?tty=true&cols=-5&rows=999999 HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		host, len(body), body,
	)

	_, _ = conn.Write([]byte(req))

	br := bufio.NewReader(conn)
	_, _ = http.ReadResponse(br, nil)

	exec.mu.Lock()
	defer exec.mu.Unlock()

	if exec.gotOpts.Cols != 0 || exec.gotOpts.Rows != 0 {
		t.Errorf("malformed winsize should clamp to zero, got %dx%d",
			exec.gotOpts.Cols, exec.gotOpts.Rows)
	}
}

// TestPodExec_RejectsEmptyCommand guards the CLI contract: the
// command list is what makes the call meaningful. An empty body
// must error fast rather than spawning bash by accident.
func TestPodExec_RejectsEmptyCommand(t *testing.T) {
	api := &API{Store: newMemStore(), Version: "test", Execer: &fakeExecer{}}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/pods/test.aaaa/exec", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}

	var env struct {
		Error string
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if !strings.Contains(env.Error, "command") {
		t.Errorf("error should mention command, got %q", env.Error)
	}
}

// TestPodExec_503WhenExecerNotConfigured locks in the
// reduced-capability behavior: an API wired without Execer returns
// 503 instead of nil-panicking.
func TestPodExec_503WhenExecerNotConfigured(t *testing.T) {
	api := &API{Store: newMemStore(), Version: "test"}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/pods/test.aaaa/exec", "application/json",
		strings.NewReader(`{"command":["bash"]}`))
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", resp.StatusCode)
	}
}

// silence "imported but unused" complaints for context — the
// fakeExecer's signature uses ExecOptions which doesn't include
// context, but importing keeps the file consistent with
// neighboring tests.
var _ = context.Canceled
