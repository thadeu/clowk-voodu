package controller

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"go.voodu.clowk.in/internal/meshdns"
)

type podsFrom struct {
	pods  []Pod
	err   error
	calls int
}

func (p *podsFrom) ListPods() ([]Pod, error) {
	p.calls++

	return p.pods, p.err
}

func sortedAddrs(in []netip.Addr) []string {
	var out []string
	for _, a := range in {
		out = append(out, a.String())
	}

	slices.Sort(out)

	return out
}

// The names another host can ask for are exactly the ones docker answers
// here: the round-robin name to every running replica, a statefulset pod's
// own name to that pod.
func TestMeshNamesAreTheLocalAliases(t *testing.T) {
	names := meshNames([]Pod{
		{Kind: "deployment", Scope: "clowk", ResourceName: "api", ReplicaID: "a1b2", Running: true, IP: "10.91.221.130"},
		{Kind: "deployment", Scope: "clowk", ResourceName: "api", ReplicaID: "c3d4", Running: true, IP: "10.91.221.131"},
		{Kind: "deployment", Scope: "clowk", ResourceName: "api", ReplicaID: "e5f6", Running: false, IP: "10.91.221.132"},
		{Kind: "statefulset", Scope: "contagorda", ResourceName: "pg", ReplicaID: "0", Running: true, IP: "10.91.221.2"},
		{Kind: "statefulset", Scope: "contagorda", ResourceName: "pg", ReplicaID: "1", Running: true, IP: "10.91.221.3"},
		{Kind: "job", Scope: "clowk", ResourceName: "migrate", Running: true, IP: "10.91.221.140"},
		{Kind: "deployment", Scope: "clowk", ResourceName: "hostnet", Running: true},
	})

	for name, want := range map[string][]string{
		"api.clowk.voodu.":       {"10.91.221.130", "10.91.221.131"},
		"pg.contagorda.voodu.":   {"10.91.221.2", "10.91.221.3"},
		"pg-0.contagorda.voodu.": {"10.91.221.2"},
		"pg-1.contagorda.voodu.": {"10.91.221.3"},
	} {
		if got := sortedAddrs(names[name]); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	for _, absent := range []string{"migrate.clowk.voodu.", "hostnet.clowk.voodu.", "pg.contagorda."} {
		if _, ok := names[absent]; ok {
			t.Errorf("%s must not be a mesh name", absent)
		}
	}
}

// Read from docker only when asked and at most every refresh; a failed read
// keeps the last names rather than making this host's services vanish.
func TestMeshIndexRefreshesLazilyAndSurvivesADockerHiccup(t *testing.T) {
	pods := &podsFrom{pods: []Pod{{Kind: "statefulset", Scope: "data", ResourceName: "pg", ReplicaID: "0", Running: true, IP: "10.91.221.2"}}}

	clock := time.Now()
	x := &meshIndex{Pods: pods, now: func() time.Time { return clock }}

	x.Lookup("pg-0.data.voodu.")
	x.Lookup("pg-0.data.voodu.")

	if pods.calls != 1 {
		t.Fatalf("docker read %d times within the refresh window, want 1", pods.calls)
	}

	clock = clock.Add(meshIndexRefresh)
	pods.err = errors.New("daemon hiccup")

	if got := x.Lookup("pg-0.data.voodu."); len(got) != 1 {
		t.Fatalf("a failed read dropped the names: %v", got)
	}

	if pods.calls != 2 {
		t.Fatalf("docker read %d times after the window, want 2", pods.calls)
	}
}

// The mesh DNS first, the host's after: docker's embedded DNS stops at the
// first NXDOMAIN.
func TestContainerDNSPutsTheMeshFirst(t *testing.T) {
	m := &meshNetwork{
		gateway:   netip.MustParseAddr("10.91.221.1"),
		upstreams: []netip.AddrPort{netip.MustParseAddrPort("185.12.64.1:53"), netip.MustParseAddrPort("185.12.64.2:53")},
	}

	if got, want := m.containerDNS(), []string{"10.91.221.1", "185.12.64.1", "185.12.64.2"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	var local *meshNetwork

	if got := local.containerDNS(); got != nil {
		t.Fatalf("a host without a routed voodu0 must keep docker's default, got %v", got)
	}
}

// Every kind is created through containerConfig, so the resolvers reach all
// of them without any handler remembering to pass them.
func TestContainerConfigCarriesTheMeshDNS(t *testing.T) {
	spec := ContainerSpec{Name: "clowk-migrate.release", Image: "app:1", Networks: []string{"voodu0"}}

	meshed := DockerContainerManager{DNS: func() []string { return []string{"10.91.221.1", "185.12.64.1"} }}

	if got := meshed.containerConfig(spec).DNS; !slices.Equal(got, []string{"10.91.221.1", "185.12.64.1"}) {
		t.Fatalf("DNS = %v", got)
	}

	if got := (DockerContainerManager{}).containerConfig(spec).DNS; got != nil {
		t.Fatalf("no mesh must mean docker's default resolvers, got %v", got)
	}
}

// wg0 may get its address after the controller starts: the tunnel listener
// waits for it instead of giving up.
func TestMeshServeWaitsForALateWG0(t *testing.T) {
	port := freeUDPPort(t)

	old := meshDNSPort
	meshDNSPort = port
	t.Cleanup(func() { meshDNSPort = old })

	var mu sync.Mutex
	up := false

	m := &meshNetwork{
		subnet:  netip.MustParsePrefix("127.0.0.0/8"),
		gateway: netip.MustParseAddr("127.0.0.1"),
		readTunnel: func() netip.Addr {
			mu.Lock()
			defer mu.Unlock()

			if up {
				return netip.MustParseAddr("127.0.0.1")
			}

			return netip.Addr{}
		},
	}

	if m.tunnelAddress().IsValid() {
		t.Fatal("no wg0 yet, but an address came back")
	}

	mu.Lock()
	up = true
	mu.Unlock()

	if got := m.tunnelAddress(); got != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("wg0 came up, tunnelAddress = %v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := &meshdns.Server{
		Local:  m.subnet,
		Tunnel: netip.MustParsePrefix("10.254.0.0/16"),
		Index:  &meshIndex{Pods: &podsFrom{pods: []Pod{{Kind: "statefulset", Scope: "data", ResourceName: "pg", ReplicaID: "0", Running: true, IP: "10.91.221.2"}}}},
		Peers:  &meshdns.WGPeers{Run: func() ([]byte, error) { return nil, nil }},
	}

	go m.serveOn(ctx, srv, m.tunnelAddress, "wg0", t.Logf)

	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 7, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("pg-0.data.voodu."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}

	query, _ := msg.Pack()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		conn, err := net.Dial("udp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port).String())
		if err != nil {
			t.Fatal(err)
		}

		conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
		conn.Write(query)

		buf := make([]byte, 512)

		n, err := conn.Read(buf)
		conn.Close()

		if err != nil {
			time.Sleep(50 * time.Millisecond)

			continue
		}

		var reply dnsmessage.Message
		if err := reply.Unpack(buf[:n]); err != nil {
			t.Fatal(err)
		}

		if len(reply.Answers) != 1 {
			t.Fatalf("answers = %v", reply.Answers)
		}

		return
	}

	t.Fatal("the listener never came up on the late tunnel address")
}

func freeUDPPort(t *testing.T) uint16 {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Close()

	return uint16(conn.LocalAddr().(*net.UDPAddr).Port)
}
