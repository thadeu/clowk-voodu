package controller

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"go.voodu.clowk.in/internal/containers"
	"go.voodu.clowk.in/pkg/plugin"
)

// runningReplica replaces the fake runtime with exactly one live
// replica of test/api, the way a roll leaves it: the previous
// container is gone, not stopped, and the new one answers to a
// different name.
func runningReplica(fc *fakeContainers, replicaID string) string {
	name := containers.ContainerName("test", "api", replicaID)

	fc.slots = map[string]*ContainerSlot{
		name: {
			Name: name,
			Identity: containers.Identity{
				Kind:      containers.KindDeployment,
				Scope:     "test",
				Name:      "api",
				ReplicaID: replicaID,
			},
			Running: true,
		},
	}

	return name
}

// lastUpstreams decodes the upstream list from the most recent apply
// the ingress plugin was invoked with.
func lastUpstreams(t *testing.T, inv *fakeInvoker) []string {
	t.Helper()

	if len(inv.calls) == 0 {
		t.Fatal("ingress plugin was never invoked")
	}

	raw := inv.calls[len(inv.calls)-1].Env[plugin.EnvIngressUpstreams]

	var got []string

	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode %s %q: %v", plugin.EnvIngressUpstreams, raw, err)
	}

	return got
}

// TestIngressRepublishFollowsReplicaTurnover is the regression that the
// upstream tests could not catch.
//
// deploymentUpstreams is correct in isolation — it reports whatever the
// runtime holds when asked. The bug was that nobody asked again. Since
// upstreams became one container name per replica, the value depends on
// the runtime, but the reconciler only fires on /desired/* changes and
// replacing a pod changes no manifest. The route kept naming a
// container that no longer existed, docker's DNS answered `no such
// host`, and the router served 503 beside a healthy deployment.
func TestIngressRepublishFollowsReplicaTurnover(t *testing.T) {
	store := newMemStore()

	seedManifest(t, store, KindDeployment, "api", deploymentSpec{Image: "api:latest", Ports: []string{"8080"}})
	seedManifest(t, store, KindIngress, "api", ingressSpec{Host: "api.example.com", Port: 8080})

	fc := &fakeContainers{}
	before := runningReplica(fc, "a1")

	inv := &fakeInvoker{}
	h := &IngressHandler{Store: store, Invoker: inv, Containers: fc, Log: quietLogger()}

	ev := putEvent(t, KindIngress, "api", ingressSpec{Host: "api.example.com", Port: 8080})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("initial apply: %v", err)
	}

	if got := lastUpstreams(t, inv); len(got) != 1 || got[0] != before+":8080" {
		t.Fatalf("after apply upstreams = %v, want [%s:8080]", got, before)
	}

	// The roll. Nothing in /desired changes, so no watch event fires and
	// the ingress handler is never called by the reconciler again.
	after := runningReplica(fc, "b2")

	if err := h.RepublishFor(context.Background(), "test", "api"); err != nil {
		t.Fatalf("RepublishFor: %v", err)
	}

	got := lastUpstreams(t, inv)

	if len(got) != 1 || got[0] != after+":8080" {
		t.Fatalf("after roll upstreams = %v, want [%s:8080]", got, after)
	}

	if got[0] == before+":8080" {
		t.Fatal("upstream still names the replaced container — docker's DNS answers `no such host` for it")
	}
}

// TestIngressRepublishIgnoresOtherDeployments keeps a republish scoped
// to the deployment that actually rolled. Re-applying every route in
// the scope would hand unrelated ingresses a plugin call they did not
// need, and one of them failing would fail this deployment's reconcile.
func TestIngressRepublishIgnoresOtherDeployments(t *testing.T) {
	store := newMemStore()

	seedManifest(t, store, KindDeployment, "api", deploymentSpec{Image: "api:latest", Ports: []string{"8080"}})
	seedManifest(t, store, KindIngress, "api", ingressSpec{Host: "api.example.com", Port: 8080})

	fc := &fakeContainers{}
	runningReplica(fc, "a1")

	inv := &fakeInvoker{}
	h := &IngressHandler{Store: store, Invoker: inv, Containers: fc, Log: quietLogger()}

	if err := h.RepublishFor(context.Background(), "test", "worker"); err != nil {
		t.Fatalf("RepublishFor: %v", err)
	}

	if len(inv.calls) != 0 {
		t.Fatalf("republishing for 'worker' touched %d route(s); the ingress targets 'api'", len(inv.calls))
	}
}

// fakeRepublisher records what the deployment reconcile asked for.
type fakeRepublisher struct {
	calls []string
	err   error
}

func (f *fakeRepublisher) RepublishFor(_ context.Context, scope, deployment string) error {
	f.calls = append(f.calls, scope+"/"+deployment)

	return f.err
}

// TestDeploymentReconcileRepublishesIngress pins the wiring. The
// resolver being right is worth nothing if the reconcile never calls
// it — that gap is the whole outage.
func TestDeploymentReconcileRepublishesIngress(t *testing.T) {
	store := newMemStore()

	rp := &fakeRepublisher{}
	cm := &fakeContainers{}

	h := &DeploymentHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
		Ingresses:   rp,
	}

	ev := putEvent(t, KindDeployment, "api", deploymentSpec{Image: "img:1"})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(rp.calls) != 1 || rp.calls[0] != "test/api" {
		t.Fatalf("republish calls = %v, want [test/api]", rp.calls)
	}
}

