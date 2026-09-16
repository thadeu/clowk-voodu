package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKey(seed byte) string {
	b := make([]byte, 32)

	for i := range b {
		b[i] = seed
	}

	return base64.StdEncoding.EncodeToString(b)
}

// fakeWG records every wg invocation and answers show commands.
type fakeWG struct {
	calls [][]string
	fail  bool

	// dump, when set, is what `wg show wg0 dump` answers. Otherwise the
	// fake tracks the peers addconf loaded and set removed, and renders
	// them — so Apply's removal pass is exercised for real.
	dump  string
	peers map[string]bool
}

func (f *fakeWG) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)

	if f.fail {
		return nil, errors.New("wg: not up")
	}

	if f.peers == nil {
		f.peers = map[string]bool{}
	}

	switch {
	case strings.Join(args, " ") == "show wg0 public-key":
		return []byte(testKey(1) + "\n"), nil
	case strings.Join(args, " ") == "show wg0 listen-port":
		return []byte("51820\n"), nil
	case strings.Join(args, " ") == "show wg0 dump":
		if f.dump != "" {
			return []byte(f.dump), nil
		}

		out := "privkey\t" + testKey(1) + "\t51820\toff\n"

		for key := range f.peers {
			out += key + "\t(none)\t(none)\t0.0.0.0/0\t0\t0\t0\t25\n"
		}

		return []byte(out), nil
	case len(args) == 3 && args[0] == "addconf":
		data, err := os.ReadFile(args[2])
		if err != nil {
			return nil, err
		}

		for _, line := range strings.Split(string(data), "\n") {
			if key, ok := strings.CutPrefix(line, "PublicKey = "); ok {
				f.peers[key] = true
			}
		}

		return nil, nil
	case len(args) == 5 && args[0] == "set" && args[2] == "peer" && args[4] == "remove":
		delete(f.peers, args[3])

		return nil, nil
	}

	return nil, nil
}

// applies counts the addconf runs — one per Apply.
func (f *fakeWG) applies() int {
	n := 0

	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "addconf" {
			n++
		}
	}

	return n
}

func (f *fakeWG) removed() []string {
	var out []string

	for _, c := range f.calls {
		if len(c) == 5 && c[0] == "set" && c[4] == "remove" {
			out = append(out, c[3])
		}
	}

	return out
}

// fakeIP tracks the routes `ip route replace/del ... dev wg0` leave on the
// interface, and answers `ip route show`.
type fakeIP struct {
	routes map[string]bool
	calls  []string
}

func (f *fakeIP) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))

	if f.routes == nil {
		f.routes = map[string]bool{}
	}

	switch {
	case len(args) >= 4 && args[1] == "route" && args[2] == "replace":
		f.routes[args[3]] = true
	case len(args) >= 4 && args[1] == "route" && args[2] == "del":
		delete(f.routes, args[3])
	case len(args) >= 3 && args[1] == "route" && args[2] == "show":
		out := "10.254.0.0/16 proto kernel scope link src 10.254.91.221\n"

		for r := range f.routes {
			out += r + " scope link\n"
		}

		return []byte(out), nil
	}

	return nil, nil
}

func newTestWire(t *testing.T) (*Wire, *memStore, *fakeWG) {
	t.Helper()

	store := newMemStore()
	wg := &fakeWG{}

	w := &Wire{
		Store:        store,
		ConfPath:     filepath.Join(t.TempDir(), "wire", "peers.conf"),
		Run:          wg.run,
		RunIP:        (&fakeIP{}).run,
		LocalAddress: func() netip.Addr { return netip.MustParseAddr("10.254.91.221") },
		OutboundIP:   func() netip.Addr { return netip.MustParseAddr("152.53.91.221") },
	}

	return w, store, wg
}

func TestWirePeersConf_RendersNativeFormat(t *testing.T) {
	got := string(WirePeersConf([]WirePeer{
		{PublicKey: testKey(3), Address: "10.254.200.7"},
		{PublicKey: testKey(2), Address: "10.254.167.105", Endpoint: "152.53.167.105:51820"},
	}))

	want := "# managed by voodu — edit with `vd wire`, not by hand\n" +
		"\n[Peer]\nPublicKey = " + testKey(2) + "\nAllowedIPs = 10.254.167.105/32, 10.167.105.0/24\nPersistentKeepalive = 25\nEndpoint = 152.53.167.105:51820\n" +
		"\n[Peer]\nPublicKey = " + testKey(3) + "\nAllowedIPs = 10.254.200.7/32, 10.200.7.0/24\nPersistentKeepalive = 25\n"

	if got != want {
		t.Fatalf("conf:\n%s\nwant:\n%s", got, want)
	}
}

