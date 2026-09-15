package controller

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// emptyHostPortWarnings flags port mappings that publish on a non-loopback
// address and leave the host port for docker to choose.
//
// Docker picks a new host port on every recreate. Behind loopback nothing
// cares — ingress dials the container by name — but on any other address
// something outside the container reached it through that port: another VM,
// a connection string written into an app's env. The next recreate moves the
// port and that caller connects to nothing, with nothing in voodu noticing.
// `ports = ["${REDIS_BIND_IP}::6379"]` once came up as 32768, and a REDIS_URL
// was written against it.
//
// A warning and not an error: the batch still applies. It has to run on the
// post-expansion batch, because a plugin block's `ports` only exists once the
// plugin has turned it into a statefulset.
func emptyHostPortWarnings(mans []*Manifest) []string {
	var out []string

	for _, m := range mans {
		if m.Kind != KindDeployment && m.Kind != KindStatefulset {
			continue
		}

		var spec struct {
			Ports []string `json:"ports"`
		}

		if err := json.Unmarshal(m.Spec, &spec); err != nil {
			continue
		}

		for i, port := range spec.Ports {
			b, ok := parseHostBinding(port)
			if !ok || b.HostPort != "" || isLoopbackBind(b.IP) {
				continue
			}

			out = append(out, fmt.Sprintf(
				"%s/%s/%s: ports[%d] %q leaves the host port empty on a non-loopback address — docker picks a new port on every recreate. Pin it: %q",
				m.Kind, m.Scope, m.Name, i, port, pinnedSuggestion(b),
			))
		}
	}

	return out
}

// isLoopbackBind reports whether a bind address only reaches the host itself.
// An empty address is docker's "every interface" — the opposite of loopback.
func isLoopbackBind(ip string) bool {
	parsed := net.ParseIP(strings.Trim(ip, "[]"))

	return parsed != nil && parsed.IsLoopback()
}

// pinnedSuggestion is the mapping with the host side pinned to the container
// port, which is the usual fix.
func pinnedSuggestion(b hostBinding) string {
	host, _, _ := strings.Cut(b.Container, "/")

	return b.IP + ":" + host + ":" + b.Container
}
