package controller

import (
	"context"
	"testing"
	"time"
)

func TestEtcdStore_WirePeers(t *testing.T) {
	store := etcdStoreForTest(t)
	ctx := context.Background()

	p := WirePeer{PublicKey: testKey(2), Address: "10.254.167.105", Endpoint: "1.2.3.4:51820", AddedAt: time.Now().UTC().Truncate(time.Second)}

	if err := store.PutWirePeer(ctx, p); err != nil {
		t.Fatal(err)
	}

	if err := store.PutWirePeer(ctx, WirePeer{PublicKey: testKey(3), Address: "10.254.200.7"}); err != nil {
		t.Fatal(err)
	}

	peers, err := store.ListWirePeers(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(peers) != 2 {
		t.Fatalf("peers = %d, want 2", len(peers))
	}

	for _, got := range peers {
		if got.Address == p.Address && (got.PublicKey != p.PublicKey || got.Endpoint != p.Endpoint || !got.AddedAt.Equal(p.AddedAt)) {
			t.Fatalf("round trip lost fields: %+v", got)
		}
	}

	if err := store.PutWirePeer(ctx, WirePeer{Address: ""}); err == nil {
		t.Fatal("empty address must be rejected")
	}

	deleted, err := store.DeleteWirePeer(ctx, "10.254.167.105")
	if err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}

	deleted, err = store.DeleteWirePeer(ctx, "10.254.167.105")
	if err != nil || deleted {
		t.Fatalf("second delete = %v, %v; want false, nil", deleted, err)
	}

	peers, _ = store.ListWirePeers(ctx)
	if len(peers) != 1 {
		t.Fatalf("peers after delete = %d, want 1", len(peers))
	}
}
