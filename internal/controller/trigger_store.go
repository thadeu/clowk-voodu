// trigger_store.go owns the etcd-backed CRUD for deploy triggers. Pairs with
// trigger.go (the pure data shape) and the in-memory implementation the unit
// tests use — the same three-file split pat.go / pat_store.go already follows.
//
// One record per trigger under `/triggers/<id>`, JSON-encoded.
//
// Every method returns `(nil, nil)` for "not found" so the handler layer can
// tell "the lookup succeeded and there is no such trigger" from "the lookup
// itself failed" — the convention the rest of EtcdStore uses.

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func (s *EtcdStore) PutTrigger(ctx context.Context, t Trigger) error {
	if t.ID == "" {
		return fmt.Errorf("etcd put trigger: empty ID")
	}

	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal trigger %s: %w", t.ID, err)
	}

	if _, err := s.client.Put(ctx, TriggerKey(t.ID), string(data)); err != nil {
		return fmt.Errorf("etcd put trigger %s: %w", t.ID, err)
	}

	return nil
}

func (s *EtcdStore) GetTrigger(ctx context.Context, id string) (*Trigger, error) {
	if id == "" {
		return nil, nil
	}

	resp, err := s.client.Get(ctx, TriggerKey(id))
	if err != nil {
		return nil, fmt.Errorf("etcd get trigger %s: %w", id, err)
	}

	if resp.Count == 0 {
		return nil, nil
	}

	var t Trigger
	if err := json.Unmarshal(resp.Kvs[0].Value, &t); err != nil {
		return nil, fmt.Errorf("decode trigger %s: %w", id, err)
	}

	return &t, nil
}

// ListTriggers enumerates every trigger on the host. No pagination: the
// realistic upper bound is one per repository an operator deploys to this box,
// which is a handful.
func (s *EtcdStore) ListTriggers(ctx context.Context) ([]Trigger, error) {
	resp, err := s.client.Get(ctx, TriggersPrefix(), clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd list triggers: %w", err)
	}

	out := make([]Trigger, 0, len(resp.Kvs))

	for _, kv := range resp.Kvs {
		var t Trigger

		if err := json.Unmarshal(kv.Value, &t); err != nil {
			return nil, fmt.Errorf("decode trigger %s: %w", string(kv.Key), err)
		}

		out = append(out, t)
	}

	return out, nil
}

// DeleteTrigger removes one trigger. Returns (true, nil) when a record was
// actually removed, (false, nil) when there was none — idempotent, the same
// posture as revoking a PAT.
func (s *EtcdStore) DeleteTrigger(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}

	resp, err := s.client.Delete(ctx, TriggerKey(id))
	if err != nil {
		return false, fmt.Errorf("etcd delete trigger %s: %w", id, err)
	}

	return resp.Deleted > 0, nil
}
