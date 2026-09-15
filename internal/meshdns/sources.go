package meshdns

import (
	"bufio"
	"bytes"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Tunnel is the voodu WireGuard network. A host's address on it is
// 10.254.X.Y, and its containers live in 10.X.Y.0/24.
var Tunnel = netip.MustParsePrefix("10.254.0.0/16")

// WGPeers lists the other hosts from wg0's own peer list: a peer's /32
// inside the tunnel is its address there, and its mesh DNS listens on it.
// Nothing to configure — the WireGuard config already says who the peers
// are.
type WGPeers struct {
	Port uint16

	// Run returns `wg show wg0 allowed-ips`. A seam for tests.
	Run func() ([]byte, error)

	mu    sync.Mutex
	at    time.Time
	peers []netip.AddrPort
}

// peersRefresh is how long a read of wg0 is trusted. Peers change when the
// operator edits WireGuard, which is rare.
const peersRefresh = 30 * time.Second

func (w *WGPeers) List() []netip.AddrPort {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.at.IsZero() && time.Since(w.at) < peersRefresh {
		return w.peers
	}

	run := w.Run
	if run == nil {
		run = func() ([]byte, error) { return exec.Command("wg", "show", "wg0", "allowed-ips").Output() }
	}

	// A failed read keeps the last good list: a transient error must not
	// make every remote name vanish.
	if out, err := run(); err == nil {
		var peers []netip.AddrPort

		for _, addr := range ParseAllowedIPs(out, Tunnel) {
			peers = append(peers, netip.AddrPortFrom(addr, w.Port))
		}

		w.peers = peers
		w.at = time.Now()
	}

	return w.peers
}

// ParseAllowedIPs reads `wg show <iface> allowed-ips` — one peer per line,
// its public key then its allowed prefixes — and returns each peer's /32
// inside tunnel: its address on the tunnel. A peer's container subnet sits
// on the same line and is not an address to ask.
func ParseAllowedIPs(out []byte, tunnel netip.Prefix) []netip.Addr {
	var addrs []netip.Addr

	sc := bufio.NewScanner(bytes.NewReader(out))

	for sc.Scan() {
		fields := strings.Fields(sc.Text())

		for _, f := range fields[min(1, len(fields)):] {
			p, err := netip.ParsePrefix(f)
			if err != nil || !p.Addr().Is4() || p.Bits() != 32 || !tunnel.Contains(p.Addr()) {
				continue
			}

			addrs = append(addrs, p.Addr())
		}
	}

	return addrs
}

// ReadUpstreams returns the host's real resolvers from the first of paths
// that exists and names one. systemd-resolved's upstream file comes first:
// with it, /etc/resolv.conf points at the 127.0.0.53 stub, which a
// container cannot reach.
func ReadUpstreams(paths ...string) []netip.AddrPort {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		if ups := ParseResolvConf(data); len(ups) > 0 {
			return ups
		}
	}

	return nil
}

// ParseResolvConf returns the nameservers in a resolv.conf, minus loopback
// ones: a stub on 127.0.0.x answers the host, not a container.
func ParseResolvConf(data []byte) []netip.AddrPort {
	var ups []netip.AddrPort

	sc := bufio.NewScanner(bytes.NewReader(data))

	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}

		addr, err := netip.ParseAddr(fields[1])
		if err != nil || addr.IsLoopback() {
			continue
		}

		ups = append(ups, netip.AddrPortFrom(addr, 53))
	}

	return ups
}
