// wire.go is how voodu manages wg0's peers: the operator adds the other
// hosts with `vd wire add`, the record lives in etcd, and wg0 is brought
// to match it without dropping the tunnel — `wg addconf` adds and updates
// the peers in the file, `wg set peer remove` drops the ones that left.
//
// Not `wg syncconf`: it rewrites the whole interface from the file, and a
// file without an [Interface] section resets the listen port to a random
// one — which is how the first end-to-end run ended with both hosts
// dialling a port nobody listened on. addconf touches peers only.
//
// wg0.conf is never edited. The install writes it once (address, port,
// key) and voodu keeps its peers in a separate file in wg's native
// format, reapplied by a PostUp on boot and by the controller on start.
// No INI parser, no file the operator and voodu both own.

package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.voodu.clowk.in/internal/meshdns"
)

// WirePeer is another voodu host on the tunnel.
type WirePeer struct {
	// PublicKey is the peer's WireGuard key, base64.
	PublicKey string `json:"public_key"`

	// Address is the peer's address on the tunnel (10.254.X.Y). Its
	// containers live in 10.X.Y.0/24, derived — so AllowedIPs needs
	// nothing else.
	Address string `json:"address"`

	// Endpoint is host:port where the peer listens. Empty for a peer
	// behind NAT that always dials in.
	Endpoint string `json:"endpoint,omitempty"`

	AddedAt time.Time `json:"added_at"`
}

// wireKeepalive keeps a NAT mapping open; 25s is WireGuard's own
// recommendation.
const wireKeepalive = 25

// Validate checks the shape the operator typed. The address must be on
// the tunnel and not this host's own.
func (p WirePeer) Validate(local netip.Addr) error {
	key, err := base64.StdEncoding.DecodeString(p.PublicKey)
	if err != nil || len(key) != 32 {
		return fmt.Errorf("public key %q: not a WireGuard key (base64 of 32 bytes)", p.PublicKey)
	}

	addr, err := netip.ParseAddr(p.Address)
	if err != nil || !addr.Is4() {
		return fmt.Errorf("address %q: not an IPv4 address", p.Address)
	}

	if !meshdns.Tunnel.Contains(addr) {
		return fmt.Errorf("address %s: not in the voodu tunnel %s", addr, meshdns.Tunnel)
	}

	if local.IsValid() && addr == local {
		return fmt.Errorf("address %s: that is this host", addr)
	}

	if p.Endpoint != "" {
		host, port, err := net.SplitHostPort(p.Endpoint)
		if err != nil || host == "" {
			return fmt.Errorf("endpoint %q: want host:port", p.Endpoint)
		}

		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("endpoint %q: bad port", p.Endpoint)
		}
	}

	return nil
}

// AllowedIPs is what wg0 accepts from and routes to the peer: its tunnel
// address and its containers' subnet.
func (p WirePeer) AllowedIPs() []netip.Prefix {
	addr, err := netip.ParseAddr(p.Address)
	if err != nil {
		return nil
	}

	return []netip.Prefix{
		netip.PrefixFrom(addr, 32),
		containerSubnetFor(addr),
	}
}

// containerSubnetFor maps a host's tunnel address to its voodu0 subnet:
// 10.254.X.Y → 10.X.Y.0/24. The same rule the install applies.
func containerSubnetFor(tunnel netip.Addr) netip.Prefix {
	b := tunnel.As4()

	return netip.PrefixFrom(netip.AddrFrom4([4]byte{10, b[2], b[3], 0}), 24)
}

// WirePeersConf renders the peers in wg's native format, the shape
// `wg syncconf` reads. Sorted by address so the file is stable.
func WirePeersConf(peers []WirePeer) []byte {
	sorted := append([]WirePeer(nil), peers...)

	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Address < sorted[j].Address })

	var b bytes.Buffer

	b.WriteString("# managed by voodu — edit with `vd wire`, not by hand\n")

	for _, p := range sorted {
		var allowed []string

		for _, pfx := range p.AllowedIPs() {
			allowed = append(allowed, pfx.String())
		}

		fmt.Fprintf(&b, "\n[Peer]\nPublicKey = %s\nAllowedIPs = %s\nPersistentKeepalive = %d\n",
			p.PublicKey, strings.Join(allowed, ", "), wireKeepalive)

		if p.Endpoint != "" {
			fmt.Fprintf(&b, "Endpoint = %s\n", p.Endpoint)
		}
	}

	return b.Bytes()
}

