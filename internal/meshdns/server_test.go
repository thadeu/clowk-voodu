package meshdns

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

var (
	containerSrc = netip.MustParseAddr("10.91.221.9")    // a container on this host
	peerSrc      = netip.MustParseAddr("10.254.167.105") // another host
	strangerSrc  = netip.MustParseAddr("203.0.113.9")    // anyone else
)

// index is a host's own names, counting lookups so a test can tell whether
// a question reached it.
type index struct {
	names map[string][]netip.Addr
	calls atomic.Int32
}

func (x *index) Lookup(name string) []netip.Addr {
	x.calls.Add(1)

	return x.names[name]
}

type peerList []netip.AddrPort

func (p peerList) List() []netip.AddrPort { return p }

func addrs(ss ...string) []netip.Addr {
	var out []netip.Addr
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}

	return out
}

// host returns the server under test: this host's voodu0 is 10.91.221.0/24.
func host(names map[string][]netip.Addr, peers ...netip.AddrPort) (*Server, *index) {
	x := &index{names: names}

	return &Server{
		Local:  netip.MustParsePrefix("10.91.221.0/24"),
		Tunnel: Tunnel,
		Index:  x,
		Peers:  peerList(peers),
	}, x
}

// startPeer runs another host's mesh DNS on loopback and returns where it
// listens. Its tunnel is loopback, so the question from the host under
// test counts as a peer's.
func startPeer(t *testing.T, names map[string][]netip.Addr) (netip.AddrPort, *index) {
	t.Helper()

	x := &index{names: names}
	s := &Server{Local: netip.MustParsePrefix("192.0.2.0/24"), Tunnel: netip.MustParsePrefix("127.0.0.0/8"), Index: x, Peers: peerList(nil)}

	return serve(t, s), x
}

// serve runs s on a loopback port shared by UDP and TCP.
func serve(t *testing.T, s *Server) netip.AddrPort {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	for range 20 {
		pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}

		addr := pc.LocalAddr().(*net.UDPAddr).AddrPort()

		ln, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(addr))
		if err != nil {
			pc.Close()

			continue
		}

		go func() { _ = s.serveOn(ctx, pc, ln) }()

		return addr
	}

	t.Fatal("no loopback port free for both UDP and TCP")

	return netip.AddrPort{}
}

func question(t *testing.T, name string, qtype dnsmessage.Type) []byte {
	t.Helper()

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 4242, RecursionDesired: true})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: qtype, Class: dnsmessage.ClassINET})

	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}

	return msg
}

type answer struct {
	rcode     dnsmessage.RCode
	addrs     []netip.Addr
	truncated bool
	authority bool
}

func read(t *testing.T, resp []byte) answer {
	t.Helper()

	var p dnsmessage.Parser

	hdr, err := p.Start(resp)
	if err != nil {
		t.Fatalf("unparseable response: %v", err)
	}

	got, err := answersA(resp)
	if err != nil {
		t.Fatal(err)
	}

	slices.SortFunc(got, func(a, b netip.Addr) int { return a.Compare(b) })

	return answer{rcode: hdr.RCode, addrs: got, truncated: hdr.Truncated, authority: hdr.Authoritative}
}

func TestAContainerGetsThisHostsNamesFromTheIndex(t *testing.T) {
	s, _ := host(map[string][]netip.Addr{"pg-0.contagorda.voodu.": addrs("10.91.221.2")})

	got := read(t, s.Handle(context.Background(), containerSrc, question(t, "pg-0.contagorda.voodu.", dnsmessage.TypeA), false))

	if got.rcode != dnsmessage.RCodeSuccess || !slices.Equal(got.addrs, addrs("10.91.221.2")) || !got.authority {
		t.Fatalf("got %+v, want an authoritative 10.91.221.2", got)
	}
}

// The AAAA asked alongside the A must not deny the name.
func TestAAAAOnAnExistingNameIsNoDataNotNXDOMAIN(t *testing.T) {
	s, _ := host(map[string][]netip.Addr{"pg-0.contagorda.voodu.": addrs("10.91.221.2")})

	got := read(t, s.Handle(context.Background(), containerSrc, question(t, "pg-0.contagorda.voodu.", dnsmessage.TypeAAAA), false))

	if got.rcode != dnsmessage.RCodeSuccess || len(got.addrs) != 0 {
		t.Fatalf("got %+v, want NOERROR with no data", got)
	}
}