// TestDeploymentReconcileSkipsRepublishWithoutRunningReplicas covers
// the stopped deployment. There is no upstream to publish, and
// resolving one would fail the reconcile over a state the operator
// asked for — `vd stop` would leave the deployment showing a reconcile
// error it cannot clear.
func TestDeploymentReconcileSkipsRepublishWithoutRunningReplicas(t *testing.T) {
	rp := &fakeRepublisher{}
	cm := &fakeContainers{}

	name := runningReplica(cm, "a1")
	cm.slots[name].Running = false

	h := &DeploymentHandler{
		Store:      newMemStore(),
		Log:        quietLogger(),
		Containers: cm,
		Ingresses:  rp,
	}

	if err := h.republishIngresses(context.Background(), "test", "api"); err != nil {
		t.Fatalf("republishIngresses: %v", err)
	}

	if len(rp.calls) != 0 {
		t.Fatalf("stopped deployment republished %v; nothing is listening to route to", rp.calls)
	}
}

// TestRollingReplaceRepublishesIngress covers the half the first fix
// missed.
//
// Publishing only at the end of apply is enough for registry-mode,
// where the handler creates the containers itself. Build-mode never
// reaches that code — `ensureReplicaCount` returns early when
// spec.Image is empty and the rollout owns the containers — so a
// build-mode deploy replaced the replica and left the router holding
// the name of the one it had just retired. Restarting the controller
// masked it, because the reconciler's startup replay republished
// against whatever was live at that moment; the next apply broke it
// again.
//
// rollingReplaceReplicas is the choke point every turnover goes
// through: Release, Rollback, `vd restart`, and the apply-time
// recreate.
func TestRollingReplaceRepublishesIngress(t *testing.T) {
	old := containers.ContainerName("test", "api", "old1")

	fc := &fakeContainers{slots: map[string]*ContainerSlot{
		old: {
			Name:     old,
			Running:  true,
			Identity: containers.Identity{Kind: containers.KindDeployment, Scope: "test", Name: "api", ReplicaID: "old1"},
		},
	}}

	rp := &fakeRepublisher{}

	h := &DeploymentHandler{
		Store:      newMemStore(),
		Log:        quietLogger(),
		Containers: fc,
		Ingresses:  rp,
	}

	live := []ContainerSlot{*fc.slots[old]}
	spec := deploymentSpec{Image: "api:1", Ports: []string{"8080"}}

	if err := h.rollingReplaceReplicas(context.Background(), "test", "api", "test-api", live, spec, "hash", ""); err != nil {
		t.Fatalf("rollingReplaceReplicas: %v", err)
	}

	// Two publishes per turnover: one with old+new (before the old goes),
	// one with new only (after). See TestRollingReplacePublishesNewBeforeRetiringOld.
	if len(rp.calls) != 2 || rp.calls[0] != "test/api" || rp.calls[1] != "test/api" {
		t.Fatalf("republish calls = %v, want [test/api test/api] — the replica set turned over", rp.calls)
	}
}

// snapshotRepublisher records which replicas were RUNNING each time the
// router was asked to republish — the order is the whole point.
type snapshotRepublisher struct {
	fc        *fakeContainers
	snapshots [][]string
}

func (r *snapshotRepublisher) RepublishFor(_ context.Context, _, _ string) error {
	var running []string

	for name, slot := range r.fc.slots {
		if slot.Running {
			running = append(running, name)
		}
	}

	sort.Strings(running)
	r.snapshots = append(r.snapshots, running)

	return nil
}

// A single-replica app used to take a ~5s 502 on every deploy: the new
// replica was ready, the old one was removed, and only THEN was the
// router told — so it sent the whole window to a dead container. The
// surge path must publish with both replicas live before the old one
// is retired, and again with only the new one after.
func TestRollingReplacePublishesNewBeforeRetiringOld(t *testing.T) {
	old := containers.ContainerName("test", "api", "old1")

	fc := &fakeContainers{slots: map[string]*ContainerSlot{
		old: {
			Name:     old,
			Running:  true,
			Identity: containers.Identity{Kind: containers.KindDeployment, Scope: "test", Name: "api", ReplicaID: "old1"},
		},
	}}

	rp := &snapshotRepublisher{fc: fc}

	h := &DeploymentHandler{
		Store:      newMemStore(),
		Log:        quietLogger(),
		Containers: fc,
		Ingresses:  rp,
	}

	live := []ContainerSlot{*fc.slots[old]}
	spec := deploymentSpec{Image: "api:1", Ports: []string{"8080"}}

	if err := h.rollingReplaceReplicas(context.Background(), "test", "api", "test-api", live, spec, "hash", ""); err != nil {
		t.Fatalf("rollingReplaceReplicas: %v", err)
	}

	if len(rp.snapshots) != 2 {
		t.Fatalf("republish snapshots = %v, want exactly two (before and after retiring the old replica)", rp.snapshots)
	}

	first, second := rp.snapshots[0], rp.snapshots[1]

	if len(first) != 2 || !hasName(first, old) {
		t.Fatalf("first republish saw %v; want the old replica AND the new one live", first)
	}

	if len(second) != 1 || hasName(second, old) {
		t.Fatalf("second republish saw %v; want only the new replica", second)
	}
}

func hasName(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}

	return false
}
