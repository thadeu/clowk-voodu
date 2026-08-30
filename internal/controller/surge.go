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
//
// Reads the normalised form so every shape reduces to the same layout
// and this does not have to re-derive normalizePort's defaults.
func pinsHostPort(port string) bool {
	normalized := normalizePort(port)

	// IPv6 literals keep their brackets and are passed to docker
	// untouched. Rather than parse them, treat them as pinned: a
	// wrong "cannot surge" costs a slower rollout, a wrong "can
	// surge" costs a failed one.
	if strings.HasPrefix(normalized, "[") {
		return true
	}

	parts := strings.Split(normalized, ":")

	// ip:host:container — the host field is the middle one, and an
	// empty middle is docker's "pick one for me".
	if len(parts) == 3 {
		return parts[1] != ""
	}

	// Anything else is a shape normalizePort did not produce. Same
	// conservative call as the bracket case.
	return true
}
