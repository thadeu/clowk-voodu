package controller

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"go.voodu.clowk.in/internal/docker"
)

// podIPAllocator gives each statefulset pod a fixed address on voodu0.
//
// Every other container on voodu0 takes whatever address docker hands out,
// and gets a new one when it is recreated. Inside a VM that is fine: the
// network alias is the stable handle. A pod reached from another VM is
// different — its address goes into a connection string over there — so it
// has to survive the pod being recreated here. The allocator draws it from
// the part of voodu0 docker leaves alone, and keeps it for as long as the
// statefulset exists, whatever happens to the pod in between.
//
// Inert unless voodu0 was created with a subnet and an --ip-range of its
// own. Without the ip-range there is no part of the network docker leaves
// alone, and a pinned address could collide with one docker hands out.
type podIPAllocator struct {
	Store Store

	// Plan reads voodu0's address plan. A seam for tests.
	Plan func() (docker.NetworkAddressPlan, bool, error)
}

// newVoodu0PodIPAllocator is the production wiring.
func newVoodu0PodIPAllocator(store Store) *podIPAllocator {
	return &podIPAllocator{
		Store: store,
		Plan:  func() (docker.NetworkAddressPlan, bool, error) { return docker.InspectNetworkAddressPlan("voodu0") },
	}
}

// fixedPodAddresses is the part of a network a pod's fixed address comes
// from: the subnet minus its network and broadcast addresses, the gateway,
// and the range docker assigns from. ok is false when there is no such part.
func fixedPodAddresses(plan docker.NetworkAddressPlan) ([]netip.Addr, bool) {
	if !plan.Subnet.IsValid() || !plan.Subnet.Addr().Is4() || !plan.IPRange.IsValid() {
		return nil, false
	}

	subnet := plan.Subnet.Masked()

	// Docker takes the first address for the gateway when none was given.
	gateway := plan.Gateway
	if !gateway.IsValid() {
		gateway = subnet.Addr().Next()
	}

	var out []netip.Addr

	for a := subnet.Addr().Next(); subnet.Contains(a.Next()); a = a.Next() {
		if a == gateway || plan.IPRange.Contains(a) {
			continue
		}

		out = append(out, a)
	}

	return out, len(out) > 0
}

// addressFor returns the pod's fixed address, reserving one on first use.
// "" with no error means voodu0 is not routed and the pod takes whatever
// docker gives it, exactly as before.
func (a *podIPAllocator) addressFor(ctx context.Context, scope, name string, ordinal int) (string, error) {
	if a == nil {
		return "", nil
	}

	plan, ok, err := a.Plan()
	if err != nil {
		return "", fmt.Errorf("read voodu0 address plan: %w", err)
	}

	if !ok {
		return "", nil
	}

	pool, ok := fixedPodAddresses(plan)
	if !ok {
		return "", nil
	}

	held, err := a.Store.GetPodIP(ctx, scope, name, ordinal)
	if err != nil {
		return "", err
	}

	if held != "" {
		if addr, err := netip.ParseAddr(held); err == nil && slices.Contains(pool, addr) {
			return held, nil
		}

		// Reserved against an earlier voodu0 that had another subnet: the
		// network was recreated. Drop it and draw again from this one.
		if err := a.Store.ReleasePodIP(ctx, scope, name, ordinal); err != nil {
			return "", err
		}
	}

	for _, addr := range pool {
		reserved, err := a.Store.ReservePodIP(ctx, scope, name, ordinal, addr.String())
		if err != nil {
			return "", err
		}

		if reserved {
			return addr.String(), nil
		}

		// The address is taken — or this very pod was reserved in the
		// meantime, in which case its address is the answer.
		held, err := a.Store.GetPodIP(ctx, scope, name, ordinal)
		if err != nil {
			return "", err
		}

		if held != "" {
			return held, nil
		}
	}

	return "", fmt.Errorf("voodu0 has no fixed address left in %s for statefulset %s/%s pod %d", plan.Subnet, scope, name, ordinal)
}

// release frees every address the statefulset holds. Scaling down does not
// call it — the ordinal may come back and should find its address waiting.
func (a *podIPAllocator) release(ctx context.Context, scope, name string) error {
	if a == nil {
		return nil
	}

	return a.Store.ReleasePodIPs(ctx, scope, name)
}