func TestWirePeer_Validate(t *testing.T) {
	local := netip.MustParseAddr("10.254.91.221")

	cases := []struct {
		name string
		p    WirePeer
		bad  string
	}{
		{"ok", WirePeer{PublicKey: testKey(2), Address: "10.254.167.105", Endpoint: "1.2.3.4:51820"}, ""},
		{"ok without endpoint", WirePeer{PublicKey: testKey(2), Address: "10.254.167.105"}, ""},
		{"key not base64", WirePeer{PublicKey: "nope", Address: "10.254.167.105"}, "public key"},
		{"key wrong length", WirePeer{PublicKey: base64.StdEncoding.EncodeToString([]byte("short")), Address: "10.254.167.105"}, "public key"},
		{"address outside tunnel", WirePeer{PublicKey: testKey(2), Address: "10.8.0.1"}, "not in the voodu tunnel"},
		{"address is this host", WirePeer{PublicKey: testKey(2), Address: "10.254.91.221"}, "that is this host"},
		{"address not ip", WirePeer{PublicKey: testKey(2), Address: "vm-2"}, "not an IPv4"},
		{"endpoint without port", WirePeer{PublicKey: testKey(2), Address: "10.254.167.105", Endpoint: "1.2.3.4"}, "host:port"},
		{"endpoint bad port", WirePeer{PublicKey: testKey(2), Address: "10.254.167.105", Endpoint: "1.2.3.4:99999"}, "bad port"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.p.Validate(local)

			if c.bad == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), c.bad) {
				t.Fatalf("error %v, want containing %q", err, c.bad)
			}
		})
	}
}

func TestParseWGDump(t *testing.T) {
	dump := "privkey\t" + testKey(1) + "\t51820\toff\n" +
		testKey(2) + "\t(none)\t152.53.167.105:51820\t10.254.167.105/32,10.167.105.0/24\t1757700000\t1234\t5678\t25\n" +
		testKey(3) + "\t(none)\t(none)\t10.254.200.7/32\t0\t0\t0\t25\n"

	links := ParseWGDump([]byte(dump))

	if len(links) != 2 {
		t.Fatalf("links = %d, want 2 (the interface line is not a peer)", len(links))
	}

	l := links[testKey(2)]
	if l.Endpoint != "152.53.167.105:51820" || l.RxBytes != 1234 || l.TxBytes != 5678 || !l.LastHandshake.Equal(time.Unix(1757700000, 0)) {
		t.Fatalf("link = %+v", l)
	}

	l = links[testKey(3)]
	if l.Endpoint != "" || !l.LastHandshake.IsZero() {
		t.Fatalf("never-seen peer = %+v, want no endpoint and zero handshake", l)
	}
}

func TestWire_AddWritesFileAndSyncs(t *testing.T) {
	w, store, wg := newTestWire(t)

	added, err := w.Add(context.Background(), WirePeer{PublicKey: " " + testKey(2) + "\n", Address: "10.254.167.105", Endpoint: "152.53.167.105:51820"})
	if err != nil {
		t.Fatal(err)
	}

	if added.PublicKey != testKey(2) || added.AddedAt.IsZero() {
		t.Fatalf("added = %+v: key not trimmed or AddedAt unset", added)
	}

	peers, _ := store.ListWirePeers(context.Background())
	if len(peers) != 1 {
		t.Fatalf("stored %d peers, want 1", len(peers))
	}

	data, err := os.ReadFile(w.ConfPath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(data), "AllowedIPs = 10.254.167.105/32, 10.167.105.0/24") {
		t.Fatalf("file:\n%s", data)
	}

	info, _ := os.Stat(w.ConfPath)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}

	if wg.applies() != 1 || len(wg.removed()) != 0 {
		t.Fatalf("wg calls = %v, want one addconf on the file and no removal", wg.calls)
	}

	for _, c := range wg.calls {
		if c[0] == "syncconf" {
			t.Fatalf("syncconf resets the listen port — never run it: %v", wg.calls)
		}
	}
}

func TestWire_AddInvalidStoresNothing(t *testing.T) {
	w, store, wg := newTestWire(t)

	if _, err := w.Add(context.Background(), WirePeer{PublicKey: "bad", Address: "10.254.167.105"}); err == nil {
		t.Fatal("want validation error")
	}

	peers, _ := store.ListWirePeers(context.Background())
	if len(peers) != 0 || wg.applies() != 0 {
		t.Fatalf("invalid peer reached the store (%d) or wg0 (%d applies)", len(peers), wg.applies())
	}
}

