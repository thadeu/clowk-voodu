package controller

import (
	"testing"
	"time"
)

// TestMemStore_PAT_CRUD covers the full lifecycle of a PAT through
// the in-memory store. Mirrors what the etcd impl does — if the
// shape diverges, /pats handlers behave differently in tests vs
// production. Locking it in here catches that drift.
func TestMemStore_PAT_CRUD(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()

	// 1. List on empty store returns empty slice, no error.
	got, err := store.ListPATs(ctx)
	if err != nil {
		t.Fatalf("ListPATs (empty): %v", err)
	}

	if len(got) != 0 {
		t.Errorf("ListPATs (empty): got %d entries, want 0", len(got))
	}

	// 2. Get on missing ID returns (nil, nil) — the "not found"
	//    contract the auth middleware relies on.
	missing, err := store.GetPAT(ctx, "doesnotexist")
	if err != nil {
		t.Fatalf("GetPAT (missing): %v", err)
	}

	if missing != nil {
		t.Errorf("GetPAT (missing): got %+v, want nil", missing)
	}

	// 3. Put + Get round-trip preserves all fields.
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	original := PAT{
		ID:        "ABCD1234",
		HashHex:   "deadbeefcafe",
		Scopes:    []Scope{ScopeRead, ScopeActions},
		Name:      "webui-staging",
		CreatedAt: now,
	}

	if err := store.PutPAT(ctx, original); err != nil {
		t.Fatalf("PutPAT: %v", err)
	}

	roundTripped, err := store.GetPAT(ctx, "ABCD1234")
	if err != nil {
		t.Fatalf("GetPAT after Put: %v", err)
	}

	if roundTripped == nil {
		t.Fatal("GetPAT after Put: got nil, want record")
	}

	if roundTripped.ID != original.ID || roundTripped.Name != original.Name {
		t.Errorf("round-trip lost identity: got %+v, want %+v", *roundTripped, original)
	}

	if len(roundTripped.Scopes) != 2 {
		t.Errorf("round-trip dropped scopes: %v", roundTripped.Scopes)
	}

	if roundTripped.HashHex != "deadbeefcafe" {
		t.Errorf("round-trip mangled HashHex: %q", roundTripped.HashHex)
	}

	// 4. List with one entry.
	all, err := store.ListPATs(ctx)
	if err != nil {
		t.Fatalf("ListPATs: %v", err)
	}

	if len(all) != 1 {
		t.Fatalf("ListPATs after 1 Put: got %d, want 1", len(all))
	}

	// 5. Delete returns true; subsequent delete returns false.
	deleted, err := store.DeletePAT(ctx, "ABCD1234")
	if err != nil {
		t.Fatalf("DeletePAT: %v", err)
	}

	if !deleted {
		t.Errorf("DeletePAT first call: got false, want true")
	}

	deleted2, err := store.DeletePAT(ctx, "ABCD1234")
	if err != nil {
		t.Fatalf("DeletePAT second call: %v", err)
	}

	if deleted2 {
		t.Errorf("DeletePAT after delete: got true, want false (idempotent)")
	}

	// 6. Get after Delete returns nil.
	after, err := store.GetPAT(ctx, "ABCD1234")
	if err != nil {
		t.Fatalf("GetPAT after delete: %v", err)
	}

	if after != nil {
		t.Errorf("GetPAT after delete: got %+v, want nil", after)
	}
}

// TestMemStore_PAT_DefensiveCopy pins that mutating a returned PAT
// pointer doesn't mutate the stored record. The etcd impl is
// naturally immune (round-trips through JSON), but the in-memory
// store could leak shared pointers — leading to confusing test
// failures where modifying a result silently changes future Gets.
func TestMemStore_PAT_DefensiveCopy(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()

	original := PAT{
		ID:      "FFFF0000",
		HashHex: "abc",
		Scopes:  []Scope{ScopeRead},
		Name:    "original",
	}

	_ = store.PutPAT(ctx, original)

	got, _ := store.GetPAT(ctx, "FFFF0000")
	got.Name = "mutated"
	got.HashHex = "tampered"

	fresh, _ := store.GetPAT(ctx, "FFFF0000")

	if fresh.Name != "original" {
		t.Errorf("store leaked pointer — caller mutation changed stored Name: %q", fresh.Name)
	}

	if fresh.HashHex != "abc" {
		t.Errorf("store leaked pointer — caller mutation changed stored HashHex: %q", fresh.HashHex)
	}
}

