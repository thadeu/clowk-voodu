// Package probe is voodu's kubelet-style health-check executor. It
// runs HTTP / TCP / exec probes against running containers and emits
// success/failure events to callers.
//
// The package is deliberately controller-free: no docker, no etcd, no
// manifest types. Inputs are plain Go structs; outputs are Go channels
// and callbacks. This is what lets the same code drive liveness today,
// readiness + startup tomorrow, and (eventually) plugin-defined probes
// without re-implementing the threshold-tracking machinery each time.
//
// Concurrency model: one Runner per (container, probe-type) tuple. The
// Runner owns its goroutine, owns its sample window, and decides when
// the configured threshold has been hit. Callers pass in:
//
//   - An Executor (the action that produces a Result), and
//   - An OnTrigger callback (what to do when the failure threshold is
//     reached — for liveness this is `docker restart <container>`).
//
// The probe package itself doesn't know what a container is or what
// "restart" means; it just runs samples and counts.
package probe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Action picks which type of probe to execute. Each Action has exactly
// one populated selector — the manifest layer is responsible for
// rejecting "two selectors set" or "zero selectors set" at parse time.
//
// The struct is deliberately polymorphic-by-presence rather than
// polymorphic-by-interface because the wire JSON shape needs to be
// simple (the manifest persists this verbatim to etcd), and Go's
// json package doesn't carry type tags by default. A flat struct
// with nil-checks is the least-surprising encoding.
type Action struct {
	HTTPGet   *HTTPGetAction
	TCPSocket *TCPSocketAction
	Exec      *ExecAction
}

// HTTPGetAction issues an HTTP GET against the container and treats
// any 2xx/3xx response as success. The Host defaults to the
// container's IP (caller resolves it before calling Run); a custom
// Host or Scheme is rare but supported for parity with k8s.
type HTTPGetAction struct {
	// Path is the request path (must start with "/"). Required.
	Path string

	// Port is the TCP port to hit. Required (the runner doesn't
	// auto-pick — the manifest layer fills in a sensible default
	// from spec.ports if the operator omitted it).
	Port int

	// Scheme defaults to "http" when empty. "https" is supported
	// but skip-verify always (probes are inside the container's
	// network — we trust the bridge, not the cert chain).
	Scheme string

	// HTTPHeaders ride on the request verbatim. Optional. Useful for
	// apps that require an auth header on /healthz, or that
	// fingerprint the User-Agent.
	HTTPHeaders map[string]string
}

// TCPSocketAction is a "can I connect?" probe — useful for non-HTTP
// services (postgres, redis, raw TCP daemons) where opening the port
// is the smallest "alive" signal.
type TCPSocketAction struct {
	// Port is the TCP port to dial. Required.
	Port int
}

// ExecAction runs a command inside the container. The container
// runtime (docker exec) is invoked by the OnExec callback passed
// to the Runner — the probe package itself doesn't shell out.
//
// Exit code 0 → success. Anything else → failure (matches k8s).
type ExecAction struct {
	// Command is the argv to execute. Required, non-empty.
	Command []string
}

// Spec is the operator-facing knobs for one probe (liveness, OR
// readiness, OR startup — pick one when constructing a Runner).
// Names + semantics mirror Kubernetes exactly so anyone moving
// between voodu and k8s sees the same vocabulary.
type Spec struct {
	// Action picks the probe type.
	Action Action

	// InitialDelay is how long to wait before the FIRST probe runs.
	// Use this to give slow boots (Rails, Java, JVM-everything)
	// time to start listening before we count a failure. Default 0.
	InitialDelay time.Duration

	// Period is the interval between probe attempts after the
	// initial delay. Defaults to 10s if zero — same as k8s default.
	Period time.Duration

	// Timeout is the per-probe deadline. HTTP/TCP probes hang up
	// after this; exec probes have their context cancelled.
	// Defaults to 1s if zero.
	Timeout time.Duration

	// FailureThreshold is how many CONSECUTIVE failures count as
	// "down" — the trigger fires when this many in a row fail.
	// Defaults to 3 if zero. Resets to 0 on any success.
	FailureThreshold int

	// SuccessThreshold is how many CONSECUTIVE successes count as
	// "up" — relevant primarily for readiness/startup where we
	// want a stable signal before flipping the pod to ready.
	// Defaults to 1 if zero (k8s default for liveness).
	SuccessThreshold int
}

