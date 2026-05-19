// Tests for DeploymentHandler's ReadinessRecorder seam:
// RecordReplicaReadiness writes per-replica readiness to the
// status blob without clobbering other fields (Releases,
// InitFailures, SpecHash). ClearReplicaReadiness drops one entry.
// truncateReplicaReadiness enforces the LRU cap.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestDeploymentHandler_RecordReplicaReadiness_PreservesReleases
// is the load-modify-write invariant pin: recording readiness
// for one replica must not erase the Releases history that
// lives on the same blob. The original M1.1 InitFailures
// helpers have the same property — we re-test here because
// each new field on DeploymentStatus is an independent
// opportunity to forget the merge.
func TestDeploymentHandler_RecordReplicaReadiness_PreservesReleases(t *testing.T) {
	store := newMemStore()

	h := &DeploymentHandler{
		Store: store,
		Log:   quietLogger(),
	}

	// Pre-seed the status blob with a Releases entry and an
	// InitFailures entry to assert both survive.
	pre, _ := json.Marshal(DeploymentStatus{
		Image:    "nginx:1.25",
		SpecHash: "abc",
		Releases: []ReleaseRecord{{ID: "r1", Status: ReleaseStatusSucceeded}},
		InitFailures: []InitFailure{
			{ReplicaID: "old", InitName: "migrate", ExitCode: 1, Attempts: 1, RecordedAt: time.Now().UTC()},
		},
	})

	_ = store.PutStatus(context.Background(), KindDeployment, "test-api", pre)

	h.RecordReplicaReadiness(context.Background(), "test-api", ReplicaReadinessStatus{
		ContainerName:  "test-api.a3f9",
		ReplicaID:      "a3f9",
		Ready:          true,
		StartupPassed:  true,
		LastTransition: time.Now().UTC(),
	})

	raw, _ := store.GetStatus(context.Background(), KindDeployment, "test-api")

	var got DeploymentStatus

	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if got.Image != "nginx:1.25" || got.SpecHash != "abc" {
		t.Errorf("Image/SpecHash clobbered: %+v", got)
	}

	if len(got.Releases) != 1 || got.Releases[0].ID != "r1" {
		t.Errorf("Releases history lost: %+v", got.Releases)
	}

	if len(got.InitFailures) != 1 || got.InitFailures[0].InitName != "migrate" {
		t.Errorf("InitFailures lost: %+v", got.InitFailures)
	}

	if r, ok := got.ReplicaReadiness["test-api.a3f9"]; !ok || !r.Ready {
		t.Errorf("ReplicaReadiness entry missing or wrong: %+v", got.ReplicaReadiness)
	}
}

// TestDeploymentHandler_ClearReplicaReadiness_NoOpOnMissing pins
// the defensive shape: clearing an entry that's not present
// shouldn't trigger an unnecessary etcd write. The original
// memstore counts puts so a regression would show up as a
// spurious extra write.
func TestDeploymentHandler_ClearReplicaReadiness_NoOpOnMissing(t *testing.T) {
	store := newMemStore()

	h := &DeploymentHandler{
		Store: store,
		Log:   quietLogger(),
	}

	// No pre-existing status — first Clear is a clean no-op (no
	// panic, no error). We can't easily count puts on memStore,
	// but we can assert the blob remains absent.
	h.ClearReplicaReadiness(context.Background(), "test-api", "ghost")

	raw, _ := store.GetStatus(context.Background(), KindDeployment, "test-api")
	if raw != nil {
		t.Errorf("Clear on missing should not synthesise a blob, got %d bytes", len(raw))
	}
}

// TestDeploymentHandler_ClearReplicaReadiness_DropsOneEntry
// verifies that Clear surgically removes the named entry and
// leaves the others alone — important so caddy doesn't see the
// whole map vanish when one replica is torn down.
func TestDeploymentHandler_ClearReplicaReadiness_DropsOneEntry(t *testing.T) {
	store := newMemStore()

	pre, _ := json.Marshal(DeploymentStatus{
		ReplicaReadiness: map[string]ReplicaReadinessStatus{
			"test-api.a1": {ContainerName: "test-api.a1", Ready: true},
			"test-api.b2": {ContainerName: "test-api.b2", Ready: false},
		},
	})

	_ = store.PutStatus(context.Background(), KindDeployment, "test-api", pre)

	h := &DeploymentHandler{
		Store: store,
		Log:   quietLogger(),
	}

	h.ClearReplicaReadiness(context.Background(), "test-api", "test-api.a1")

	raw, _ := store.GetStatus(context.Background(), KindDeployment, "test-api")

	var got DeploymentStatus

	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if _, gone := got.ReplicaReadiness["test-api.a1"]; gone {
		t.Errorf("Clear did not remove the named entry: %+v", got.ReplicaReadiness)
	}

	if _, kept := got.ReplicaReadiness["test-api.b2"]; !kept {
		t.Errorf("Clear removed the wrong entry: %+v", got.ReplicaReadiness)
	}
}

// TestTruncateReplicaReadiness_CapEvictsOldest pins the LRU
// behavior: once the map crosses maxReplicaReadiness, the entry
// with the OLDEST LastTransition is dropped. A chronically
// broken fleet would otherwise grow the status blob without
// bound.
func TestTruncateReplicaReadiness_CapEvictsOldest(t *testing.T) {
	m := make(map[string]ReplicaReadinessStatus)

	base := time.Now().UTC()

	// Fill exactly the cap.
	for i := 0; i < maxReplicaReadiness; i++ {
		name := fmt.Sprintf("test-api.r%d", i)
		m[name] = ReplicaReadinessStatus{
			ContainerName:  name,
			LastTransition: base.Add(time.Duration(i) * time.Second),
		}
	}

	// At the cap → no eviction yet.
	m = truncateReplicaReadiness(m)
	if len(m) != maxReplicaReadiness {
		t.Fatalf("at cap, len=%d, want %d", len(m), maxReplicaReadiness)
	}

	// One over → oldest (r0) goes.
	newName := "test-api.fresh"
	m[newName] = ReplicaReadinessStatus{
		ContainerName:  newName,
		LastTransition: base.Add(1000 * time.Second),
	}

	m = truncateReplicaReadiness(m)

	if len(m) != maxReplicaReadiness {
		t.Errorf("after truncate, len=%d, want %d", len(m), maxReplicaReadiness)
	}

	if _, present := m["test-api.r0"]; present {
		t.Error("oldest entry r0 should have been evicted")
	}

	if _, present := m[newName]; !present {
		t.Error("newest entry must survive eviction")
	}
}

// TestDeploymentHandler_RecordReplicaReadiness_EmptyMapAllocates
// covers the cold-start path: first write must allocate the map
// (json.Unmarshal of an empty blob leaves it nil). Without this
// the assignment would panic.
func TestDeploymentHandler_RecordReplicaReadiness_EmptyMapAllocates(t *testing.T) {
	store := newMemStore()

	h := &DeploymentHandler{
		Store: store,
		Log:   quietLogger(),
	}

	// No pre-existing status at all.
	h.RecordReplicaReadiness(context.Background(), "fresh-app", ReplicaReadinessStatus{
		ContainerName: "fresh-app.x",
		Ready:         true,
	})

	raw, _ := store.GetStatus(context.Background(), KindDeployment, "fresh-app")
	if raw == nil {
		t.Fatal("first Record should create the status blob")
	}

	var got DeploymentStatus

	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if got.ReplicaReadiness == nil || len(got.ReplicaReadiness) != 1 {
		t.Errorf("expected 1 entry in fresh map, got %+v", got.ReplicaReadiness)
	}
}
