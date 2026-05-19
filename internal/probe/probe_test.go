// Tests for the probe primitives — Spec.Validate, execute(), and
// the default HTTP / TCP executors. Runner-level tests live in
// runner_test.go.
//
// Every probe is exercised via injected fakes (HTTPGetter, TCPDialer,
// ExecRunner) so the suite stays hermetic — no real network calls.

package probe

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeHTTP captures the URL it was called with and returns canned
// (status, err) pairs. The recorder lets us assert "the probe used
// the right URL" without spinning up an http server in every test.
type fakeHTTP struct {
	gotURL     string
	gotHeaders map[string]string
	status     int
	err        error
}

func (f *fakeHTTP) Get(ctx context.Context, url string, headers map[string]string) (int, error) {
	f.gotURL = url
	f.gotHeaders = headers

	return f.status, f.err
}

type fakeTCP struct {
	gotAddr string
	err     error
}

func (f *fakeTCP) Dial(ctx context.Context, addr string) error {
	f.gotAddr = addr
	return f.err
}

type fakeExec struct {
	gotCmd []string
	code   int
	stderr string
	err    error
}

func (f *fakeExec) Exec(ctx context.Context, container string, cmd []string) (int, string, error) {
	f.gotCmd = cmd
	return f.code, f.stderr, f.err
}

// TestValidate covers the rule "exactly one action selector". Every
// failure mode produces a distinct error so the manifest parser can
// surface it to the operator verbatim.
func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		spec    Spec
		wantErr string
	}{
		{
			name:    "no action set",
			spec:    Spec{},
			wantErr: "exactly one of",
		},
		{
			name: "two actions set",
			spec: Spec{Action: Action{
				HTTPGet:   &HTTPGetAction{Path: "/x", Port: 80},
				TCPSocket: &TCPSocketAction{Port: 80},
			}},
			wantErr: "exactly one of",
		},
		{
			name: "http_get without path",
			spec: Spec{Action: Action{
				HTTPGet: &HTTPGetAction{Port: 80},
			}},
			wantErr: "http_get.path is required",
		},
		{
			name: "http_get path without leading slash",
			spec: Spec{Action: Action{
				HTTPGet: &HTTPGetAction{Path: "healthz", Port: 80},
			}},
			wantErr: "must start with '/'",
		},
		{
			name: "http_get without port",
			spec: Spec{Action: Action{
				HTTPGet: &HTTPGetAction{Path: "/healthz"},
			}},
			wantErr: "http_get.port is required",
		},
		{
			name: "tcp_socket without port",
			spec: Spec{Action: Action{
				TCPSocket: &TCPSocketAction{},
			}},
			wantErr: "tcp_socket.port is required",
		},
		{
			name: "exec without command",
			spec: Spec{Action: Action{
				Exec: &ExecAction{},
			}},
			wantErr: "exec.command must be non-empty",
		},
		{
			name: "valid http_get",
			spec: Spec{Action: Action{
				HTTPGet: &HTTPGetAction{Path: "/healthz", Port: 8080},
			}},
		},
		{
			name: "valid tcp_socket",
			spec: Spec{Action: Action{
				TCPSocket: &TCPSocketAction{Port: 5432},
			}},
		},
		{
			name: "valid exec",
			spec: Spec{Action: Action{
				Exec: &ExecAction{Command: []string{"pg_isready"}},
			}},
		},
		{
			name: "negative initial_delay",
			spec: Spec{
				Action:       Action{HTTPGet: &HTTPGetAction{Path: "/x", Port: 80}},
				InitialDelay: -1 * time.Second,
			},
			wantErr: "initial_delay must be >= 0",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate()

			if c.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}

			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestExecute_HTTPGet pins the 2xx/3xx success vs 4xx/5xx failure
// rule. The probe URL is composed from action + host + port.
func TestExecute_HTTPGet(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    error
		wantOK bool
	}{
		{"200 → ok", 200, nil, true},
		{"201 → ok", 201, nil, true},
		{"301 → ok (3xx counts)", 301, nil, true},
		{"399 → ok (edge)", 399, nil, true},
		{"400 → fail", 400, nil, false},
		{"404 → fail", 404, nil, false},
		{"500 → fail", 500, nil, false},
		{"network error → fail", 0, errors.New("connection refused"), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &fakeHTTP{status: c.status, err: c.err}
			spec := Spec{
				Action:  Action{HTTPGet: &HTTPGetAction{Path: "/healthz", Port: 8080}},
				Timeout: time.Second,
			}

			r := execute(context.Background(), spec, "container", "127.0.0.1",
				executors{http: h})

			if r.OK != c.wantOK {
				t.Errorf("OK=%v want %v (reason=%q)", r.OK, c.wantOK, r.Reason)
			}

			if h.gotURL != "http://127.0.0.1:8080/healthz" {
				t.Errorf("URL: got %q, want http://127.0.0.1:8080/healthz", h.gotURL)
			}

			if !c.wantOK && r.Reason == "" {
				t.Error("failure must carry a reason")
			}
		})
	}
}