// The heart of it: a name on another host comes back from its owner, and
// the same name on two hosts comes back with both — logged, since two
// environments sharing a scope look exactly like this.
func TestARemoteNameIsAskedOfThePeersAndMerged(t *testing.T) {
	vm2, _ := startPeer(t, map[string][]netip.Addr{"cache.contagorda.voodu.": addrs("10.167.105.3")})
	vm3, _ := startPeer(t, map[string][]netip.Addr{"cache.contagorda.voodu.": addrs("10.12.34.3")})
	vm4, _ := startPeer(t, nil)

	var (
		mu   sync.Mutex
		logs []string
	)

	s, _ := host(nil, vm2, vm3, vm4)
	s.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()

		logs = append(logs, format)
	}

	got := read(t, s.Handle(context.Background(), containerSrc, question(t, "cache.contagorda.voodu.", dnsmessage.TypeA), false))

	if !slices.Equal(got.addrs, addrs("10.12.34.3", "10.167.105.3")) {
		t.Fatalf("got %v, want both hosts' answers", got.addrs)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(logs) != 1 || !strings.Contains(logs[0], "answered by") {
		t.Errorf("two hosts answering must be logged, got %q", logs)
	}
}

// A typo or a stopped service must not send a round to every peer on every
// lookup.
func TestANameNobodyHasIsNXDOMAINAndBrieflyCached(t *testing.T) {
	peer, peerIndex := startPeer(t, nil)

	clock := time.Now()
	s, _ := host(nil, peer)
	s.now = func() time.Time { return clock }

	ask := func() answer {
		return read(t, s.Handle(context.Background(), containerSrc, question(t, "typo.contagorda.voodu.", dnsmessage.TypeA), false))
	}

	if got := ask(); got.rcode != dnsmessage.RCodeNameError {
		t.Fatalf("got %+v, want NXDOMAIN", got)
	}

	ask()

	if calls := peerIndex.calls.Load(); calls != 1 {
		t.Fatalf("peer was asked %d times within the negative cache, want 1", calls)
	}

	clock = clock.Add(negativeTTL + time.Millisecond)
	ask()

	if calls := peerIndex.calls.Load(); calls != 2 {
		t.Fatalf("peer was asked %d times after the cache expired, want 2", calls)
	}
}

// A peer gets this host's names and nothing more — so a question never
// travels in a loop between hosts.
func TestAPeersQuestionIsAnsweredOnlyFromThisHost(t *testing.T) {
	other, otherIndex := startPeer(t, map[string][]netip.Addr{"cache.contagorda.voodu.": addrs("10.12.34.3")})
	s, _ := host(nil, other)

	got := read(t, s.Handle(context.Background(), peerSrc, question(t, "cache.contagorda.voodu.", dnsmessage.TypeA), false))

	if got.rcode != dnsmessage.RCodeNameError {
		t.Fatalf("got %+v, want NXDOMAIN — a peer's question must not be passed on", got)
	}

	if otherIndex.calls.Load() != 0 {
		t.Fatal("the peer's question reached another host")
	}
}

func TestAnyoneElseIsRefused(t *testing.T) {
	s, _ := host(map[string][]netip.Addr{"pg-0.contagorda.voodu.": addrs("10.91.221.2")})

	for _, name := range []string{"pg-0.contagorda.voodu.", "example.com."} {
		if got := read(t, s.Handle(context.Background(), strangerSrc, question(t, name, dnsmessage.TypeA), false)); got.rcode != dnsmessage.RCodeRefused {
			t.Errorf("%s from a stranger: %+v, want REFUSED", name, got)
		}
	}
}

