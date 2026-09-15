package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
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
	dump  string
	fail  bool
}

func (f *fakeWG) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)

	if f.fail {
		return nil, errors.New("wg: not up")
	}

	switch strings.Join(args, " ") {
	case "show wg0 public-key":
		return []byte(testKey(1) + "\n"), nil
	case "show wg0 listen-port":
		return []byte("51820\n"), nil
	case "show wg0 dump":
		return []byte(f.dump), nil
	}

	return nil, nil
}

func (f *fakeWG) syncs() int {
	n := 0

	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "syncconf" {
			n++
		}
	}

	return n
}

func newTestWire(t *testing.T) (*Wire, *memStore, *fakeWG) {
	t.Helper()

	store := newMemStore()
	wg := &fakeWG{}

	w := &Wire{
		Store:        store,
		ConfPath:     filepath.Join(t.TempDir(), "wire", "peers.conf"),
		Run:          wg.run,
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

	if wg.syncs() != 1 || strings.Join(wg.calls[len(wg.calls)-1], " ") != "syncconf wg0 "+w.ConfPath {
		t.Fatalf("wg calls = %v, want one syncconf on the file", wg.calls)
	}
}

func TestWire_AddInvalidStoresNothing(t *testing.T) {
	w, store, wg := newTestWire(t)

	if _, err := w.Add(context.Background(), WirePeer{PublicKey: "bad", Address: "10.254.167.105"}); err == nil {
		t.Fatal("want validation error")
	}

	peers, _ := store.ListWirePeers(context.Background())
	if len(peers) != 0 || wg.syncs() != 0 {
		t.Fatalf("invalid peer reached the store (%d) or wg0 (%d syncs)", len(peers), wg.syncs())
	}
}

func TestWire_AddSameAddressReplaces(t *testing.T) {
	w, store, _ := newTestWire(t)
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

	if wg.syncs() != 2 {
		t.Fatalf("syncs = %d, want 2 (add + remove)", wg.syncs())
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

	if wg.syncs() != 10 {
		t.Fatalf("syncs = %d, want 10", wg.syncs())
	}
}