// TestMemStore_PAT_Touch pins LastUsedAt update — best-effort,
// but must work when the PAT exists. A regression here would
// make `vd pat list` always show "never used" even for active
// tokens.
func TestMemStore_PAT_Touch(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()

	created := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	_ = store.PutPAT(ctx, PAT{ID: "TOUCH001", CreatedAt: created})

	// Before touch — LastUsedAt is zero.
	pre, _ := store.GetPAT(ctx, "TOUCH001")
	if !pre.LastUsedAt.IsZero() {
		t.Errorf("LastUsedAt should be zero pre-touch, got %v", pre.LastUsedAt)
	}

	// Touch.
	touchAt := time.Date(2026, 5, 22, 12, 30, 0, 0, time.UTC)
	if err := store.TouchPAT(ctx, "TOUCH001", touchAt); err != nil {
		t.Fatalf("TouchPAT: %v", err)
	}

	// After touch — LastUsedAt is set.
	post, _ := store.GetPAT(ctx, "TOUCH001")
	if !post.LastUsedAt.Equal(touchAt) {
		t.Errorf("LastUsedAt: got %v, want %v", post.LastUsedAt, touchAt)
	}

	// CreatedAt didn't get clobbered.
	if !post.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt clobbered by Touch: got %v, want %v", post.CreatedAt, created)
	}
}

// TestMemStore_PAT_Touch_MissingIDNoOp pins that touching a
// nonexistent PAT is a silent no-op (not an error). The middleware
// calls Touch best-effort; if the PAT was revoked between the
// auth lookup and the touch call, the touch must not bubble an
// error that would mask the actual response.
func TestMemStore_PAT_Touch_MissingIDNoOp(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()

	if err := store.TouchPAT(ctx, "GHOST", time.Now()); err != nil {
		t.Errorf("TouchPAT on missing ID should be no-op, got err: %v", err)
	}
}

// TestMemStore_PAT_ListReturnsAll pins ListPATs surfaces every
// stored record. `vd pat list` depends on this; partial results
// would confuse audit / cleanup workflows.
func TestMemStore_PAT_ListReturnsAll(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()

	for _, id := range []string{"AAAA0001", "BBBB0002", "CCCC0003"} {
		_ = store.PutPAT(ctx, PAT{ID: id, Scopes: []Scope{ScopeRead}})
	}

	got, err := store.ListPATs(ctx)
	if err != nil {
		t.Fatalf("ListPATs: %v", err)
	}

	if len(got) != 3 {
		t.Errorf("ListPATs: got %d entries, want 3", len(got))
	}

	seen := map[string]bool{}
	for _, p := range got {
		seen[p.ID] = true
	}

	for _, want := range []string{"AAAA0001", "BBBB0002", "CCCC0003"} {
		if !seen[want] {
			t.Errorf("ListPATs missing %q from results", want)
		}
	}
}

// TestMemStore_PAT_PutEmptyIDRejected pins the input-guard contract.
// The etcd impl rejects empty IDs because they would yield the key
// `/pats/` (which conflicts with the listing prefix); the in-memory
// store mirrors that — silently dropping the put is fine (matches
// the no-op-on-bad-input pattern other store methods follow).
func TestMemStore_PAT_PutEmptyIDRejected(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()

	_ = store.PutPAT(ctx, PAT{ID: "", Name: "empty"})

	all, _ := store.ListPATs(ctx)
	if len(all) != 0 {
		t.Errorf("Put with empty ID should not store, got %d entries", len(all))
	}
}
