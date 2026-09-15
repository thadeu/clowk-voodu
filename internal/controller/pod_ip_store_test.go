package controller

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// etcdStoreForTest starts a real embedded etcd: the reservation's promise —
// never one address for two pods — lives in an etcd transaction, and the
// memStore can only imitate it.
func etcdStoreForTest(t *testing.T) *EtcdStore {
	t.Helper()

	e, err := StartEmbeddedEtcd(EtcdConfig{
		Name:      "test",
		DataDir:   t.TempDir(),
		ClientURL: freeLocalURL(t),
		PeerURL:   freeLocalURL(t),
		Quiet:     true,
	})
	if err != nil {
		t.Fatalf("embedded etcd: %v", err)
	}

	t.Cleanup(e.Close)

	return NewEtcdStore(e.Client)
}

func freeLocalURL(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer l.Close()

	return "http://" + l.Addr().String()
}

func TestEtcdPodIPReservationIsExclusiveBothWays(t *testing.T) {
	ctx := context.Background()
	s := etcdStoreForTest(t)

	if ok, err := s.ReservePodIP(ctx, "data", "pg", 0, "10.8.2.2"); err != nil || !ok {
		t.Fatalf("first reservation: ok=%v err=%v", ok, err)
	}

	// The address is taken.
	if ok, _ := s.ReservePodIP(ctx, "data", "cache", 0, "10.8.2.2"); ok {
		t.Error("10.8.2.2 was handed to a second pod")
	}

	// The pod already has one.
	if ok, _ := s.ReservePodIP(ctx, "data", "pg", 0, "10.8.2.3"); ok {
		t.Error("pg-0 got a second address")
	}

	if ip, _ := s.GetPodIP(ctx, "data", "pg", 0); ip != "10.8.2.2" {
		t.Errorf("pg-0 = %q, want 10.8.2.2", ip)
	}
}

// Many pods racing for the same address: exactly one wins.
func TestEtcdPodIPReservationSurvivesARace(t *testing.T) {
	ctx := context.Background()
	s := etcdStoreForTest(t)

	var wins atomic.Int32

	var wg sync.WaitGroup

	for i := 0; i < 16; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			if ok, err := s.ReservePodIP(ctx, "data", "pg"+strconv.Itoa(i), 0, "10.8.2.2"); err == nil && ok {
				wins.Add(1)
			}
		}(i)
	}

	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("%d pods won 10.8.2.2, want exactly 1", got)
	}
}

func TestEtcdPodIPReleaseFreesOnlyWhatThePodHolds(t *testing.T) {
	ctx := context.Background()
	s := etcdStoreForTest(t)

	_, _ = s.ReservePodIP(ctx, "data", "pg", 0, "10.8.2.2")
	_, _ = s.ReservePodIP(ctx, "data", "pg", 1, "10.8.2.3")

	if err := s.ReleasePodIPs(ctx, "data", "pg"); err != nil {
		t.Fatal(err)
	}

	for _, ip := range []string{"10.8.2.2", "10.8.2.3"} {
		if ok, _ := s.ReservePodIP(ctx, "other", ip, 0, ip); !ok {
			t.Errorf("%s is still held after the statefulset released it", ip)
		}
	}

	// A pod key pointing at an address another pod holds — the state a
	// crash between two writes could leave — must not free that address.
	if _, err := s.client.Put(ctx, PodIPPodKey("data", "stray", 0), "10.8.2.3"); err != nil {
		t.Fatal(err)
	}

	if err := s.ReleasePodIP(ctx, "data", "stray", 0); err != nil {
		t.Fatal(err)
	}

	if ok, _ := s.ReservePodIP(ctx, "third", "x", 0, "10.8.2.3"); ok {
		t.Error("releasing a stray pod key freed an address another pod holds")
	}
}
