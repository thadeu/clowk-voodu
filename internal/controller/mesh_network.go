package controller

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/meshdns"
)

// meshNetwork is this host's place in the cross-VM mesh: a routed voodu0,
// the host's address on the voodu tunnel, and the resolvers every container
// is created with.
type meshNetwork struct {
	subnet     netip.Prefix      // voodu0's subnet: this host's containers
	gateway    netip.Addr        // voodu0's gateway: where they reach the mesh DNS
	tunnel     netip.Addr        // wg0's address at start; invalid without wg0
	readTunnel func() netip.Addr // re-reads wg0; wg0Address in production
	upstreams  []netip.AddrPort  // the host's own resolvers
}

// detectMeshNetwork reads voodu0 and wg0. nil when voodu0 is not routed: a
// host local to itself gets no mesh DNS, and its containers keep docker's
// default resolvers exactly as before.
func detectMeshNetwork(logf func(string, ...any)) *meshNetwork {
	plan, ok, err := docker.InspectNetworkAddressPlan("voodu0")
	if err != nil {
		logf("mesh: cannot read voodu0: %v", err)

		return nil
	}

	if !ok || !plan.IPRange.IsValid() {
		return nil
	}

	subnet := plan.Subnet.Masked()

	gateway := plan.Gateway
	if !gateway.IsValid() {
		gateway = subnet.Addr().Next()
	}

	m := &meshNetwork{
		subnet:     subnet,
		gateway:    gateway,
		tunnel:     wg0Address(),
		readTunnel: wg0Address,
		upstreams:  meshdns.ReadUpstreams("/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"),
	}

	if !m.tunnel.IsValid() {
		logf("mesh: voodu0 is routed but wg0 has no address in %s — names on other hosts will not resolve", meshdns.Tunnel)
	}

	if len(m.upstreams) == 0 {
		logf("mesh: no host resolver found — containers have no fallback while the controller is down")
	}

	return m
}

// wg0Address is this host's address on the voodu tunnel, read from the
// interface itself.
func wg0Address() netip.Addr {
	iface, err := net.InterfaceByName("wg0")
	if err != nil {
		return netip.Addr{}
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return netip.Addr{}
	}

	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}

		if ip, ok := netip.AddrFromSlice(n.IP.To4()); ok && meshdns.Tunnel.Contains(ip) {
			return ip
		}
	}

	return netip.Addr{}
}

// containerDNS is what every container is created with: the mesh DNS, then
// the host's resolvers. The order is load-bearing — docker's embedded DNS
// stops at the first NXDOMAIN, so a host resolver in front would deny every
// .voodu name before the mesh saw it — and the host's resolvers after it are
// what keeps internet names resolving while the controller restarts. nil on
// a host without a routed voodu0.
func (m *meshNetwork) containerDNS() []string {
	if m == nil {
		return nil
	}

	out := []string{m.gateway.String()}

	for _, up := range m.upstreams {
		out = append(out, up.Addr().String())
	}

	return out
}

// serve runs the mesh DNS on voodu0's gateway, for this host's containers,
// and on the tunnel address, for the other hosts, until ctx ends. Never on
// 0.0.0.0: systemd-resolved holds 127.0.0.53, and nothing else should reach
// it. A listener that cannot bind is retried — the address may not be up
// yet.
func (m *meshNetwork) serve(ctx context.Context, srv *meshdns.Server, logf func(string, ...any)) {
	if m == nil || srv == nil {
		return
	}

	go m.serveOn(ctx, srv, func() netip.Addr { return m.gateway }, "voodu0 gateway", logf)

	// The tunnel address is read again on every attempt: wg0 may come up
	// after the controller (boot order, a wg-quick restart), and the other
	// hosts must still find this one.
	go m.serveOn(ctx, srv, m.tunnelAddress, "wg0", logf)
}

// server builds the mesh DNS for this host. nil on a host without a
// routed voodu0, and every use of it is nil-safe.
func (m *meshNetwork) server(index meshdns.Index, logf func(string, ...any)) *meshdns.Server {
	if m == nil {
		return nil
	}

	return &meshdns.Server{
		Local:     m.subnet,
		Tunnel:    meshdns.Tunnel,
		Index:     index,
		Peers:     &meshdns.WGPeers{Port: 53},
		Upstreams: m.upstreams,
		Logf:      logf,
	}
}