// applyDefaults fills the zero-value knobs with k8s-default values.
// Caller-set values are preserved; only zeros get touched. We do
// this on Run rather than on Spec construction so callers building
// a Spec programmatically don't have to remember which fields are
// "0 means use default" vs "0 means actual zero".
func (s *Spec) applyDefaults() {
	if s.Period == 0 {
		s.Period = 10 * time.Second
	}

	if s.Timeout == 0 {
		s.Timeout = time.Second
	}

	if s.FailureThreshold == 0 {
		s.FailureThreshold = 3
	}

	if s.SuccessThreshold == 0 {
		s.SuccessThreshold = 1
	}
}

// Result is one probe attempt's outcome.
type Result struct {
	// OK is true when the probe succeeded under its action's rules
	// (2xx/3xx for HTTP, connect for TCP, exit 0 for exec).
	OK bool

	// Reason is a human-readable string explaining a failure. Empty
	// on success. The Runner surfaces this in trigger logs so
	// operators reading `vd describe pod` see "5xx from /healthz"
	// rather than just "probe failed."
	Reason string

	// At is when the sample completed. Used by Runner internally
	// for cooldown / debounce logic; surfaced in callbacks so
	// downstream metrics can timestamp the event.
	At time.Time
}

// Validate checks the Action shape — exactly one selector must be
// populated. Returns nil for a valid spec; non-nil error explains
// which constraint failed. Called by the manifest layer before
// constructing a Runner (Runner.Run does NOT re-validate — it
// trusts the caller; production wiring will always have gone
// through Validate, tests do too).
func (s *Spec) Validate() error {
	count := 0
	if s.Action.HTTPGet != nil {
		count++

		if s.Action.HTTPGet.Path == "" {
			return fmt.Errorf("http_get.path is required")
		}

		if !strings.HasPrefix(s.Action.HTTPGet.Path, "/") {
			return fmt.Errorf("http_get.path must start with '/'")
		}

		if s.Action.HTTPGet.Port <= 0 {
			return fmt.Errorf("http_get.port is required (got %d)", s.Action.HTTPGet.Port)
		}
	}

	if s.Action.TCPSocket != nil {
		count++

		if s.Action.TCPSocket.Port <= 0 {
			return fmt.Errorf("tcp_socket.port is required (got %d)", s.Action.TCPSocket.Port)
		}
	}

	if s.Action.Exec != nil {
		count++

		if len(s.Action.Exec.Command) == 0 {
			return fmt.Errorf("exec.command must be non-empty")
		}
	}

	switch count {
	case 0:
		return fmt.Errorf("probe requires exactly one of http_get / tcp_socket / exec (none set)")
	case 1:
		// ok
	default:
		return fmt.Errorf("probe requires exactly one of http_get / tcp_socket / exec (got %d)", count)
	}

	if s.InitialDelay < 0 {
		return fmt.Errorf("initial_delay must be >= 0")
	}

	if s.Period < 0 {
		return fmt.Errorf("period must be >= 0 (0 → 10s default)")
	}

	if s.Timeout < 0 {
		return fmt.Errorf("timeout must be >= 0 (0 → 1s default)")
	}

	if s.FailureThreshold < 0 {
		return fmt.Errorf("failure_threshold must be >= 0 (0 → 3 default)")
	}

	if s.SuccessThreshold < 0 {
		return fmt.Errorf("success_threshold must be >= 0 (0 → 1 default)")
	}

	return nil
}

// ExecRunner is the callback the probe package uses to run exec
// probes inside containers. Tests substitute a fake; production wires
// docker.ContainerManager.Exec (or equivalent).
//
// Returns (exitCode, stderr) for diagnostics on failure. The exit
// code drives success/failure decision; stderr feeds the Result.Reason.
type ExecRunner interface {
	Exec(ctx context.Context, container string, command []string) (exitCode int, stderr string, err error)
}

// HTTPGetter is the callback for HTTP probes. Default impl uses
// net/http; tests pass a fake that returns canned responses without
// network. Keeps probe tests hermetic.
type HTTPGetter interface {
	Get(ctx context.Context, url string, headers map[string]string) (statusCode int, err error)
}

