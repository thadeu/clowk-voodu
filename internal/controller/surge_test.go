package controller

import (
	"context"
	"testing"

	"go.voodu.clowk.in/internal/containers"
)

// TestCanSurgeReadsTheHostPort decides the whole thing: a replacement
// replica can only run alongside the one it replaces when nothing
// pins a host port, because two containers cannot hold the same one.
//
// The common form is already ephemeral — `ports = ["8080"]` normalises
// to 127.0.0.1::8080, which asks docker to pick — so surge is available
// far more often than the shape of the problem suggests.
func TestCanSurgeReadsTheHostPort(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ports []string
		want  bool
	}{
		{"no ports at all", nil, true},
		{"container port only, host chosen by docker", []string{"8080"}, true},
		{"explicit ephemeral", []string{"0.0.0.0::8084"}, true},
		{"pinned host port", []string{"3000:80"}, false},
		{"pinned with interface", []string{"0.0.0.0:8084:8084"}, false},
		{"one pinned among many", []string{"8080", "3000:80"}, false},
		{"udp container port", []string{"5060/udp"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canSurge(deploymentSpec{Ports: tc.ports}); got != tc.want {
				t.Errorf("canSurge(%v) = %v, want %v", tc.ports, got, tc.want)
			}
		})
	}
}

// TestPinnedHostPortIsAlreadySingleReplica documents why the
// non-surging case costs so little: a deployment that pins a host port
// could never have run more than one replica anyway — the second would
// fail to bind. Surge takes nothing away from it.
func TestPinnedHostPortIsAlreadySingleReplica(t *testing.T) {
	if canSurge(deploymentSpec{Ports: []string{"3000:80"}, Replicas: 3}) {
		t.Fatal("a pinned host port must never surge")
	}
}

// TestRolloutSurgesWhenPortsAllow is the behaviour change: the
// replacement is up before the replica it replaces is taken away, so
// the workload never has a moment with nothing serving.
//
// The old order — remove, then start — is what made `voodu apply` cut
// live connections, and with replicas = 1 there was no load balancer
// anywhere to hide it.
func TestRolloutSurgesWhenPortsAllow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ports []string
		want  []string
	}{
		{"ephemeral host port surges", []string{"8080"}, []string{"ensure", "remove"}},
		{"pinned host port replaces in place", []string{"3000:80"}, []string{"remove", "ensure"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()

			old := containers.ContainerName("prod", "esl", "old1")

			fc := &fakeContainers{slots: map[string]*ContainerSlot{
				old: {
					Name:     old,
					Running:  true,
					Identity: containers.Identity{Kind: containers.KindDeployment, Scope: "prod", Name: "esl", ReplicaID: "old1"},
				},
			}}

			h := &DeploymentHandler{Store: store, Log: quietLogger(), Containers: fc}

			live := []ContainerSlot{*fc.slots[old]}

			spec := deploymentSpec{Image: "esl:1", Ports: tc.ports}

			if err := h.rollingReplaceReplicas(context.Background(), "prod", "esl", "prod-esl", live, spec, "hash", ""); err != nil {
				t.Fatalf("roll: %v", err)
			}

			if len(fc.ops) != 2 || fc.ops[0] != tc.want[0] || fc.ops[1] != tc.want[1] {
				t.Fatalf("order was %v, want %v", fc.ops, tc.want)
			}
		})
	}
}