// meshResolver is the seam the ingress handler resolves a remote service
// through. nil on a host that is local to itself.
func meshResolver(srv *meshdns.Server) func(context.Context, string) []netip.Addr {
	if srv == nil {
		return nil
	}

	return srv.Resolve
}

// serveOn keeps the mesh DNS listening on the address that at returns,
// retrying every 5s while the address is missing or the bind fails.
func (m *meshNetwork) serveOn(ctx context.Context, srv *meshdns.Server, at func() netip.Addr, what string, logf func(string, ...any)) {
	waited := false

	for {
		addr := at()

		if addr.IsValid() {
			listen := netip.AddrPortFrom(addr, meshDNSPort)

			logf("mesh dns: answering .voodu on %s (%s)", listen, what)

			err := srv.ListenAndServe(ctx, listen)
			if ctx.Err() != nil {
				return
			}

			logf("mesh dns: %s: %v — retrying in 5s", listen, err)
		} else if !waited {
			logf("mesh dns: %s has no address in %s yet — waiting", what, meshdns.Tunnel)
			waited = true
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// tunnelAddress is wg0's current address: the one detected at start, or a
// fresh read when there was none.
func (m *meshNetwork) tunnelAddress() netip.Addr {
	if m.tunnel.IsValid() {
		return m.tunnel
	}

	if addr := m.readTunnel(); addr.IsValid() {
		return addr
	}

	return netip.Addr{}
}

// meshDNSPort is where the mesh DNS listens; tests move it off 53.
var meshDNSPort uint16 = 53

// meshIndexRefresh bounds how stale this host's own answers can be: a
// deployment's replicas move on every deploy.
const meshIndexRefresh = 2 * time.Second

// meshIndex answers this host's own .voodu names from docker: every running
// deployment and statefulset container on voodu0, under the same aliases
// docker registers for it here. Read only when asked, at most every
// meshIndexRefresh.
type meshIndex struct {
	Pods PodsLister

	// now is swapped by tests.
	now func() time.Time

	mu    sync.Mutex
	at    time.Time
	names map[string][]netip.Addr
}

func (x *meshIndex) Lookup(name string) []netip.Addr {
	x.mu.Lock()
	defer x.mu.Unlock()

	now := time.Now()
	if x.now != nil {
		now = x.now()
	}

	if x.names == nil || now.Sub(x.at) >= meshIndexRefresh {
		// A failed read keeps the last names: a docker hiccup must not
		// make this host's services vanish from every other host.
		if pods, err := x.Pods.ListPods(); err == nil {
			x.names = meshNames(pods)
			x.at = now
		}
	}

	return x.names[name]
}

// meshNames maps each .voodu alias to the running containers behind it —
// the round-robin name to every replica, a statefulset pod's own name to
// that pod. Jobs and cronjobs register no aliases (they would collide with a
// deployment of the same scope and name) and so are not here either.
func meshNames(pods []Pod) map[string][]netip.Addr {
	names := map[string][]netip.Addr{}

	add := func(aliases []string, ip netip.Addr) {
		for _, alias := range aliases {
			if !strings.HasSuffix(alias, "."+networkAliasTLD) {
				continue
			}

			key := alias + "."

			if !slices.Contains(names[key], ip) {
				names[key] = append(names[key], ip)
			}
		}
	}

	for _, p := range pods {
		if !p.Running || (p.Kind != string(KindDeployment) && p.Kind != string(KindStatefulset)) {
			continue
		}

		ip, err := netip.ParseAddr(p.IP)
		if err != nil {
			continue
		}

		add(BuildNetworkAliases(p.Scope, p.ResourceName), ip)

		if p.Kind == string(KindStatefulset) {
			if ordinal, err := strconv.Atoi(p.ReplicaID); err == nil {
				add(BuildPodNetworkAliases(p.Scope, p.ResourceName, ordinal), ip)
			}
		}
	}

	return names
}