// TCPDialer is the callback for TCP probes. Same testing rationale as
// HTTPGetter — production uses net.DialTimeout; tests stub it.
type TCPDialer interface {
	Dial(ctx context.Context, address string) error
}

// defaultHTTPGetter is the production HTTP probe executor. Tolerates
// self-signed certs because internal probes target containers on the
// voodu0 bridge — no public CA chain involved.
type defaultHTTPGetter struct{}

func (defaultHTTPGetter) Get(ctx context.Context, url string, headers map[string]string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// One client per call: cheap (no connection reuse needed for
	// probes — they fire every few seconds with fresh state each
	// time), and avoids cross-probe state leaks if one container
	// returns gzipped chunked weirdness.
	client := &http.Client{Timeout: 0} // timeout is on the context

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}

	defer resp.Body.Close()

	return resp.StatusCode, nil
}

// defaultTCPDialer is the production TCP probe executor.
type defaultTCPDialer struct{}

func (defaultTCPDialer) Dial(ctx context.Context, address string) error {
	var d net.Dialer

	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}

	return conn.Close()
}

// execute runs one probe sample and returns the Result. Pure-ish:
// the only side effects are the HTTP/TCP/exec call themselves.
// Tests inject fake getters/dialers/runners through the executors
// argument; production passes defaults.
//
// Container is the docker container name (or any identifier the
// caller chose); used for exec probes and for building the HTTP
// URL when the action specifies a Host that's a container name
// instead of an IP.
func execute(ctx context.Context, spec Spec, container, host string, execs executors) Result {
	now := time.Now()

	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	switch {
	case spec.Action.HTTPGet != nil:
		a := spec.Action.HTTPGet

		scheme := a.Scheme
		if scheme == "" {
			scheme = "http"
		}

		// host comes in pre-resolved by the caller (container IP or
		// hostname reachable from where the probe runs). The action
		// could override this in a future revision, but for now the
		// caller's choice is canonical.
		url := fmt.Sprintf("%s://%s:%d%s", scheme, host, a.Port, a.Path)

		code, err := execs.http.Get(ctx, url, a.HTTPHeaders)
		if err != nil {
			return Result{OK: false, Reason: fmt.Sprintf("GET %s: %v", url, err), At: now}
		}

		// k8s convention: 2xx / 3xx → success; anything else → fail.
		if code >= 200 && code < 400 {
			return Result{OK: true, At: now}
		}

		return Result{OK: false, Reason: fmt.Sprintf("GET %s → %d", url, code), At: now}

	case spec.Action.TCPSocket != nil:
		a := spec.Action.TCPSocket

		address := fmt.Sprintf("%s:%d", host, a.Port)

		if err := execs.tcp.Dial(ctx, address); err != nil {
			return Result{OK: false, Reason: fmt.Sprintf("dial %s: %v", address, err), At: now}
		}

		return Result{OK: true, At: now}

	case spec.Action.Exec != nil:
		a := spec.Action.Exec

		// Production wires DockerContainerManager.Exec here. Tests
		// that don't provide an Exec runner shouldn't crash the
		// probe loop — surface a clear failure so the operator
		// sees what's wrong, treat the probe as failed.
		if execs.exec == nil {
			return Result{OK: false, Reason: "no exec runner configured", At: now}
		}

		code, stderr, err := execs.exec.Exec(ctx, container, a.Command)
		if err != nil {
			return Result{OK: false, Reason: fmt.Sprintf("exec %v: %v", a.Command, err), At: now}
		}

		if code == 0 {
			return Result{OK: true, At: now}
		}

		reason := fmt.Sprintf("exec %v: exit %d", a.Command, code)
		if stderr != "" {
			reason = fmt.Sprintf("%s — %s", reason, strings.TrimSpace(stderr))
		}

		return Result{OK: false, Reason: reason, At: now}

	default:
		// Validate would have caught this; defensive belt.
		return Result{OK: false, Reason: "no action selector", At: now}
	}
}

// executors is the test seam — bundles the three callback shapes so
// execute() takes one argument instead of three.
type executors struct {
	http HTTPGetter
	tcp  TCPDialer
	exec ExecRunner
}
