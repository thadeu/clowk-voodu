// Tests for the M1.3 statefulset probe wiring — verifies that
// Probes hook lands on per-ordinal Ensure / Remove and that the
// status blob's ReplicaReadiness map is correctly maintained by
// the statefulset-side Recorder methods.

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestStatefulsetHandler_RecordReplicaReadiness_PreservesReleases
// is the load-modify-write invariant pin for the statefulset
// blob — same shape DeploymentHandler tests assert, but scoped
// to KindStatefulset so we know the persistence path doesn't
// silently fall back to the deployment kind.
func TestStatefulsetHandler_RecordReplicaReadiness_PreservesReleases(t *testing.T) {
	store := newMemStore()

	h := &StatefulsetHandler{
		Store: store,
		Log:   quietLogger(),
	}

	pre, _ := json.Marshal(DeploymentStatus{
		Image:    "postgres:16",
		SpecHash: "abc",
		Releases: []ReleaseRecord{{ID: "r1", Status: ReleaseStatusSucceeded}},
	})

	_ = store.PutStatus(context.Background(), KindStatefulset, "data-pg", pre)

	h.RecordReplicaReadiness(context.Background(), "data-pg", ReplicaReadinessStatus{
		ContainerName:  "data-pg.0",
		ReplicaID:      "0",
		Ready:          true,
		StartupPassed:  true,
		LastTransition: time.Now().UTC(),
	})

	raw, _ := store.GetStatus(context.Background(), KindStatefulset, "data-pg")

	var got DeploymentStatus

	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if got.Image != "postgres:16" || got.SpecHash != "abc" {
		t.Errorf("Image/SpecHash clobbered on statefulset write: %+v", got)
	}

	if len(got.Releases) != 1 || got.Releases[0].ID != "r1" {
		t.Errorf("Releases history lost on statefulset write: %+v", got.Releases)
	}

	if r, ok := got.ReplicaReadiness["data-pg.0"]; !ok || !r.Ready {
		t.Errorf("ReplicaReadiness entry missing or wrong: %+v", got.ReplicaReadiness)
	}
}

// TestStatefulsetHandler_ClearReplicaReadiness_DropsOneEntry
// pins the surgical-removal contract — same as the deployment
// counterpart but writing to the statefulset blob.
func TestStatefulsetHandler_ClearReplicaReadiness_DropsOneEntry(t *testing.T) {
	store := newMemStore()

	pre, _ := json.Marshal(DeploymentStatus{
		ReplicaReadiness: map[string]ReplicaReadinessStatus{
			"data-pg.0": {ContainerName: "data-pg.0", Ready: true},
			"data-pg.1": {ContainerName: "data-pg.1", Ready: false},
		},
	})

	_ = store.PutStatus(context.Background(), KindStatefulset, "data-pg", pre)

	h := &StatefulsetHandler{
		Store: store,
		Log:   quietLogger(),
	}

	h.ClearReplicaReadiness(context.Background(), "data-pg", "data-pg.0")

	raw, _ := store.GetStatus(context.Background(), KindStatefulset, "data-pg")

	var got DeploymentStatus

	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if _, gone := got.ReplicaReadiness["data-pg.0"]; gone {
		t.Errorf("Clear did not remove the named entry: %+v", got.ReplicaReadiness)
	}

	if _, kept := got.ReplicaReadiness["data-pg.1"]; !kept {
		t.Errorf("Clear removed the wrong entry: %+v", got.ReplicaReadiness)
	}
}

// TestCompositeReadinessLookup_FirstHitWins pins the composite
// lookup behaviour: when multiple registries are chained, the
// first one with an entry for the queried container name wins.
// In practice docker enforces host-wide name uniqueness so two
// registries never carry the same key — but the deterministic
// order matters for tests and for the "second registry queried
// only if first misses" contract.
func TestCompositeReadinessLookup_FirstHitWins(t *testing.T) {
	r1 := &ProbeRegistry{}
	r1.runners.Store("foo", &runnerEntry{state: &replicaReadiness{
		startupPassed: true,
	}})

	r2 := &ProbeRegistry{}
	r2.runners.Store("bar", &runnerEntry{state: &replicaReadiness{
		startupPassed: true,
	}})

	composite := compositeReadinessLookup{r1, r2}

	// First registry has "foo"
	if s, ok := composite.LookupReadiness("foo"); !ok || s.ContainerName != "foo" {
		t.Errorf("foo: expected hit in r1, got ok=%v name=%q", ok, s.ContainerName)
	}

	// Falls through to second registry for "bar"
	if s, ok := composite.LookupReadiness("bar"); !ok || s.ContainerName != "bar" {
		t.Errorf("bar: expected hit in r2, got ok=%v name=%q", ok, s.ContainerName)
	}

	// Neither registry has "ghost"
	if _, ok := composite.LookupReadiness("ghost"); ok {
		t.Error("ghost: expected miss")
	}
}

// TestCompositeReadinessLookup_NilSafe verifies that the
// composite tolerates nil entries — a controller wired with
// only one kind's probe registry shouldn't crash.
func TestCompositeReadinessLookup_NilSafe(t *testing.T) {
	r := &ProbeRegistry{}
	r.runners.Store("foo", &runnerEntry{state: &replicaReadiness{startupPassed: true}})

	composite := compositeReadinessLookup{nil, r, nil}

	if _, ok := composite.LookupReadiness("foo"); !ok {
		t.Error("nil entries should be skipped, not abort the lookup")
	}
}
