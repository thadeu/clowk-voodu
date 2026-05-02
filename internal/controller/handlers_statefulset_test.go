package controller

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/containers"
)

// withZeroRolloutPause swaps slotRolloutPause to 0 for the duration
// of a test so the ordered spawn loop doesn't add 2s per replica.
// Restored on cleanup.
func withZeroRolloutPause(t *testing.T) {
	t.Helper()

	orig := slotRolloutPause

	slotRolloutPause = 0

	t.Cleanup(func() { slotRolloutPause = orig })
}

// statefulsetSlot is the test-side shape of a pre-seeded statefulset
// pod. Mirrors deploymentSlot but stamps the statefulset-specific
// Kind + ReplicaOrdinal so ListByIdentity finds it under the right
// kind and Identity.Ordinal() recovers cleanly.
func statefulsetSlot(scope, name, image string, ordinal int) ContainerSlot {
	id := containers.OrdinalReplicaID(ordinal)

	return ContainerSlot{
		Name:  containers.ContainerName(scope, name, id),
		Image: image,
		Identity: containers.Identity{
			Kind:           containers.KindStatefulset,
			Scope:          scope,
			Name:           name,
			ReplicaID:      id,
			ReplicaOrdinal: ordinal,
		},
		Running: true,
	}
}

// TestStatefulsetHandler_SpawnsOrdinalsBottomUp pins the most
// fundamental contract of the handler: replicas spawn in order
// 0 → 1 → 2, with deterministic container names, the right
// labels, and per-pod + shared aliases on each network.
//
// Without this test a regression in ensureOrdinalsUp could
// silently flip the spawn order to top-down, breaking the
// postgres-style "primary at ordinal 0 boots first" convention.
func TestStatefulsetHandler_SpawnsOrdinalsBottomUp(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	spec := map[string]any{
		"image":    "postgres:15",
		"replicas": 3,
	}

	ev := putEvent(t, KindStatefulset, "pg", spec)

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(cm.ensures) != 3 {
		t.Fatalf("expected 3 ensures, got %d: %+v", len(cm.ensures), cm.ensures)
	}

	for n, e := range cm.ensures {
		wantName := "test-pg." + containers.OrdinalReplicaID(n)
		if e.Name != wantName {
			t.Errorf("ensures[%d].Name = %q, want %q (spawn order broken)", n, e.Name, wantName)
		}

		// Per-pod alias must be present so `pg-N.test` resolves
		// to this specific replica. Shared alias also present
		// so round-robin clients (`pg.test`) still work.
		wantPodAlias := "pg-" + containers.OrdinalReplicaID(n) + ".test"
		if !slices.Contains(e.NetworkAliases, wantPodAlias) {
			t.Errorf("ordinal %d missing per-pod alias %q: %+v", n, wantPodAlias, e.NetworkAliases)
		}

		if !slices.Contains(e.NetworkAliases, "pg.test") {
			t.Errorf("ordinal %d missing shared alias pg.test: %+v", n, e.NetworkAliases)
		}

		if !slices.Contains(e.Labels, containers.LabelKind+"="+containers.KindStatefulset) {
			t.Errorf("ordinal %d label set missing voodu.kind=statefulset: %+v", n, e.Labels)
		}

		// Ordinal label must be emitted — without it,
		// docker ps --filter label=voodu.replica_ordinal=N
		// breaks and the rolling-restart sort can't
		// recover the index.
		wantOrdLabel := containers.LabelReplicaOrdinal + "=" + containers.OrdinalReplicaID(n)
		if !slices.Contains(e.Labels, wantOrdLabel) {
			t.Errorf("ordinal %d label set missing %q: %+v", n, wantOrdLabel, e.Labels)
		}
	}
}

