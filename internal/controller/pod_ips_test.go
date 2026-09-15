package controller

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/docker"
)

// routedVoodu0 is the plan the install creates on a host whose tunnel
// address is 10.254.91.221: the /24 derived from it, docker's half on top.
func routedVoodu0() docker.NetworkAddressPlan {
	return docker.NetworkAddressPlan{
		Subnet:  netip.MustParsePrefix("10.91.221.0/24"),
		IPRange: netip.MustParsePrefix("10.91.221.128/25"),
		Gateway: netip.MustParseAddr("10.91.221.1"),
	}
}

func allocatorWith(store Store, plan docker.NetworkAddressPlan, ok bool) *podIPAllocator {
	return &podIPAllocator{
		Store: store,
		Plan:  func() (docker.NetworkAddressPlan, bool, error) { return plan, ok, nil },
	}
}

// The fixed half is what docker never hands out on its own: the subnet
// minus network, broadcast, gateway and the --ip-range.
func TestFixedPodAddressesAreThePartDockerLeavesAlone(t *testing.T) {
	pool, ok := fixedPodAddresses(routedVoodu0())
	if !ok {
		t.Fatal("a routed voodu0 must have a fixed part")
	}

	if first, last := pool[0].String(), pool[len(pool)-1].String(); first != "10.91.221.2" || last != "10.91.221.127" {
		t.Errorf("pool = %s..%s, want 10.91.221.2..10.91.221.127", first, last)
	}

	if len(pool) != 126 {
		t.Errorf("pool has %d addresses, want 126", len(pool))
	}

	// Without an ip-range docker may hand out any address in the subnet,
	// so no address is safe to pin.
	noRange := routedVoodu0()
	noRange.IPRange = netip.Prefix{}

	if _, ok := fixedPodAddresses(noRange); ok {
		t.Error("a voodu0 without --ip-range has no part docker leaves alone")
	}

	// Docker takes .1 for the gateway when none was declared.
	noGateway := routedVoodu0()
	noGateway.Gateway = netip.Addr{}

	if pool, _ := fixedPodAddresses(noGateway); pool[0].String() != "10.91.221.2" {
		t.Errorf("first address = %s, want 10.91.221.2 (docker's default gateway skipped)", pool[0])
	}
}

// A voodu0 that is not routed changes nothing: the pod takes whatever
// docker gives it, and nothing is reserved.
func TestPodIPIsInertWithoutARoutedVoodu0(t *testing.T) {
	store := newMemStore()

	ip, err := allocatorWith(store, docker.NetworkAddressPlan{}, false).addressFor(context.Background(), "data", "pg", 0)
	if err != nil || ip != "" {
		t.Fatalf("got %q, %v — want no address and no error", ip, err)
	}

	if held, _ := store.GetPodIP(context.Background(), "data", "pg", 0); held != "" {
		t.Errorf("reserved %q on a voodu0 that is not routed", held)
	}
}

// The whole point: the same pod gets the same address every time, and no
// two pods share one.
func TestPodIPIsStablePerPodAndUniqueAcrossPods(t *testing.T) {
	ctx := context.Background()
	a := allocatorWith(newMemStore(), routedVoodu0(), true)

	first, _ := a.addressFor(ctx, "data", "pg", 0)
	again, _ := a.addressFor(ctx, "data", "pg", 0)

	if first == "" || first != again {
		t.Fatalf("pg-0 got %q then %q — the address must not move", first, again)
	}

	seen := map[string]string{first: "data/pg/0"}

	for _, pod := range []struct {
		scope, name string
		ordinal     int
	}{{"data", "pg", 1}, {"data", "cache", 0}, {"clowk", "pg", 0}} {
		ip, err := a.addressFor(ctx, pod.scope, pod.name, pod.ordinal)
		if err != nil {
			t.Fatal(err)
		}

		if owner, dup := seen[ip]; dup {
			t.Fatalf("%s/%s/%d got %s, already held by %s", pod.scope, pod.name, pod.ordinal, ip, owner)
		}

		seen[ip] = pod.scope + "/" + pod.name
	}
}

// A full pool is an error that says so, not a silent fallback to a
// docker-assigned address the other VM does not know.
func TestPodIPExhaustionNamesItself(t *testing.T) {
	ctx := context.Background()

	// 10.8.2.0/29 = .1 gateway, .2-.3 fixed, .4-.7 docker's.
	tiny := docker.NetworkAddressPlan{
		Subnet:  netip.MustParsePrefix("10.8.2.0/29"),
		IPRange: netip.MustParsePrefix("10.8.2.4/30"),
		Gateway: netip.MustParseAddr("10.8.2.1"),
	}

	a := allocatorWith(newMemStore(), tiny, true)

	for ord := 0; ord < 2; ord++ {
		if _, err := a.addressFor(ctx, "data", "pg", ord); err != nil {
			t.Fatalf("pod %d: %v", ord, err)
		}
	}

	_, err := a.addressFor(ctx, "data", "pg", 2)
	if err == nil || !strings.Contains(err.Error(), "no fixed address left") {
		t.Fatalf("third pod: err = %v, want the pool-exhausted error", err)
	}
}