// WireLink is a peer as wg0 sees it right now.
type WireLink struct {
	Endpoint      string    `json:"endpoint,omitempty"`
	LastHandshake time.Time `json:"last_handshake,omitempty"`
	RxBytes       int64     `json:"rx_bytes"`
	TxBytes       int64     `json:"tx_bytes"`
}

// ParseWGDump reads `wg show wg0 dump`: the first line is the interface,
// each following line a peer — public key, preshared key, endpoint,
// allowed ips, last handshake (unix), rx, tx, keepalive. Keyed by public
// key.
func ParseWGDump(out []byte) map[string]WireLink {
	links := map[string]WireLink{}

	sc := bufio.NewScanner(bytes.NewReader(out))

	first := true

	for sc.Scan() {
		if first {
			first = false

			continue
		}

		f := strings.Split(sc.Text(), "\t")
		if len(f) < 7 {
			continue
		}

		l := WireLink{}

		if f[2] != "(none)" {
			l.Endpoint = f[2]
		}

		if ts, err := strconv.ParseInt(f[4], 10, 64); err == nil && ts > 0 {
			l.LastHandshake = time.Unix(ts, 0).UTC()
		}

		l.RxBytes, _ = strconv.ParseInt(f[5], 10, 64)
		l.TxBytes, _ = strconv.ParseInt(f[6], 10, 64)

		links[f[0]] = l
	}

	return links
}

// WireIdentity is this host, as the other side needs it for `vd wire add`.
type WireIdentity struct {
	PublicKey  string `json:"public_key"`
	Address    string `json:"address"`
	ListenPort int    `json:"listen_port"`

	// Endpoint is the outbound IP and the listen port — the usual value,
	// which the operator overrides when the host sits behind NAT.
	Endpoint string `json:"endpoint,omitempty"`
}

// Wire applies the etcd peers to wg0 and reads wg0 back.
type Wire struct {
	Store Store

	// ConfPath is where the peers file is written: under VOODU_ROOT, the
	// one path the controller's sandbox lets it write.
	ConfPath string

	// Run executes wg. A seam for tests; nil runs the real binary.
	Run func(args ...string) ([]byte, error)

	// LocalAddress is this host's tunnel address. Nil reads wg0.
	LocalAddress func() netip.Addr

	// OutboundIP is what the host's packets leave with. Nil reads the
	// routing table.
	OutboundIP func() netip.Addr

	// Bridge is voodu0's host interface, the name a firewall rule needs.
	// Empty on a host whose voodu0 is not routed.
	Bridge func() string

	// RunIP executes `ip`. A seam for tests; nil runs the real binary.
	RunIP func(args ...string) ([]byte, error)

	Logf func(string, ...any)

	// mu serialises add, remove and apply: two at once would race on the
	// peers file and run syncconf over each other.
	mu sync.Mutex
}

