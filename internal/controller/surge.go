package controller

import "strings"

// How a workload's host ports are configured, as a plugin sees it.
//
// Exposed so a plugin does not have to reimplement normalizePort to
// find out — and a reimplementation would drift from it, which is the
// same bug as not knowing. Derived from the identical helper the
// rollout uses to decide whether it can surge, so the two can never
// disagree.
const (
	// hostPortsNone: the workload publishes nothing on the host.
	// The ideal shape in front of a load balancer — the balancer is
	// then the only way in, which is what it is for.
	hostPortsNone = "none"

	// hostPortsEphemeral: ports are published, but docker chooses
	// the host side. Replicas coexist, so a rollout can surge.
	hostPortsEphemeral = "ephemeral"

	// hostPortsFixed: at least one host port is pinned. Only one
	// container can hold it, which makes the workload single-replica
	// and leaves a load balancer with nothing to balance.
	//
	// Shapes this package cannot classify are reported as fixed:
	// erring toward "cannot surge" costs a slower rollout, erring
	// the other way costs a failed one.
	hostPortsFixed = "fixed"
)

// hostPortMode classifies a workload's host ports for the plugins that
// need to reason about them.
func hostPortMode(spec deploymentSpec) string {
	if len(spec.Ports) == 0 {
		return hostPortsNone
	}

	if !canSurge(spec) {
		return hostPortsFixed
	}

	return hostPortsEphemeral
}

// canSurge reports whether a replacement replica may run alongside the
// one it replaces during a rolling restart.
//
// The answer is entirely about host ports. Docker gives a host port to
// one container at a time, so a deployment that pins one cannot have
// two replicas alive at once — not during a surge, and not at steady
// state either. Such a deployment was already single-replica by
// construction, which is why declining to surge costs it nothing.
//
// The usual form is not pinned: `ports = ["8080"]` normalises to
// 127.0.0.1::8080, which asks docker to choose the host side. So surge
// is available for most workloads, and specifically for every workload
// fronted by a load balancer — a pinned host port and a load balancer
// are mutually exclusive anyway, since a pinned port is itself the
// single way in and leaves nothing to balance.
func canSurge(spec deploymentSpec) bool {
	for _, p := range spec.Ports {
		if pinsHostPort(p) {
			return false
		}
	}

	return true
}

// pinsHostPort reports whether a port mapping names the host side.
func pinsHostPort(port string) bool {
	// IPv6 literals stay classified as pinned. parseHostBinding can
	// read them, but surge has always treated them conservatively,
	// and the asymmetry still holds: a wrong "cannot surge" costs a
	// slower rollout, a wrong "can surge" costs a failed one.
	if strings.HasPrefix(normalizePort(port), "[") {
		return true
	}

	b, ok := parseHostBinding(port)
	if !ok {
		// A shape normalizePort did not produce. Same conservative
		// call as the bracket case.
		return true
	}

	return b.HostPort != ""
}

// hostBinding is a port mapping in the shape docker receives it: the
// address the host side binds, the host port, and the container side
// (with its protocol suffix, when there is one).
type hostBinding struct {
	IP        string
	HostPort  string
	Container string
}

// parseHostBinding reads a port mapping through normalizePort, so every
// shape reduces to ip:host:container and nothing here re-derives its
// defaults. It is the one reading of a port spec: surge and the
// empty-host-port warning both go through it, so they cannot disagree
// about what a mapping means.
//
// An IPv6 address keeps its brackets in IP. An empty HostPort is
// docker's "pick one for me". ok is false for a shape normalizePort
// did not produce; what that means is the caller's decision.
func parseHostBinding(port string) (hostBinding, bool) {
	normalized := normalizePort(port)

	var ip, rest string

	if strings.HasPrefix(normalized, "[") {
		end := strings.Index(normalized, "]:")
		if end < 0 {
			return hostBinding{}, false
		}

		ip, rest = normalized[:end+1], normalized[end+2:]
	} else {
		var found bool

		ip, rest, found = strings.Cut(normalized, ":")
		if !found {
			return hostBinding{}, false
		}
	}

	host, container, found := strings.Cut(rest, ":")
	if !found || strings.Contains(container, ":") {
		return hostBinding{}, false
	}

	return hostBinding{IP: ip, HostPort: host, Container: container}, true
}