func TestWire_AddSameAddressReplaces(t *testing.T) {
	w, store, wg := newTestWire(t)
	ctx := context.Background()

	if _, err := w.Add(ctx, WirePeer{PublicKey: testKey(2), Address: "10.254.167.105"}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Add(ctx, WirePeer{PublicKey: testKey(4), Address: "10.254.167.105", Endpoint: "1.2.3.4:51820"}); err != nil {
		t.Fatal(err)
	}

	peers, _ := store.ListWirePeers(ctx)
	if len(peers) != 1 || peers[0].PublicKey != testKey(4) {
		t.Fatalf("peers = %+v, want the second key only", peers)
	}

	if wg.peers[testKey(2)] || !wg.peers[testKey(4)] {
		t.Fatalf("wg0 peers = %v, want only the new key", wg.peers)
	}
}

func TestWire_RemoveSyncsAndErrsOnUnknown(t *testing.T) {
	w, _, wg := newTestWire(t)
	ctx := context.Background()

	if _, err := w.Add(ctx, WirePeer{PublicKey: testKey(2), Address: "10.254.167.105"}); err != nil {
		t.Fatal(err)
	}

	if err := w.Remove(ctx, "10.254.167.105"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(w.ConfPath)
	if strings.Contains(string(data), "[Peer]") {
		t.Fatalf("file still has a peer:\n%s", data)
	}

	if wg.applies() != 2 {
		t.Fatalf("applies = %d, want 2 (add + remove)", wg.applies())
	}

	// addconf cannot drop a peer: the removal is an explicit wg set.
	if got := wg.removed(); len(got) != 1 || got[0] != testKey(2) {
		t.Fatalf("removed = %v, want the peer's key", got)
	}

	if wg.peers[testKey(2)] {
		t.Fatal("peer still on wg0 after remove")
	}

	err := w.Remove(ctx, "10.254.9.9")
	if !errors.Is(err, ErrWirePeerNotFound) {
		t.Fatalf("remove unknown: %v, want ErrWirePeerNotFound", err)
	}
}

func TestWire_ApplyFailsWhenWGFails(t *testing.T) {
	w, _, wg := newTestWire(t)
	wg.fail = true

	_, err := w.Add(context.Background(), WirePeer{PublicKey: testKey(2), Address: "10.254.167.105"})
	if err == nil || !strings.Contains(err.Error(), "not up") {
		t.Fatalf("err = %v, want wg failure surfaced", err)
	}
}

func TestWire_StatusJoinsStoreAndDump(t *testing.T) {
	w, _, wg := newTestWire(t)
	ctx := context.Background()

	for _, p := range []WirePeer{
		{PublicKey: testKey(2), Address: "10.254.167.105", Endpoint: "152.53.167.105:51820"},
		{PublicKey: testKey(3), Address: "10.254.200.7"},
	} {
		if _, err := w.Add(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	// wg0 carries the first peer only: the second is stored but not applied.
	wg.dump = "privkey\t" + testKey(1) + "\t51820\toff\n" +
		testKey(2) + "\t(none)\t152.53.167.105:51820\t10.254.167.105/32\t1757700000\t10\t20\t25\n"

	id, peers, err := w.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if id.PublicKey != testKey(1) || id.Address != "10.254.91.221" || id.ListenPort != 51820 || id.Endpoint != "152.53.91.221:51820" {
		t.Fatalf("identity = %+v", id)
	}

	if len(peers) != 2 || peers[0].Address != "10.254.167.105" {
		t.Fatalf("peers = %+v, want 2 sorted by address", peers)
	}

	if !peers[0].Applied || peers[0].Link == nil || peers[0].Link.RxBytes != 10 {
		t.Fatalf("applied peer = %+v", peers[0])
	}

	if peers[1].Applied || peers[1].Link != nil {
		t.Fatalf("peer missing from wg0 reported as applied: %+v", peers[1])
	}
}

func TestWire_StatusWithoutWG0(t *testing.T) {
	w, _, wg := newTestWire(t)
	wg.fail = true

	if _, _, err := w.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "wg0 is not up") {
		t.Fatalf("err = %v", err)
	}
}

func TestWire_RejectsAKeyAlreadyUsedByAnotherPeer(t *testing.T) {
	w, store, _ := newTestWire(t)
	ctx := context.Background()

	if _, err := w.Add(ctx, WirePeer{PublicKey: testKey(2), Address: "10.254.167.105"}); err != nil {
		t.Fatal(err)
	}

	_, err := w.Add(ctx, WirePeer{PublicKey: testKey(2), Address: "10.254.200.7"})
	if err == nil || !strings.Contains(err.Error(), "already used by peer 10.254.167.105") {
		t.Fatalf("err = %v", err)
	}

	peers, _ := store.ListWirePeers(ctx)
	if len(peers) != 1 {
		t.Fatalf("the duplicate reached the store: %+v", peers)
	}

	// The same address with the same key is a re-add, not a duplicate.
	if _, err := w.Add(ctx, WirePeer{PublicKey: testKey(2), Address: "10.254.167.105", Endpoint: "1.2.3.4:51820"}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
}

func TestWire_WGFailureIsUnavailable(t *testing.T) {
	w, _, wg := newTestWire(t)
	wg.fail = true

	_, err := w.Add(context.Background(), WirePeer{PublicKey: testKey(2), Address: "10.254.167.105"})
	if !errors.Is(err, ErrWireUnavailable) {
		t.Fatalf("err = %v, want ErrWireUnavailable", err)
	}
}

func TestWire_ConcurrentAddsAllLand(t *testing.T) {
	w, store, wg := newTestWire(t)
	ctx := context.Background()

	var g sync.WaitGroup

	for i := 2; i < 12; i++ {
		g.Add(1)

		go func() {
			defer g.Done()

			if _, err := w.Add(ctx, WirePeer{PublicKey: testKey(byte(i)), Address: fmt.Sprintf("10.254.1.%d", i)}); err != nil {
				t.Error(err)
			}
		}()
	}

	g.Wait()

	peers, _ := store.ListWirePeers(ctx)
	if len(peers) != 10 {
		t.Fatalf("peers = %d, want 10", len(peers))
	}

	data, _ := os.ReadFile(w.ConfPath)
	if n := strings.Count(string(data), "[Peer]"); n != 10 {
		t.Fatalf("final file has %d peers, want 10:\n%s", n, data)
	}

	if wg.applies() != 10 {
		t.Fatalf("applies = %d, want 10", wg.applies())
	}
}

func TestUFWRules(t *testing.T) {
	rules := UFWRules("br-21f70aa6d28e", 51820)

	want := [][]string{
		{"allow", "51820/udp"},
		{"allow", "in", "on", "wg0", "to", "any", "port", "53"},
		{"allow", "in", "on", "br-21f70aa6d28e", "to", "any", "port", "53"},
		{"route", "allow", "in", "on", "wg0", "out", "on", "br-21f70aa6d28e"},
	}

	if len(rules) != len(want) {
		t.Fatalf("rules = %v", rules)
	}

	for i := range want {
		if strings.Join(rules[i], " ") != strings.Join(want[i], " ") {
			t.Fatalf("rule %d = %v, want %v", i, rules[i], want[i])
		}
	}

	if got := UFWRules("br-x", 0)[0][1]; got != "51820/udp" {
		t.Fatalf("unknown port must default to 51820, got %s", got)
	}
}

func TestWire_UFWNeedsARoutedVoodu0(t *testing.T) {
	w, _, _ := newTestWire(t)

	if _, _, err := w.UFW(); !errors.Is(err, ErrWireNotRouted) {
		t.Fatalf("err = %v, want ErrWireNotRouted", err)
	}

	w.Bridge = func() string { return "br-21f70aa6d28e" }

	bridge, rules, err := w.UFW()
	if err != nil || bridge != "br-21f70aa6d28e" || len(rules) != 4 {
		t.Fatalf("bridge=%q rules=%v err=%v", bridge, rules, err)
	}
}

// The peer's containers are reached through wg0 only if the kernel has a
// route: wg-quick adds none for a peer that arrived after `up`.
func TestWire_AddRoutesThePeersContainersAndRemoveDropsIt(t *testing.T) {
	w, _, _ := newTestWire(t)
	ip := &fakeIP{}
	w.RunIP = ip.run
	ctx := context.Background()

	if _, err := w.Add(ctx, WirePeer{PublicKey: testKey(2), Address: "10.254.167.105"}); err != nil {
		t.Fatal(err)
	}

	if !ip.routes["10.167.105.0/24"] {
		t.Fatalf("no route to the peer's containers: %v", ip.calls)
	}

	if !slices.Contains(ip.calls, "-4 route replace 10.167.105.0/24 dev wg0") {
		t.Fatalf("route not installed on wg0: %v", ip.calls)
	}

	if err := w.Remove(ctx, "10.254.167.105"); err != nil {
		t.Fatal(err)
	}

	if ip.routes["10.167.105.0/24"] {
		t.Fatalf("route survived the remove: %v", ip.calls)
	}

	for _, c := range ip.calls {
		if strings.Contains(c, "10.254.0.0/16") {
			t.Fatalf("the tunnel's own route must never be touched: %v", ip.calls)
		}
	}
}

func TestParseContainerRoutes(t *testing.T) {
	out := "10.254.0.0/16 proto kernel scope link src 10.254.91.221\n" +
		"10.167.105.0/24 scope link\n" +
		"10.200.7.0/24 scope link\n" +
		"192.168.1.0/24 scope link\n" +
		"10.0.0.0/8 via 10.254.0.1\n"

	got := ParseContainerRoutes([]byte(out))

	want := []string{"10.167.105.0/24", "10.200.7.0/24"}
	if len(got) != 2 || got[0].String() != want[0] || got[1].String() != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}