// TestExecute_HTTPGet_Scheme covers HTTPS scheme + custom headers
// — both are operator-supplied and need to ride through to the
// underlying HTTP call.
func TestExecute_HTTPGet_SchemeAndHeaders(t *testing.T) {
	h := &fakeHTTP{status: 200}
	spec := Spec{
		Action: Action{HTTPGet: &HTTPGetAction{
			Path:        "/secure",
			Port:        443,
			Scheme:      "https",
			HTTPHeaders: map[string]string{"X-Probe": "voodu", "Authorization": "Bearer xxx"},
		}},
		Timeout: time.Second,
	}

	_ = execute(context.Background(), spec, "container", "10.0.0.1",
		executors{http: h})

	if h.gotURL != "https://10.0.0.1:443/secure" {
		t.Errorf("URL: got %q", h.gotURL)
	}

	if h.gotHeaders["X-Probe"] != "voodu" {
		t.Errorf("X-Probe header missing: %+v", h.gotHeaders)
	}

	if h.gotHeaders["Authorization"] != "Bearer xxx" {
		t.Errorf("Authorization header missing: %+v", h.gotHeaders)
	}
}

// TestExecute_TCPSocket covers the connect-success / connect-fail
// dichotomy. Same dialer fake pattern.
func TestExecute_TCPSocket(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		wantOK bool
	}{
		{"connect ok", nil, true},
		{"connection refused", errors.New("refused"), false},
		{"timeout", context.DeadlineExceeded, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &fakeTCP{err: c.err}
			spec := Spec{
				Action:  Action{TCPSocket: &TCPSocketAction{Port: 5432}},
				Timeout: time.Second,
			}

			r := execute(context.Background(), spec, "container", "172.17.0.5",
				executors{tcp: d})

			if r.OK != c.wantOK {
				t.Errorf("OK=%v want %v", r.OK, c.wantOK)
			}

			if d.gotAddr != "172.17.0.5:5432" {
				t.Errorf("addr: %q", d.gotAddr)
			}
		})
	}
}

// TestExecute_Exec covers the exit-0-equals-success rule + stderr
// surfaced in reasons.
func TestExecute_Exec(t *testing.T) {
	t.Run("exit 0 → ok", func(t *testing.T) {
		e := &fakeExec{code: 0}
		spec := Spec{
			Action:  Action{Exec: &ExecAction{Command: []string{"pg_isready"}}},
			Timeout: time.Second,
		}

		r := execute(context.Background(), spec, "voodu-pg", "", executors{exec: e})
		if !r.OK {
			t.Errorf("expected ok, got %q", r.Reason)
		}
	})

	t.Run("exit non-zero → fail with reason", func(t *testing.T) {
		e := &fakeExec{code: 2, stderr: "no response from server"}
		spec := Spec{
			Action:  Action{Exec: &ExecAction{Command: []string{"pg_isready"}}},
			Timeout: time.Second,
		}

		r := execute(context.Background(), spec, "voodu-pg", "", executors{exec: e})
		if r.OK {
			t.Errorf("expected failure")
		}

		if !strings.Contains(r.Reason, "exit 2") || !strings.Contains(r.Reason, "no response") {
			t.Errorf("reason should include exit code + stderr: %q", r.Reason)
		}
	})

	t.Run("runner error → fail", func(t *testing.T) {
		e := &fakeExec{err: errors.New("container not running")}
		spec := Spec{
			Action:  Action{Exec: &ExecAction{Command: []string{"x"}}},
			Timeout: time.Second,
		}

		r := execute(context.Background(), spec, "voodu-pg", "", executors{exec: e})
		if r.OK {
			t.Error("expected failure")
		}
	})
}

// TestExecute_DefaultsApplied — when a Spec is constructed with zero
// values for timeout/period/thresholds, execute() should still work
// with the package defaults. Pin via applyDefaults explicitly.
func TestSpec_ApplyDefaults(t *testing.T) {
	s := Spec{Action: Action{HTTPGet: &HTTPGetAction{Path: "/", Port: 80}}}
	s.applyDefaults()

	if s.Period != 10*time.Second {
		t.Errorf("period: %v", s.Period)
	}

	if s.Timeout != time.Second {
		t.Errorf("timeout: %v", s.Timeout)
	}

	if s.FailureThreshold != 3 {
		t.Errorf("failure_threshold: %d", s.FailureThreshold)
	}

	if s.SuccessThreshold != 1 {
		t.Errorf("success_threshold: %d", s.SuccessThreshold)
	}
}

// TestDefaultHTTPGetter_RealRoundTrip — wires the production
// defaultHTTPGetter against an httptest server. Lightweight integration
// test that guarantees the net/http path doesn't drift.
func TestDefaultHTTPGetter_RealRoundTrip(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Probe") != "voodu" {
			w.WriteHeader(400)
			return
		}

		w.WriteHeader(200)
	}))

	defer ts.Close()

	g := defaultHTTPGetter{}

	code, err := g.Get(context.Background(), ts.URL+"/health",
		map[string]string{"X-Probe": "voodu"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if code != 200 {
		t.Errorf("status: %d", code)
	}
}

// TestDefaultTCPDialer_RealRoundTrip — wires the production dialer
// against a real listener. Pins that the connect-on-listening path
// works AND that connect-on-unlistening fails fast.
func TestDefaultTCPDialer_RealRoundTrip(t *testing.T) {
	// First half: connect to a listener that's up.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	d := defaultTCPDialer{}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := d.Dial(ctx, l.Addr().String()); err != nil {
		t.Errorf("dial to listening: %v", err)
	}

	// Second half: connect to a closed listener.
	addr := l.Addr().String()
	l.Close()

	// Brief sleep so the kernel reclaims the port to "ECONNREFUSED"
	// rather than "still in TIME_WAIT" depending on OS quirks.
	time.Sleep(50 * time.Millisecond)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()

	if err := d.Dial(ctx2, addr); err == nil {
		t.Error("expected dial to closed port to fail")
	}
}