// fakeResolver stands in for the host's resolver: it answers every A
// question with ip.
func fakeResolver(t *testing.T, ip string) netip.AddrPort {
	t.Helper()

	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { pc.Close() })

	go func() {
		buf := make([]byte, 1500)

		for {
			n, from, err := pc.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}

			var p dnsmessage.Parser

			hdr, err := p.Start(buf[:n])
			if err != nil {
				continue
			}

			q, err := p.Question()
			if err != nil {
				continue
			}

			_, _ = pc.WriteToUDPAddrPort(build(hdr, &q, dnsmessage.RCodeSuccess, addrs(ip), false), from)
		}
	}()

	return pc.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Everything outside the mesh goes to the host's resolver and comes back as
// it answered — but only for this host's containers.
func TestInternetNamesGoToTheHostResolverOnlyForContainers(t *testing.T) {
	s, _ := host(nil)
	s.Upstreams = []netip.AddrPort{fakeResolver(t, "93.184.216.34")}

	got := read(t, s.Handle(context.Background(), containerSrc, question(t, "example.com.", dnsmessage.TypeA), false))

	if got.rcode != dnsmessage.RCodeSuccess || !slices.Equal(got.addrs, addrs("93.184.216.34")) {
		t.Fatalf("got %+v, want the host resolver's answer", got)
	}

	if refused := read(t, s.Handle(context.Background(), peerSrc, question(t, "example.com.", dnsmessage.TypeA), false)); refused.rcode != dnsmessage.RCodeRefused {
		t.Fatalf("a peer asked for an internet name: %+v, want REFUSED", refused)
	}
}

func TestNoReachableHostResolverIsServfail(t *testing.T) {
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	dead := pc.LocalAddr().(*net.UDPAddr).AddrPort()
	pc.Close()

	s, _ := host(nil)
	s.Upstreams = []netip.AddrPort{dead}
	s.UpstreamTimeout = 200 * time.Millisecond

	if got := read(t, s.Handle(context.Background(), containerSrc, question(t, "example.com.", dnsmessage.TypeA), false)); got.rcode != dnsmessage.RCodeServerFailure {
		t.Fatalf("got %+v, want SERVFAIL", got)
	}
}

// Forty replicas do not fit a classic UDP answer: UDP says so, TCP carries
// them all, and a peer round retries over TCP on its own.
func TestALargeAnswerTruncatesOverUDPAndCompletesOverTCP(t *testing.T) {
	var many []netip.Addr
	for i := range 40 {
		many = append(many, netip.AddrFrom4([4]byte{10, 91, 221, byte(128 + i)}))
	}

	names := map[string][]netip.Addr{"api.clowk.voodu.": many}
	s, _ := host(names)

	udp := read(t, s.Handle(context.Background(), containerSrc, question(t, "api.clowk.voodu.", dnsmessage.TypeA), false))
	if !udp.truncated || len(udp.addrs) != 0 {
		t.Fatalf("UDP: %+v, want truncated and empty", udp)
	}

	if tcp := read(t, s.Handle(context.Background(), containerSrc, question(t, "api.clowk.voodu.", dnsmessage.TypeA), true)); len(tcp.addrs) != 40 {
		t.Fatalf("TCP carried %d addresses, want 40", len(tcp.addrs))
	}

	peer, _ := startPeer(t, names)
	remote, _ := host(nil, peer)

	if got := read(t, remote.Handle(context.Background(), containerSrc, question(t, "api.clowk.voodu.", dnsmessage.TypeA), true)); len(got.addrs) != 40 {
		t.Fatalf("the peer round returned %d addresses, want 40 (retry over TCP)", len(got.addrs))
	}
}

func TestParseAllowedIPsKeepsEachPeersTunnelAddress(t *testing.T) {
	out := []byte("xTIB=\t10.254.167.105/32 10.167.105.0/24\n" +
		"aB3k=\t10.254.12.34/32 10.12.34.0/24\n" +
		"legacy=\t10.8.0.1/32\n" +
		"none=\t(none)\n")

	got := ParseAllowedIPs(out, Tunnel)

	if !slices.Equal(got, addrs("10.254.167.105", "10.254.12.34")) {
		t.Fatalf("got %v", got)
	}
}