// voodu0 recreated with another subnet (a manual migration): the old
// reservation points outside the network, so the pod draws again.
func TestPodIPFollowsARecreatedVoodu0(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()

	_, _ = store.ReservePodIP(ctx, "data", "pg", 0, "10.8.9.2")

	ip, err := allocatorWith(store, routedVoodu0(), true).addressFor(ctx, "data", "pg", 0)
	if err != nil {
		t.Fatal(err)
	}

	if !netip.MustParsePrefix("10.91.221.0/24").Contains(netip.MustParseAddr(ip)) {
		t.Fatalf("pg-0 kept %s, outside the current voodu0", ip)
	}

	// The stale address is free again.
	if ok, _ := store.ReservePodIP(ctx, "other", "x", 0, "10.8.9.2"); !ok {
		t.Error("the stale reservation still holds 10.8.9.2")
	}
}

// Deleting the statefulset hands its addresses back.
func TestPodIPReleaseFreesTheAddresses(t *testing.T) {
	ctx := context.Background()
	a := allocatorWith(newMemStore(), routedVoodu0(), true)

	ip, _ := a.addressFor(ctx, "data", "pg", 0)

	if err := a.release(ctx, "data", "pg"); err != nil {
		t.Fatal(err)
	}

	if held, _ := a.Store.GetPodIP(ctx, "data", "pg", 0); held != "" {
		t.Errorf("pg-0 still holds %s after release", held)
	}

	if next, _ := a.addressFor(ctx, "data", "cache", 0); next != ip {
		t.Errorf("cache-0 got %s, want the freed %s", next, ip)
	}
}

// A voodu0 read failure is the reconcile's failure — retried — and never a
// pod created with an address the other VM does not know.
func TestPodIPPlanErrorFailsLoudly(t *testing.T) {
	a := &podIPAllocator{
		Store: newMemStore(),
		Plan: func() (docker.NetworkAddressPlan, bool, error) {
			return docker.NetworkAddressPlan{}, false, errors.New("daemon unreachable")
		},
	}

	if _, err := a.addressFor(context.Background(), "data", "pg", 0); err == nil {
		t.Fatal("want the plan error, got none")
	}
}

// End to end through the handler: pods on a routed voodu0 are created with
// their fixed address, keep it through a rolling restart, keep it when
// scaled down, and hand it back when the statefulset is deleted.
func TestStatefulsetPodsGetFixedAddressesOnARoutedVoodu0(t *testing.T) {
	withZeroRolloutPause(t)

	ctx := context.Background()
	store := newMemStore()
	cm := &fakeContainers{}
	envChanged := false

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return envChanged, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
		PodIPs:      allocatorWith(store, routedVoodu0(), true),
	}

	ev := putEvent(t, KindStatefulset, "pg", map[string]any{"image": "postgres:16", "replicas": 2})

	if err := h.Handle(ctx, ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	created := map[string]string{}

	for _, e := range cm.ensures {
		if e.IP == "" {
			t.Fatalf("%s was created without a fixed address", e.Name)
		}

		created[e.Name] = e.IP
	}

	if created["test-pg.0"] == created["test-pg.1"] {
		t.Fatalf("both pods got %s", created["test-pg.0"])
	}

	// Rolling restart: every pod is removed and created again.
	envChanged = true
	cm.ensures = nil

	if err := h.Handle(ctx, ev); err != nil {
		t.Fatalf("Handle (restart): %v", err)
	}

	if len(cm.ensures) != 2 {
		t.Fatalf("restart re-created %d pods, want 2", len(cm.ensures))
	}

	for _, e := range cm.ensures {
		if e.IP != created[e.Name] {
			t.Errorf("%s came back on %s, was %s — the address must survive the recreate", e.Name, e.IP, created[e.Name])
		}
	}

	// Delete hands the addresses back.
	if err := h.Handle(ctx, WatchEvent{Type: WatchDelete, Kind: KindStatefulset, Scope: "test", Name: "pg"}); err != nil {
		t.Fatalf("Handle (delete): %v", err)
	}

	for ord := 0; ord < 2; ord++ {
		if held, _ := store.GetPodIP(ctx, "test", "pg", ord); held != "" {
			t.Errorf("pg-%d still holds %s after the statefulset was deleted", ord, held)
		}
	}
}

// --ip applies to the primary network. A pod whose default route runs
// through a network the operator declared would answer a remote caller
// from the wrong address, so it gets no fixed one.
func TestStatefulsetPodWithAnotherPrimaryNetworkGetsNoFixedAddress(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
		PodIPs:      allocatorWith(store, routedVoodu0(), true),
	}

	ev := putEvent(t, KindStatefulset, "pg", map[string]any{
		"image":    "postgres:16",
		"replicas": 1,
		"networks": []string{"backend"},
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(cm.ensures) != 1 || cm.ensures[0].IP != "" {
		t.Fatalf("ensures = %+v, want one pod with no fixed address", cm.ensures)
	}
}
