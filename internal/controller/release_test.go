package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestReleaseTimeoutFallsBackToDefault traps the parser: an empty
// or malformed timeout must NOT block the rollout — fallback to
// the package default (10m). Without this, a typo in the manifest
// could make releases run forever or fail instantly.
func TestReleaseTimeoutFallsBackToDefault(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty", "", defaultReleaseTimeout},
		{"malformed", "five-minutes", defaultReleaseTimeout},
		{"negative", "-5m", defaultReleaseTimeout},
		{"valid 30s", "30s", 30 * time.Second},
		{"valid 1h", "1h", time.Hour},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := releaseTimeout(c.in)
			if got != c.want {
				t.Errorf("releaseTimeout(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestReleaseContainerName locks in the naming convention so a
// later refactor doesn't accidentally collide release containers
// with deployment replicas. Operator-visible: this name shows up in
// `vd get pods` and is what `vd logs scope/<name>-release` matches.
func TestReleaseContainerName(t *testing.T) {
	got := releaseContainerName("clowk-lp", "web", "abcd")

	for _, want := range []string{"clowk-lp", "web-release", "abcd"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in container name %q", want, got)
		}
	}
}

// TestNewReleaseIDIsSortable confirms the ID minting contract:
// IDs generated later sort lexically AFTER earlier ones. Without
// this the history list would need to track creation timestamps
// separately just to render in chronological order.
func TestNewReleaseIDIsSortable(t *testing.T) {
	first := newReleaseID()

	// Sleep just past the next-second boundary so the base36 timestamp
	// portion changes. Sub-second collisions fall back on the random
	// suffix for uniqueness, but the SORT contract holds at the
	// second granularity (which is all `vd release history` cares
	// about — operators never trigger releases millisecond-apart).
	time.Sleep(1100 * time.Millisecond)

	second := newReleaseID()

	if first >= second {
		t.Errorf("expected first %q < second %q (sortable)", first, second)
	}

	if first == second {
		t.Errorf("two IDs collided: %q", first)
	}
}

// TestRelease_RollbackToExplicitID covers the canonical
// `vd rollback web <release_id>` flow: operator picks a specific
// release ID, server re-Puts that release's snapshot. New release
// record is created with rolled_back_from=target.ID.
func TestRelease_RollbackToExplicitID(t *testing.T) {
	store := newMemStore()

	currentSpec := json.RawMessage(`{"image":"v3","replicas":1}`)
	v2Spec := json.RawMessage(`{"image":"v2","replicas":1}`)

	_, _ = store.Put(context.Background(), &Manifest{
		Kind: KindDeployment, Scope: "test", Name: "web", Spec: currentSpec,
	})

	statusBlob, _ := json.Marshal(DeploymentStatus{
		Releases: []ReleaseRecord{
			{ID: "rel3", Status: ReleaseStatusSucceeded, SpecHash: "h3", SpecSnapshot: currentSpec},
			{ID: "rel2", Status: ReleaseStatusSucceeded, SpecHash: "h2", SpecSnapshot: v2Spec},
			{ID: "rel1", Status: ReleaseStatusSucceeded, SpecHash: "h1"},
		},
	})

	_ = store.PutStatus(context.Background(), KindDeployment, "test-web", statusBlob)

	h := &DeploymentHandler{Store: store, Log: quietLogger()}

	newID, err := h.Rollback(context.Background(), "test", "web", "rel2")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if newID == "" {
		t.Error("rollback returned empty new release ID")
	}

	if newID == "rel2" {
		t.Errorf("rollback should mint a NEW id, not reuse %q", newID)
	}

	got, _ := store.Get(context.Background(), KindDeployment, "test", "web")

	if string(got.Spec) != string(v2Spec) {
		t.Errorf("spec after rollback: got %s, want %s", got.Spec, v2Spec)
	}

	// Verify the new release record was appended with rolled_back_from.
	st, _ := h.loadDeploymentStatus(context.Background(), "test-web")

	if len(st.Releases) == 0 {
		t.Fatal("history empty after rollback")
	}

	newest := st.Releases[0]
	if newest.ID != newID {
		t.Errorf("newest release ID: got %q, want %q", newest.ID, newID)
	}

	if newest.RolledBackFrom != "rel2" {
		t.Errorf("rolled_back_from: got %q, want %q", newest.RolledBackFrom, "rel2")
	}
}

// TestRelease_RollbackToEmptyPicksPreviousSucceeded confirms the
// "no ID specified" default: rollback to the second-most-recent
// succeeded release (the version before the current one),
// Heroku-style.
func TestRelease_RollbackToEmptyPicksPreviousSucceeded(t *testing.T) {
	store := newMemStore()

	v3Spec := json.RawMessage(`{"image":"v3"}`)
	v2Spec := json.RawMessage(`{"image":"v2"}`)

	_, _ = store.Put(context.Background(), &Manifest{
		Kind: KindDeployment, Scope: "test", Name: "web", Spec: v3Spec,
	})

	statusBlob, _ := json.Marshal(DeploymentStatus{
		Releases: []ReleaseRecord{
			{ID: "rel3", Status: ReleaseStatusSucceeded, SpecHash: "h3", SpecSnapshot: v3Spec},
			{ID: "rel2", Status: ReleaseStatusSucceeded, SpecHash: "h2", SpecSnapshot: v2Spec},
		},
	})

	_ = store.PutStatus(context.Background(), KindDeployment, "test-web", statusBlob)

	h := &DeploymentHandler{Store: store, Log: quietLogger()}

	if _, err := h.Rollback(context.Background(), "test", "web", ""); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	got, _ := store.Get(context.Background(), KindDeployment, "test", "web")

	if string(got.Spec) != string(v2Spec) {
		t.Errorf("spec after rollback: got %s, want %s", got.Spec, v2Spec)
	}
}

// TestRelease_RollbackRefusesFailedTarget protects against rolling
// back to a known-broken release: even if the operator explicitly
// picks rel4, if rel4 was Failed, error out. Don't silently roll
// forward to a broken state.
func TestRelease_RollbackRefusesFailedTarget(t *testing.T) {
	store := newMemStore()

	_, _ = store.Put(context.Background(), &Manifest{
		Kind: KindDeployment, Scope: "test", Name: "web",
	})

	statusBlob, _ := json.Marshal(DeploymentStatus{
		Releases: []ReleaseRecord{
			{ID: "rel4", Status: ReleaseStatusFailed, SpecHash: "h4"},
		},
	})

	_ = store.PutStatus(context.Background(), KindDeployment, "test-web", statusBlob)

	h := &DeploymentHandler{Store: store, Log: quietLogger()}

	_, err := h.Rollback(context.Background(), "test", "web", "rel4")
	if err == nil {
		t.Fatal("expected error when rolling back to a Failed release")
	}

	if !strings.Contains(err.Error(), "succeeded") {
		t.Errorf("error should mention succeeded requirement: %q", err.Error())
	}
}

// TestRelease_RollbackErrorsOnUnknownID guards the typo-friendly
// behaviour: explicit ID that doesn't exist in history → clear
// "not found" error instead of silently picking the closest one.
func TestRelease_RollbackErrorsOnUnknownID(t *testing.T) {
	store := newMemStore()

	_, _ = store.Put(context.Background(), &Manifest{
		Kind: KindDeployment, Scope: "test", Name: "web",
	})

	statusBlob, _ := json.Marshal(DeploymentStatus{
		Releases: []ReleaseRecord{
			{ID: "rel1", Status: ReleaseStatusSucceeded, SpecHash: "h1"},
		},
	})

	_ = store.PutStatus(context.Background(), KindDeployment, "test-web", statusBlob)

	h := &DeploymentHandler{Store: store, Log: quietLogger()}

	_, err := h.Rollback(context.Background(), "test", "web", "nope-id")
	if err == nil {
		t.Fatal("expected error for unknown release id")
	}

	if !strings.Contains(err.Error(), "nope-id") {
		t.Errorf("error should mention release nope-id: %q", err.Error())
	}
}

// TestRelease_AppendRecordCapsHistory protects the cap so a
// long-lived deployment doesn't accumulate hundreds of release
// records in etcd over time. After maxReleaseHistory entries, the
// oldest drops off.
func TestRelease_AppendRecordCapsHistory(t *testing.T) {
	store := newMemStore()

	_, _ = store.Put(context.Background(), &Manifest{
		Kind: KindDeployment, Scope: "test", Name: "web",
	})

	h := &DeploymentHandler{Store: store, Log: quietLogger()}

	for i := 0; i < maxReleaseHistory+5; i++ {
		r := ReleaseRecord{
			ID:        "r" + string(rune('0'+i%10)),
			Status:    ReleaseStatusSucceeded,
			SpecHash:  "h",
			StartedAt: time.Now(),
		}

		_ = h.appendReleaseRecord(context.Background(), "test-web", r)
	}

	st, _ := h.loadDeploymentStatus(context.Background(), "test-web")

	if len(st.Releases) != maxReleaseHistory {
		t.Errorf("history length = %d, want %d (cap)", len(st.Releases), maxReleaseHistory)
	}
}