func TestParseResolvConfDropsLoopbackStubs(t *testing.T) {
	data := []byte("# managed by systemd\nnameserver 127.0.0.53\nnameserver ::1\nnameserver 185.12.64.1\noptions edns0\nnameserver 185.12.64.2\n")

	var got []string
	for _, up := range ParseResolvConf(data) {
		got = append(got, up.String())
	}

	if want := []string{"185.12.64.1:53", "185.12.64.2:53"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A failed read of wg0 keeps the last good list: a transient error must not
// make every remote name vanish.
func TestWGPeersKeepTheLastGoodList(t *testing.T) {
	fail := false

	w := &WGPeers{Port: 53, Run: func() ([]byte, error) {
		if fail {
			return nil, net.ErrClosed
		}

		return []byte("k=\t10.254.1.2/32 10.1.2.0/24\n"), nil
	}}

	if got := w.List(); len(got) != 1 || got[0].String() != "10.254.1.2:53" {
		t.Fatalf("got %v", got)
	}

	fail = true
	w.at = time.Time{}

	if got := w.List(); len(got) != 1 {
		t.Fatalf("a failed read dropped the peers: %v", got)
	}
}

// Resolve is the controller's own way in: the local index, then the peers,
// with or without the trailing dot; anything outside .voodu is nobody's.
func TestResolveLooksLocallyThenAtPeers(t *testing.T) {
	peer, _ := startPeer(t, map[string][]netip.Addr{"api.clowk.voodu.": addrs("10.167.105.130")})
	s, _ := host(map[string][]netip.Addr{"pg-0.data.voodu.": addrs("10.91.221.2")}, peer)

	if got := s.Resolve(context.Background(), "pg-0.data.voodu"); len(got) != 1 || got[0] != netip.MustParseAddr("10.91.221.2") {
		t.Fatalf("local = %v", got)
	}

	if got := s.Resolve(context.Background(), "API.clowk.voodu."); len(got) != 1 || got[0] != netip.MustParseAddr("10.167.105.130") {
		t.Fatalf("remote = %v", got)
	}

	if got := s.Resolve(context.Background(), "example.com"); got != nil {
		t.Fatalf("an internet name resolved through the mesh: %v", got)
	}
}

// The AAAA a resolver sends alongside the A can carry no mesh answer, so it
// must not cost a round of the peers.
func TestAAAAOfARemoteNameDoesNotAskThePeers(t *testing.T) {
	peer, peerIndex := startPeer(t, map[string][]netip.Addr{"api.clowk.voodu.": addrs("10.167.105.130")})
	s, _ := host(nil, peer)

	got := read(t, s.Handle(context.Background(), containerSrc, question(t, "api.clowk.voodu.", dnsmessage.TypeAAAA), false))
	if got.rcode != dnsmessage.RCodeNameError {
		t.Fatalf("AAAA before any A: %+v, want NXDOMAIN without asking", got)
	}

	if calls := peerIndex.calls.Load(); calls != 0 {
		t.Fatalf("peer was asked %d times for an AAAA, want 0", calls)
	}

	if got := read(t, s.Handle(context.Background(), containerSrc, question(t, "api.clowk.voodu.", dnsmessage.TypeA), false)); got.rcode != dnsmessage.RCodeSuccess {
		t.Fatalf("A: %+v", got)
	}

	if calls := peerIndex.calls.Load(); calls != 1 {
		t.Fatalf("peer was asked %d times for the A, want 1", calls)
	}
}

// Names asked once and never again leave the cache when they expire.
func TestExpiredCacheEntriesAreEvicted(t *testing.T) {
	peer, _ := startPeer(t, nil)

	clock := time.Now()
	s, _ := host(nil, peer)
	s.now = func() time.Time { return clock }

	for i := range 50 {
		s.Handle(context.Background(), containerSrc, question(t, fmt.Sprintf("typo%d.contagorda.voodu.", i), dnsmessage.TypeA), false)
	}

	clock = clock.Add(negativeTTL + time.Millisecond)
	s.Handle(context.Background(), containerSrc, question(t, "another.contagorda.voodu.", dnsmessage.TypeA), false)

	s.mu.Lock()
	n := len(s.cache)
	s.mu.Unlock()

	if n != 1 {
		t.Fatalf("cache holds %d entries after they all expired, want 1", n)
	}
}