func (w *Wire) run(args ...string) ([]byte, error) {
	if w.Run != nil {
		return w.Run(args...)
	}

	out, err := exec.Command("wg", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("wg %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return out, nil
}

func (w *Wire) ip(args ...string) ([]byte, error) {
	if w.RunIP != nil {
		return w.RunIP(args...)
	}

	out, err := exec.Command("ip", args...).CombinedOutput()

	if err != nil {
		return nil, fmt.Errorf("ip %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return out, nil
}

func (w *Wire) local() netip.Addr {
	if w.LocalAddress != nil {
		return w.LocalAddress()
	}

	return wg0Address()
}

// Apply writes the peers file from etcd and syncs wg0 to it. Called on
// every add and remove, and once on controller start so a rebooted host —
// or a recreated wg0 — comes back with the peers etcd says it has.
func (w *Wire) Apply(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.apply(ctx)
}

// ErrWireUnavailable wraps a failure past validation — the store or wg
// itself — so the handler can answer 503 rather than 400.
var ErrWireUnavailable = errors.New("wire unavailable")

func unavailable(err error) error {
	return fmt.Errorf("%w: %v", ErrWireUnavailable, err)
}

func (w *Wire) apply(ctx context.Context) error {
	peers, err := w.Store.ListWirePeers(ctx)
	if err != nil {
		return unavailable(err)
	}

	if err := os.MkdirAll(filepath.Dir(w.ConfPath), 0o700); err != nil {
		return unavailable(err)
	}

	tmp := w.ConfPath + ".tmp"

	if err := os.WriteFile(tmp, WirePeersConf(peers), 0o600); err != nil {
		return unavailable(err)
	}

	if err := os.Rename(tmp, w.ConfPath); err != nil {
		return unavailable(err)
	}

	if _, err := w.run("addconf", "wg0", w.ConfPath); err != nil {
		return unavailable(err)
	}

	// addconf never removes: a peer that left etcd is still on wg0 until
	// told otherwise.
	dump, err := w.run("show", "wg0", "dump")

	if err != nil {
		return unavailable(err)
	}

	wanted := map[string]bool{}

	for _, p := range peers {
		wanted[p.PublicKey] = true
	}

	for key := range ParseWGDump(dump) {
		if wanted[key] {
			continue
		}

		if _, err := w.run("set", "wg0", "peer", key, "remove"); err != nil {
			return unavailable(err)
		}
	}

	return w.routes(peers)
}

// routes keeps the kernel's routes to the peers' container subnets in step
// with the peers. wg-quick installs routes for AllowedIPs only at `up`, from
// wg0.conf; a peer that arrives through addconf gets none, and a packet for
// the other host's containers leaves through the default route instead —
// how the first end-to-end run lost every connection while every name
// resolved. The tunnel addresses themselves need nothing: wg0's own /16
// covers them.
func (w *Wire) routes(peers []WirePeer) error {
	wanted := map[netip.Prefix]bool{}

	for _, p := range peers {
		if addr, err := netip.ParseAddr(p.Address); err == nil {
			wanted[containerSubnetFor(addr)] = true
		}
	}

	for prefix := range wanted {
		if _, err := w.ip("-4", "route", "replace", prefix.String(), "dev", "wg0"); err != nil {
			return unavailable(err)
		}
	}

	out, err := w.ip("-4", "route", "show", "dev", "wg0")

	if err != nil {
		return unavailable(err)
	}

	for _, prefix := range ParseContainerRoutes(out) {
		if wanted[prefix] {
			continue
		}

		if _, err := w.ip("-4", "route", "del", prefix.String(), "dev", "wg0"); err != nil {
			return unavailable(err)
		}
	}

	return nil
}

// ParseContainerRoutes reads `ip -4 route show dev wg0` and returns the
// routes that look like a peer's container subnet — a /24 in 10.0.0.0/8
// outside the tunnel. The tunnel's own /16 and anything else on the
// interface is left alone.
func ParseContainerRoutes(out []byte) []netip.Prefix {
	var routes []netip.Prefix

	sc := bufio.NewScanner(bytes.NewReader(out))

	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}

		p, err := netip.ParsePrefix(fields[0])
		if err != nil || p.Bits() != 24 || !p.Addr().Is4() || p.Addr().As4()[0] != 10 || meshdns.Tunnel.Contains(p.Addr()) {
			continue
		}

		routes = append(routes, p)
	}

	return routes
}

// Add validates, stores and applies one peer.
func (w *Wire) Add(ctx context.Context, p WirePeer) (WirePeer, error) {
	p.PublicKey = strings.TrimSpace(p.PublicKey)
	p.Address = strings.TrimSpace(p.Address)
	p.Endpoint = strings.TrimSpace(p.Endpoint)

	if err := p.Validate(w.local()); err != nil {
		return WirePeer{}, err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// WireGuard identifies a peer by its key: a second address with the
	// same key would replace the first on the interface, and `list` would
	// show one of them as not applied without saying why.
	existing, err := w.Store.ListWirePeers(ctx)
	if err != nil {
		return WirePeer{}, unavailable(err)
	}

	for _, e := range existing {
		if e.PublicKey == p.PublicKey && e.Address != p.Address {
			return WirePeer{}, fmt.Errorf("public key already used by peer %s — remove it first, or check the address", e.Address)
		}
	}

	if p.AddedAt.IsZero() {
		p.AddedAt = time.Now().UTC()
	}

	if err := w.Store.PutWirePeer(ctx, p); err != nil {
		return WirePeer{}, unavailable(err)
	}

	return p, w.apply(ctx)
}

// ErrWirePeerNotFound is `vd wire remove` of an address nobody added.
var ErrWirePeerNotFound = errors.New("no peer with that address")

// Remove drops one peer by address and applies.
func (w *Wire) Remove(ctx context.Context, address string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	deleted, err := w.Store.DeleteWirePeer(ctx, strings.TrimSpace(address))
	if err != nil {
		return unavailable(err)
	}

	if !deleted {
		return fmt.Errorf("%w: %s", ErrWirePeerNotFound, address)
	}

	return w.apply(ctx)
}

// WirePeerStatus is a stored peer joined with what wg0 reports for it.
type WirePeerStatus struct {
	WirePeer

	// Applied is false when wg0 has no such peer: etcd says one thing,
	// the interface another. The next Apply fixes it.
	Applied bool `json:"applied"`

	Link *WireLink `json:"link,omitempty"`
}

// Status is the identity and every peer with its link, for `vd wire`.
func (w *Wire) Status(ctx context.Context) (WireIdentity, []WirePeerStatus, error) {
	id, err := w.Identity()
	if err != nil {
		return WireIdentity{}, nil, err
	}

	peers, err := w.Store.ListWirePeers(ctx)
	if err != nil {
		return WireIdentity{}, nil, err
	}

	sort.Slice(peers, func(i, j int) bool { return peers[i].Address < peers[j].Address })

	links := map[string]WireLink{}

	if out, err := w.run("show", "wg0", "dump"); err == nil {
		links = ParseWGDump(out)
	} else if w.Logf != nil {
		w.Logf("wire: %v", err)
	}

	out := make([]WirePeerStatus, 0, len(peers))

	for _, p := range peers {
		st := WirePeerStatus{WirePeer: p}

		if l, ok := links[p.PublicKey]; ok {
			st.Applied = true
			st.Link = &l
		}

		out = append(out, st)
	}

	return id, out, nil
}

// Identity reads this host's key, port and address from wg0.
func (w *Wire) Identity() (WireIdentity, error) {
	key, err := w.run("show", "wg0", "public-key")

	if err != nil {
		return WireIdentity{}, fmt.Errorf("wg0 is not up: %w", err)
	}

	id := WireIdentity{PublicKey: strings.TrimSpace(string(key))}

	if port, err := w.run("show", "wg0", "listen-port"); err == nil {
		id.ListenPort, _ = strconv.Atoi(strings.TrimSpace(string(port)))
	}

	if addr := w.local(); addr.IsValid() {
		id.Address = addr.String()
	}

	outbound := netip.Addr{}

	if w.OutboundIP != nil {
		outbound = w.OutboundIP()
	} else {
		outbound = outboundIP()
	}

	if outbound.IsValid() && id.ListenPort > 0 {
		id.Endpoint = netip.AddrPortFrom(outbound, uint16(id.ListenPort)).String()
	}

	return id, nil
}

// outboundIP is the source address the kernel picks for the internet —
// the same reading the install uses for the host's identity.
func outboundIP() netip.Addr {
	conn, err := net.Dial("udp4", "1.1.1.1:53")

	if err != nil {
		return netip.Addr{}
	}

	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}
	}

	ip, _ := netip.AddrFromSlice(addr.IP.To4())

	return ip
}

// UFWRules is what a host firewall running ufw has to let through for the
// mesh to work, as ufw arguments: the tunnel, the other hosts asking this
// one for names, this host's containers asking their controller, and the
// other hosts reaching the containers. Each is one `ufw <args>`; the same
// args after `delete` (or `route delete`) undo it.
func UFWRules(bridge string, listenPort int) [][]string {
	if listenPort <= 0 {
		listenPort = 51820
	}

	return [][]string{
		{"allow", fmt.Sprintf("%d/udp", listenPort)},
		{"allow", "in", "on", "wg0", "to", "any", "port", "53"},
		{"allow", "in", "on", bridge, "to", "any", "port", "53"},
		{"route", "allow", "in", "on", "wg0", "out", "on", bridge},
	}
}

// ErrWireNotRouted is a firewall question on a host whose voodu0 is local:
// there is no bridge to name and nothing to let through.
var ErrWireNotRouted = errors.New("voodu0 is not routed over wg0 on this host")

// UFW returns the rules for this host.
func (w *Wire) UFW() (bridge string, rules [][]string, err error) {
	if w.Bridge != nil {
		bridge = w.Bridge()
	}

	if bridge == "" {
		return "", nil, ErrWireNotRouted
	}

	port := 0

	if out, err := w.run("show", "wg0", "listen-port"); err == nil {
		port, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	}

	return bridge, UFWRules(bridge, port), nil
}