// TestStatefulsetHandler_PrunesAboveDesired confirms scale-down
// removes the highest-ordinal pods first. The test pre-seeds 3
// pods then re-applies with replicas=1; pods 1 and 2 must go,
// pod 0 must survive. Without top-down ordering, a bug could
// remove pod-0 (the primary by convention) and break the cluster.
func TestStatefulsetHandler_PrunesAboveDesired(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	// Seed three live pods first, with a status hash that
	// matches what the upcoming apply will compute. Otherwise
	// recreateOrdinalsIfSpecChanged would see the seeded slots
	// as drifted (no prior status -> baseline path), which is
	// fine but mixes scale-down with rolling-restart removes.
	for i := 0; i < 3; i++ {
		cm.seedSlot(statefulsetSlot("test", "pg", "postgres:15", i))
	}

	preHash := statefulsetSpecHash(statefulsetSpec{
		Image:    "postgres:15",
		Networks: []string{"voodu0"},
		Restart:  "unless-stopped",
	}, nil)

	pre, _ := json.Marshal(DeploymentStatus{
		Image:    "postgres:15",
		SpecHash: preHash,
		Replicas: 3,
	})

	_ = store.PutStatus(context.Background(), KindStatefulset, "test-pg", pre)

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindStatefulset, "pg", map[string]any{
		"image":    "postgres:15",
		"replicas": 1,
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Top-down: ordinal 2 first, then ordinal 1. Ordinal 0 stays.
	want := []string{"test-pg.2", "test-pg.1"}

	if len(cm.removes) != len(want) {
		t.Fatalf("removes = %v, want %v", cm.removes, want)
	}

	for i, r := range cm.removes {
		if r != want[i] {
			t.Errorf("removes[%d] = %q, want %q (top-down order broken)", i, r, want[i])
		}
	}
}

// TestStatefulsetHandler_MaterialisesVolumeClaimsPerOrdinal pins
// the M-S2 contract: every VolumeClaim block produces one docker
// volume per ordinal, named deterministically, and the resulting
// `<volume>:<mountpath>` mount is appended to the pod's Volumes.
//
// Without this test a regression in ensureClaimsForOrdinal could
// silently drop the volume create call, making postgres replicas
// share one anonymous docker-managed volume — a quiet but
// catastrophic failure mode (data loss on the second pod's first
// write).
func TestStatefulsetHandler_MaterialisesVolumeClaimsPerOrdinal(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	spec := map[string]any{
		"image":    "postgres:15",
		"replicas": 2,
		"volume_claims": []map[string]any{
			{"name": "data", "mount_path": "/var/lib/postgresql/data"},
		},
	}

	ev := putEvent(t, KindStatefulset, "pg", spec)

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Two pods × one claim = two volumes. Names are
	// deterministic per (scope, name, claim, ordinal).
	wantVolumes := []string{
		"voodu-test-pg-data-0",
		"voodu-test-pg-data-1",
	}

	for _, want := range wantVolumes {
		if _, ok := cm.volumes[want]; !ok {
			t.Errorf("expected volume %q to exist, got %v", want, cm.volumes)
		}
	}

	// Each pod must mount its OWN volume — pod-0 sees data-0,
	// pod-1 sees data-1. Crossing them would defeat statefulset
	// identity.
	for n, e := range cm.ensures {
		wantMount := "voodu-test-pg-data-" + containers.OrdinalReplicaID(n) + ":/var/lib/postgresql/data"
		if !slices.Contains(e.Volumes, wantMount) {
			t.Errorf("ordinal %d Volumes missing %q: %v", n, wantMount, e.Volumes)
		}
	}

	// Volume labels carry the ordinal so prune / describe can
	// find the volume by (scope, name, ordinal) without parsing
	// the volume name.
	for n, want := range wantVolumes {
		labels := cm.volumes[want]
		wantLbl := containers.LabelReplicaOrdinal + "=" + containers.OrdinalReplicaID(n)

		if !slices.Contains(labels, wantLbl) {
			t.Errorf("volume %q missing ordinal label %q: %v", want, wantLbl, labels)
		}
	}
}

// TestStatefulsetHandler_RollbackPreservesVolumes is the M-S3
// invariant: rolling back a scaled-up statefulset MUST NOT
// destroy the volumes of the dropped ordinals. The whole point
// of statefulset rollback is "I want yesterday's spec WITH
// today's data" — wiping volumes silently would defeat the
// point and could lose data the operator never explicitly
// approved deleting.
func TestStatefulsetHandler_RollbackPreservesVolumes(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	// Apply 1: replicas=1 + one volume claim. Captures the
	// snapshot we'll roll back to. Persist via Store.Put first
	// so Rollback() can later read the manifest back — the
	// production /apply HTTP handler does this; the test
	// shortcuts straight into the handler so we replicate the
	// store write here.
	ev1 := putEvent(t, KindStatefulset, "pg", map[string]any{
		"image":    "postgres:15",
		"replicas": 1,
		"volume_claims": []map[string]any{
			{"name": "data", "mount_path": "/var/lib/postgresql/data"},
		},
	})

	if _, err := store.Put(context.Background(), ev1.Manifest); err != nil {
		t.Fatalf("Store.Put apply 1: %v", err)
	}

	if err := h.Handle(context.Background(), ev1); err != nil {
		t.Fatalf("Handle apply 1: %v", err)
	}

	// Apply 2: replicas=3. Triggers a release record (count
	// changed), spawns ordinals 1 and 2 with their own volumes.
	ev2 := putEvent(t, KindStatefulset, "pg", map[string]any{
		"image":    "postgres:15",
		"replicas": 3,
		"volume_claims": []map[string]any{
			{"name": "data", "mount_path": "/var/lib/postgresql/data"},
		},
	})

	if _, err := store.Put(context.Background(), ev2.Manifest); err != nil {
		t.Fatalf("Store.Put apply 2: %v", err)
	}

	if err := h.Handle(context.Background(), ev2); err != nil {
		t.Fatalf("Handle apply 2: %v", err)
	}

	// Confirm the post-apply state: 3 volumes, 3 pods, two
	// release records.
	for n := 0; n < 3; n++ {
		want := "voodu-test-pg-data-" + containers.OrdinalReplicaID(n)
		if _, ok := cm.volumes[want]; !ok {
			t.Fatalf("pre-rollback: volume %q missing", want)
		}
	}

	status, _ := h.loadStatus(context.Background(), "test-pg")

	if len(status.Releases) != 2 {
		t.Fatalf("expected 2 release records, got %d", len(status.Releases))
	}

	// Target the ORIGINAL release (replicas=1) by ID.
	originalRelease := status.Releases[1].ID

	// Rollback to it.
	if _, err := h.Rollback(context.Background(), "test", "pg", originalRelease); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Pods 1 and 2 should be gone (scaled back to 1).
	wantRemoves := []string{"test-pg.2", "test-pg.1"}

	for _, want := range wantRemoves {
		found := false

		for _, r := range cm.removes {
			if r == want {
				found = true
				break
			}
		}

		if !found {
			t.Errorf("expected remove of %q during rollback, got %v", want, cm.removes)
		}
	}

	// THE INVARIANT: volumes for ordinals 1 and 2 STILL exist
	// after the rollback. This is what makes the rollback safe —
	// scale back up later and the data flows back in.
	for n := 0; n < 3; n++ {
		want := "voodu-test-pg-data-" + containers.OrdinalReplicaID(n)
		if _, ok := cm.volumes[want]; !ok {
			t.Errorf("rollback destroyed volume %q — data loss", want)
		}
	}

	// volumeOps must contain ZERO 'remove' entries — the prune
	// path is the only thing that should ever destroy volumes,
	// and rollback never invokes it.
	for _, op := range cm.volumeOps {
		if len(op) >= 7 && op[:7] == "remove " {
			t.Errorf("rollback issued volume remove op: %q", op)
		}
	}
}

// TestStatefulsetHandler_PruneVolumesWipesAllOrdinals is the
// other side of the rollback contract: when the operator
// explicitly opts in via `vd delete --prune`, every volume the
// statefulset ever owned (including ordinals scaled down out of)
// gets removed in one sweep.
func TestStatefulsetHandler_PruneVolumesWipesAllOrdinals(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindStatefulset, "pg", map[string]any{
		"image":    "postgres:15",
		"replicas": 3,
		"volume_claims": []map[string]any{
			{"name": "data", "mount_path": "/var/lib/postgresql/data"},
			{"name": "wal", "mount_path": "/var/lib/postgresql/wal"},
		},
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// 3 ordinals × 2 claims = 6 volumes.
	if len(cm.volumes) != 6 {
		t.Fatalf("expected 6 volumes pre-prune, got %d: %v", len(cm.volumes), cm.volumes)
	}

	removed, err := h.PruneVolumes("test", "pg")
	if err != nil {
		t.Fatalf("PruneVolumes: %v", err)
	}

	if len(removed) != 6 {
		t.Errorf("PruneVolumes removed %d, want 6: %v", len(removed), removed)
	}

	if len(cm.volumes) != 0 {
		t.Errorf("expected 0 volumes post-prune, got %d: %v", len(cm.volumes), cm.volumes)
	}
}

// TestStatefulsetHandler_RestartsTopDownOnSpecDrift covers the
// canonical update flow: image bump triggers spec hash drift, every
// pod recycles, ordering is N-1 → 0 so the convention-bearing pod-0
// is the LAST to swap. Removes and ensures must alternate per pod
// because the rolling-replace loop does Remove → Ensure for each.
func TestStatefulsetHandler_RestartsTopDownOnSpecDrift(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	for i := 0; i < 3; i++ {
		cm.seedSlot(statefulsetSlot("test", "pg", "postgres:15", i))
	}

	prevHash := statefulsetSpecHash(statefulsetSpec{
		Image:    "postgres:15",
		Networks: []string{"voodu0"},
		Restart:  "unless-stopped",
	}, nil)

	pre, _ := json.Marshal(DeploymentStatus{
		Image:    "postgres:15",
		SpecHash: prevHash,
		Replicas: 3,
	})

	_ = store.PutStatus(context.Background(), KindStatefulset, "test-pg", pre)

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindStatefulset, "pg", map[string]any{
		"image":    "postgres:16",
		"replicas": 3,
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Three removes total — one per ordinal, top-down.
	wantRemoves := []string{"test-pg.2", "test-pg.1", "test-pg.0"}

	if len(cm.removes) != len(wantRemoves) {
		t.Fatalf("removes = %v, want %v", cm.removes, wantRemoves)
	}

	for i, r := range cm.removes {
		if r != wantRemoves[i] {
			t.Errorf("removes[%d] = %q, want %q (drift restart order broken)", i, r, wantRemoves[i])
		}
	}

	// Three respawn ensures — same names (deterministic by
	// ordinal) but with the new image.
	if len(cm.ensures) != 3 {
		t.Fatalf("ensures = %d, want 3", len(cm.ensures))
	}

	for _, e := range cm.ensures {
		if e.Image != "postgres:16" {
			t.Errorf("post-drift ensure %q has stale image %q, want postgres:16", e.Name, e.Image)
		}
	}
}

// TestStatefulsetHandler_InjectsPodIdentityEnv pins the per-pod
// platform identity contract: every spawned pod carries a unique
// VOODU_REPLICA_ORDINAL/VOODU_REPLICA_ID plus the shared
// VOODU_SCOPE/VOODU_NAME. Plugin-authored entrypoint scripts
// (voodu-redis picks master vs replica from VOODU_REPLICA_ORDINAL
// at boot) depend on these landing on every replica, every time.
//
// Without this test, a regression that drops Env from the
// ContainerSpec would silently boot every redis pod as a master,
// breaking replication without any visible error in the
// handler log.
func TestStatefulsetHandler_InjectsPodIdentityEnv(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindStatefulset, "redis", map[string]any{
		"image":    "redis:7",
		"replicas": 3,
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(cm.ensures) != 3 {
		t.Fatalf("expected 3 ensures, got %d", len(cm.ensures))
	}

	for n, e := range cm.ensures {
		if e.Env == nil {
			t.Fatalf("ordinal %d: Env is nil — plugin entrypoints will boot blind", n)
		}

		if got := e.Env["VOODU_SCOPE"]; got != "test" {
			t.Errorf("ordinal %d: VOODU_SCOPE = %q, want %q", n, got, "test")
		}

		if got := e.Env["VOODU_NAME"]; got != "redis" {
			t.Errorf("ordinal %d: VOODU_NAME = %q, want %q", n, got, "redis")
		}

		wantOrd := containers.OrdinalReplicaID(n)

		if got := e.Env["VOODU_REPLICA_ORDINAL"]; got != wantOrd {
			t.Errorf("ordinal %d: VOODU_REPLICA_ORDINAL = %q, want %q", n, got, wantOrd)
		}

		if got := e.Env["VOODU_REPLICA_ID"]; got != wantOrd {
			t.Errorf("ordinal %d: VOODU_REPLICA_ID = %q, want %q", n, got, wantOrd)
		}
	}
}

// TestStatefulsetHandler_OperatorEnvOverridesPlatformDefaults
// pins the last-wins contract — operators can deliberately
// override platform-stamped VOODU_* keys by setting them in
// their HCL `env { ... }` block. Examples:
//
//   - point VOODU_CONTROLLER_URL at a mock for local testing
//   - alias VOODU_SCOPE for legacy app code expecting a logical
//     scope name different from voodu's
//   - override VOODU_REPLICA_ORDINAL for unusual setups where
//     the app's "ordinal" semantics differ from voodu's
//
// Cross-tenant safety isn't enforced by env (voodu's authorization
// uses manifest source + container labels), so an operator
// "spoofing" their own pod's identity just confuses their own
// application — no cross-tenant boundary is broken. The flexibility
// matches docker `-e` precedence and lets operators escape
// inconvenient platform defaults without forking the controller.
func TestStatefulsetHandler_OperatorEnvOverridesPlatformDefaults(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindStatefulset, "redis", map[string]any{
		"image":    "redis:7",
		"replicas": 1,
		"env": map[string]any{
			"VOODU_SCOPE":           "operator-alias",
			"VOODU_REPLICA_ORDINAL": "99",
			"APP_LOG_LEVEL":         "debug",
		},
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(cm.ensures) != 1 {
		t.Fatalf("expected 1 ensure, got %d", len(cm.ensures))
	}

	got := cm.ensures[0].Env

	if got["VOODU_SCOPE"] != "operator-alias" {
		t.Errorf("VOODU_SCOPE = %q, want %q (operator override must win)", got["VOODU_SCOPE"], "operator-alias")
	}

	if got["VOODU_REPLICA_ORDINAL"] != "99" {
		t.Errorf("VOODU_REPLICA_ORDINAL = %q, want %q (operator override must win)", got["VOODU_REPLICA_ORDINAL"], "99")
	}

	// Platform defaults survive on keys the operator did NOT touch.
	if got["VOODU_NAME"] != "redis" {
		t.Errorf("VOODU_NAME = %q, want %q (platform default should land when operator doesn't override)", got["VOODU_NAME"], "redis")
	}

	// Non-reserved operator env flows through as before.
	if got["APP_LOG_LEVEL"] != "debug" {
		t.Errorf("APP_LOG_LEVEL = %q, want %q (operator env got dropped)", got["APP_LOG_LEVEL"], "debug")
	}
}

// TestStatefulsetHandler_RestartsOnEnvChange pins the F2.2 fix for
// `vd redis:failover`. Failover writes REDIS_MASTER_ORDINAL via a
// config_set action; the controller's maybeRestartAffected fan-out
// re-fires the statefulset's apply, and the apply path must roll
// every pod top-down so the wrapper script picks the new role at
// boot. Without this branch (the bug Thadeu hit live), the bucket
// changes but pods stay on the old REDIS_MASTER_ORDINAL forever
// until a manual `vd restart -k statefulset` kicks them.
//
// Mirrors TestDeploymentHandler_RestartsWhenEnvChangedAndContainerExists
// — same posture, same WriteEnv-returns-true signal, same expectation
// of remove+ensure per existing pod.
func TestStatefulsetHandler_RestartsOnEnvChange(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	// Three live pods at the start. Baseline status with the same
	// spec hash the upcoming apply will compute, so the spec-drift
	// recreate path is a no-op and the test exercises the
	// env-change branch in isolation.
	for i := 0; i < 3; i++ {
		cm.seedSlot(statefulsetSlot("test", "redis", "redis:7", i))
	}

	hash := statefulsetSpecHash(statefulsetSpec{
		Image:    "redis:7",
		Networks: []string{"voodu0"},
		Restart:  "unless-stopped",
	}, nil)

	pre, _ := json.Marshal(DeploymentStatus{
		Image:    "redis:7",
		SpecHash: hash,
		Replicas: 3,
	})

	_ = store.PutStatus(context.Background(), KindStatefulset, "test-redis", pre)

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return true, nil }, // env CHANGED
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindStatefulset, "redis", map[string]any{
		"image":    "redis:7",
		"replicas": 3,
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Top-down rolling restart: pod-2 first, then pod-1, then pod-0.
	// Each pod removed and re-ensured under the same ordinal-derived
	// name so the per-pod data volume re-attaches.
	wantRemoves := []string{"test-redis.2", "test-redis.1", "test-redis.0"}

	if len(cm.removes) != len(wantRemoves) {
		t.Fatalf("removes = %v, want %v (env-change rolling restart didn't fire)",
			cm.removes, wantRemoves)
	}

	for i, r := range cm.removes {
		if r != wantRemoves[i] {
			t.Errorf("removes[%d] = %q, want %q (top-down order broken)", i, r, wantRemoves[i])
		}
	}

	// Three respawns, deterministic ordinal-derived names — proves
	// the fresh pods come up under the same identity (same volumes,
	// same DNS aliases) and not as new replica IDs.
	if len(cm.ensures) != 3 {
		t.Fatalf("ensures = %d, want 3", len(cm.ensures))
	}
}

// TestStatefulsetHandler_NoEnvChange_NoRestart confirms the gate
// fires ONLY when WriteEnv reports a real change. A reconcile that
// re-runs the same env merge (no diff) must NOT churn pods —
// otherwise every reconcile fires a phantom rolling restart.
func TestStatefulsetHandler_NoEnvChange_NoRestart(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	for i := 0; i < 3; i++ {
		cm.seedSlot(statefulsetSlot("test", "redis", "redis:7", i))
	}

	hash := statefulsetSpecHash(statefulsetSpec{
		Image:    "redis:7",
		Networks: []string{"voodu0"},
		Restart:  "unless-stopped",
	}, nil)

	pre, _ := json.Marshal(DeploymentStatus{
		Image:    "redis:7",
		SpecHash: hash,
		Replicas: 3,
	})

	_ = store.PutStatus(context.Background(), KindStatefulset, "test-redis", pre)

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil }, // unchanged
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindStatefulset, "redis", map[string]any{
		"image":    "redis:7",
		"replicas": 3,
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(cm.removes) != 0 {
		t.Errorf("no env change should not restart pods, got removes=%v", cm.removes)
	}
}

// TestStatefulsetHandler_FrozenOrdinalNotSpawned pins the
// `vd stop --freeze` contract: the operator's intent persists in
// the store as a frozen-ordinals annotation, and the apply path
// must respect it on the spawn side. Without this, the next
// reconcile after a freeze would re-spawn the pod the operator
// just stopped, undoing the intent.
//
// Test shape: pre-seed the frozen list with ordinal 1, run an
// apply against a fresh statefulset declaring 3 replicas. We
// expect ensureOrdinalsUp to skip ordinal 1 entirely — pods 0
// and 2 are spawned, pod-1 is left absent.
func TestStatefulsetHandler_FrozenOrdinalNotSpawned(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	// Persist the operator's freeze intent BEFORE apply runs.
	if err := store.SetFrozenReplicaIDs(context.Background(), KindStatefulset, "test", "redis", []string{"1"}); err != nil {
		t.Fatalf("seed frozen: %v", err)
	}

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindStatefulset, "redis", map[string]any{
		"image":    "redis:7",
		"replicas": 3,
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Two ensures (ordinals 0 and 2), zero ensure for ordinal 1.
	if len(cm.ensures) != 2 {
		t.Fatalf("expected 2 ensures (frozen ord-1 skipped), got %d: %+v", len(cm.ensures), cm.ensures)
	}

	wantNames := map[string]bool{
		"test-redis.0": true,
		"test-redis.2": true,
	}

	for _, e := range cm.ensures {
		if !wantNames[e.Name] {
			t.Errorf("unexpected ensure for %q (ordinal 1 should be frozen)", e.Name)
		}
	}
}

// TestStatefulsetHandler_FrozenOrdinalNotRestarted: even when an
// env-change rolling restart fires, frozen ordinals stay parked.
// Otherwise a `vd config set` (or any fan-out trigger) would
// undo the freeze the operator's `vd stop --freeze` just set.
func TestStatefulsetHandler_FrozenOrdinalNotRestarted(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	// Three live pods, one of them frozen (its container can be
	// stopped or running; the test cares about the freeze gate,
	// not the docker state).
	for i := 0; i < 3; i++ {
		cm.seedSlot(statefulsetSlot("test", "redis", "redis:7", i))
	}

	// Mark ordinal 2 as frozen.
	if err := store.SetFrozenReplicaIDs(context.Background(), KindStatefulset, "test", "redis", []string{"2"}); err != nil {
		t.Fatalf("seed frozen: %v", err)
	}

	// Status that matches the upcoming spec hash so spec-drift
	// recreate is a no-op — we want to exercise the env-change
	// branch.
	hash := statefulsetSpecHash(statefulsetSpec{
		Image:    "redis:7",
		Networks: []string{"voodu0"},
		Restart:  "unless-stopped",
	}, nil)

	pre, _ := json.Marshal(DeploymentStatus{
		Image:    "redis:7",
		SpecHash: hash,
		Replicas: 3,
	})

	_ = store.PutStatus(context.Background(), KindStatefulset, "test-redis", pre)

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return true, nil }, // env CHANGED
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindStatefulset, "redis", map[string]any{
		"image":    "redis:7",
		"replicas": 3,
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Top-down rolling restart over ordinals 0 and 1; ordinal 2
	// stays parked. Removes are 1 and 0 (top-down skipping 2).
	wantRemoves := []string{"test-redis.1", "test-redis.0"}

	if len(cm.removes) != len(wantRemoves) {
		t.Fatalf("removes = %v, want %v (frozen ord-2 should be skipped)",
			cm.removes, wantRemoves)
	}

	for i, r := range cm.removes {
		if r != wantRemoves[i] {
			t.Errorf("removes[%d] = %q, want %q", i, r, wantRemoves[i])
		}
	}

	// Confirm pod-2 was NEVER touched.
	for _, r := range cm.removes {
		if r == "test-redis.2" {
			t.Errorf("frozen pod-2 was removed: %v", cm.removes)
		}
	}
}

// TestStatefulsetHandler_FirstApply_NoEnvRestartChurn guards against
// the first-reconcile-after-controller-upgrade case. Without prior
// status, resolveAppEnv reports envChanged=true (the controller
// didn't track it before, so the merge looks new), and we'd cycle
// every freshly-spawned pod for nothing. The firstApply gate skips
// the restart when there's no baseline to drift from.
func TestStatefulsetHandler_FirstApply_NoEnvRestartChurn(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	// No prior status → firstApply=true. Pods get spawned by
	// ensureOrdinalsUp. The env-change branch must NOT fire on
	// top of that spawn.
	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return true, nil }, // would-be "changed"
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	ev := putEvent(t, KindStatefulset, "redis", map[string]any{
		"image":    "redis:7",
		"replicas": 2,
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Two ensures from ensureOrdinalsUp (pods 0 and 1). Zero
	// removes — the env-change path didn't double-cycle them.
	if len(cm.ensures) != 2 {
		t.Errorf("expected 2 first-apply ensures, got %d", len(cm.ensures))
	}

	if len(cm.removes) != 0 {
		t.Errorf("first apply must not env-restart freshly-spawned pods, got removes=%v", cm.removes)
	}
}

// TestStatefulsetHandler_EnvFromStacksAcrossOrdinals pins the
// env_from feature on statefulsets — symmetric to JobHandler's
// stacking. Every ordinal in the same reconcile gets the SAME
// ExtraEnvFiles slice; resolution happens once per reconcile,
// reused for each spawn.
//
// The canonical use case is a sentinel resource inheriting the
// data redis's REDIS_PASSWORD + REDIS_MASTER_ORDINAL via
// env_from = ["scope/redis"]. This test pins the underlying
// machinery — voodu-redis's expand emits this declaration
// automatically; the controller-side wiring is generic.
func TestStatefulsetHandler_EnvFromStacksAcrossOrdinals(t *testing.T) {
	withZeroRolloutPause(t)

	tmp := t.TempDir()
	t.Setenv("VOODU_ROOT", tmp)

	// Pre-seed env files on disk so the resolver finds them.
	// Format mirrors mustTouch from env_from_test.go.
	mustTouch(t, filepath.Join(tmp, "apps", "test-redis", "shared", ".env"))

	store := newMemStore()
	cm := &fakeContainers{}

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	spec := map[string]any{
		"image":    "redis:8",
		"replicas": 3,
		"env_from": []string{"redis"}, // current scope (test) → test-redis
	}

	if err := h.Handle(context.Background(), putEvent(t, KindStatefulset, "redis-ha", spec)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(cm.ensures) != 3 {
		t.Fatalf("expected 3 ordinal ensures, got %d", len(cm.ensures))
	}

	// Every ordinal got the same env_from resolution — without
	// per-pod variance. If we accidentally re-resolved in a way
	// that depended on ordinal, the slices would diverge.
	for n, e := range cm.ensures {
		if len(e.ExtraEnvFiles) != 1 {
			t.Errorf("ordinal %d: ExtraEnvFiles = %v, want 1 entry", n, e.ExtraEnvFiles)
			continue
		}

		if !strings.HasSuffix(e.ExtraEnvFiles[0], "/test-redis/shared/.env") {
			t.Errorf("ordinal %d: ExtraEnvFiles[0] = %q, want ends with /test-redis/shared/.env",
				n, e.ExtraEnvFiles[0])
		}
	}
}

// TestStatefulsetHandler_NoEnvFromIsEmpty pins the negative
// path — statefulsets without env_from get nil ExtraEnvFiles
// (or empty), no spurious --env-file flags. Confirms the
// feature is opt-in and doesn't change behaviour for existing
// statefulsets.
func TestStatefulsetHandler_NoEnvFromIsEmpty(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
	}

	spec := map[string]any{
		"image":    "redis:8",
		"replicas": 2,
	}

	if err := h.Handle(context.Background(), putEvent(t, KindStatefulset, "redis", spec)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	for n, e := range cm.ensures {
		if len(e.ExtraEnvFiles) != 0 {
			t.Errorf("ordinal %d: expected no ExtraEnvFiles, got %v", n, e.ExtraEnvFiles)
		}
	}
}

// TestStatefulsetHandler_AutoInjectsControllerURL pins the
// VOODU_CONTROLLER_URL auto-injection — when the handler has
// ControllerURL set (production wiring), every spawned pod
// gets the env var without operator declaration. This is the
// other half of "plugin resolves everything" — sentinel's
// failover hook + preflight need this URL to call back, and
// the operator should never have to set it manually.
func TestStatefulsetHandler_AutoInjectsControllerURL(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	h := &StatefulsetHandler{
		Store:         store,
		Log:           quietLogger(),
		WriteEnv:      func(string, []string) (bool, error) { return false, nil },
		EnvFilePath:   func(app string) string { return "/tmp/" + app + ".env" },
		Containers:    cm,
		ControllerURL: "http://controller.voodu:8686",
	}

	spec := map[string]any{
		"image":    "redis:8",
		"replicas": 1,
	}

	if err := h.Handle(context.Background(), putEvent(t, KindStatefulset, "redis", spec)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(cm.ensures) != 1 {
		t.Fatalf("expected 1 ensure, got %d", len(cm.ensures))
	}

	got := cm.ensures[0].Env[EnvControllerURL]
	if got != "http://controller.voodu:8686" {
		t.Errorf("VOODU_CONTROLLER_URL = %q, want auto-injected to %q",
			got, "http://controller.voodu:8686")
	}
}

// TestStatefulsetHandler_NoControllerURLLeavesEnvUnset is the
// inverse — when ControllerURL is empty (test setup, no HTTP
// plumbing), VOODU_CONTROLLER_URL must NOT appear in pod env.
// Otherwise we'd leak an empty string that shells / scripts
// might mistake for a configured-but-empty value.
func TestStatefulsetHandler_NoControllerURLLeavesEnvUnset(t *testing.T) {
	withZeroRolloutPause(t)

	store := newMemStore()
	cm := &fakeContainers{}

	h := &StatefulsetHandler{
		Store:       store,
		Log:         quietLogger(),
		WriteEnv:    func(string, []string) (bool, error) { return false, nil },
		EnvFilePath: func(app string) string { return "/tmp/" + app + ".env" },
		Containers:  cm,
		// ControllerURL deliberately unset.
	}

	if err := h.Handle(context.Background(), putEvent(t, KindStatefulset, "redis",
		map[string]any{"image": "redis:8", "replicas": 1})); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if _, present := cm.ensures[0].Env[EnvControllerURL]; present {
		t.Errorf("VOODU_CONTROLLER_URL must NOT appear when handler.ControllerURL is empty (would leak empty string)")
	}
}

