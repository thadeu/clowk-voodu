// pat_store.go owns the etcd-backed CRUD for PAT records. Pairs
// with pat.go (which owns the pure data shape) and the in-memory
// implementation in memstore_test.go (which mirrors this for unit
// tests).
//
// One record per PAT under `/pats/<id>`, JSON-encoded. The ID is
// the public 8-char token prefix; collisions are negligible at
// 40 bits of entropy for realistic PAT counts (1000s per host).
//
// All methods return `(nil, nil)` for "not found" so the handler
// layer can distinguish "the lookup succeeded but no such PAT"
// from "the lookup itself failed" — same convention the rest of
// EtcdStore follows for manifests and status blobs.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// PutPAT writes (or overwrites) the PAT record at /pats/<id>.
// Overwrite semantics are intentional: TouchPAT uses this to update
// LastUsedAt without a separate transaction, and a future scope-edit
// flow can rewrite the same record.
//
// Validation (non-empty ID, well-formed scopes, etc.) happens at
// the caller — this layer trusts what it stores.
func (s *EtcdStore) PutPAT(ctx context.Context, p PAT) error {
	if p.ID == "" {
		return fmt.Errorf("etcd put PAT: empty ID")
	}

	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal PAT %s: %w", p.ID, err)
	}

	if _, err := s.client.Put(ctx, PATKey(p.ID), string(data)); err != nil {
		return fmt.Errorf("etcd put PAT %s: %w", p.ID, err)
	}

	return nil
}

// GetPAT fetches one PAT by ID. Returns (nil, nil) when no record
// exists with that ID — the canonical "missing token" signal the
// auth middleware uses to issue 401.
func (s *EtcdStore) GetPAT(ctx context.Context, id string) (*PAT, error) {
	if id == "" {
		return nil, nil
	}

	resp, err := s.client.Get(ctx, PATKey(id))
	if err != nil {
		return nil, fmt.Errorf("etcd get PAT %s: %w", id, err)
	}

	if resp.Count == 0 {
		return nil, nil
	}

	var p PAT
	if err := json.Unmarshal(resp.Kvs[0].Value, &p); err != nil {
		return nil, fmt.Errorf("decode PAT %s: %w", id, err)
	}

	return &p, nil
}

// ListPATs enumerates every PAT on the host. Used by `vd pat list`
// (operator-facing) and by future audit / bulk-revoke tooling.
//
// No pagination today — the realistic upper bound is dozens of
// PATs per host (one per operator + WebUI instance + integration).
// If we ever cross hundreds, switch to keys-only listing + chunked
// per-ID decode.
func (s *EtcdStore) ListPATs(ctx context.Context) ([]PAT, error) {
	resp, err := s.client.Get(ctx, PATsPrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd list PATs: %w", err)
	}

	out := make([]PAT, 0, len(resp.Kvs))

	for _, kv := range resp.Kvs {
		var p PAT

		if err := json.Unmarshal(kv.Value, &p); err != nil {
			return nil, fmt.Errorf("decode PAT %s: %w", string(kv.Key), err)
		}

		out = append(out, p)
	}

	return out, nil
}

// DeletePAT removes one PAT by ID. Returns (true, nil) when a
// record was actually deleted, (false, nil) when no such ID
// existed (idempotent revoke — same posture as Manifest.Delete).
func (s *EtcdStore) DeletePAT(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}

	resp, err := s.client.Delete(ctx, PATKey(id))
	if err != nil {
		return false, fmt.Errorf("etcd delete PAT %s: %w", id, err)
	}

	return resp.Deleted > 0, nil
}

// TouchPAT updates LastUsedAt for one PAT — best-effort.
//
// Read-modify-write rather than a transactional update because:
//   - Touch races resolve naturally to "later timestamp wins"
//     (both writers move the field monotonically forward).
//   - Field is observability metadata; a lost write means the next
//     auth retries the touch on its own coalesce-window.
//   - Etcd transactional update would cost two RPCs per touch
//     instead of one, doubling the LastUsedAt write cost the
//     middleware's coalesce dampener was designed to bound.
//
// No-op when the ID doesn't exist (silent — the auth path would
// have failed before TouchPAT ran in that case, but we keep the
// guard for direct callers).
func (s *EtcdStore) TouchPAT(ctx context.Context, id string, at time.Time) error {
	cur, err := s.GetPAT(ctx, id)
	if err != nil {
		return err
	}

	if cur == nil {
		return nil
	}

	cur.LastUsedAt = at.UTC()

	return s.PutPAT(ctx, *cur)
}
