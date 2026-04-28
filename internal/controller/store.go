package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Store is the persistence contract for desired state. Defined as an
// interface so tests can substitute an in-memory implementation without
// spinning up etcd.
//
// Scoped kinds (deployment, ingress) must carry a non-empty scope;
// unscoped kinds ignore the scope argument. The split lives on the
// manifest type (see IsScoped) so the store stays kind-agnostic.
type Store interface {
	Put(ctx context.Context, m *Manifest) (*Manifest, error)
	Get(ctx context.Context, kind Kind, scope, name string) (*Manifest, error)
	Delete(ctx context.Context, kind Kind, scope, name string) (bool, error)

	// List returns every manifest of kind across all scopes. Useful for
	// the reconciler and the collision check in /apply.
	List(ctx context.Context, kind Kind) ([]*Manifest, error)

	// ListByScope returns the manifests of kind filed under scope. This
	// is the set /apply diff-uses to compute prune candidates.
	ListByScope(ctx context.Context, kind Kind, scope string) ([]*Manifest, error)

	ListAll(ctx context.Context) ([]*Manifest, error)
	Watch(ctx context.Context) <-chan WatchEvent
	Close() error

	// Status I/O — stored under /status/<kind>s/<name> separately from
	// /desired so re-applying a manifest doesn't erase what the plugin
	// produced (credentials, container ids, etc.). Status is keyed by
	// name only: the /apply layer guarantees (kind, name) uniqueness
	// across scopes, so no scope segment is needed here.
	PutStatus(ctx context.Context, kind Kind, name string, data []byte) error
	GetStatus(ctx context.Context, kind Kind, name string) ([]byte, error)
	DeleteStatus(ctx context.Context, kind Kind, name string) error

	// Config I/O. scope is required; name="" addresses scope-level
	// config (shared across resources in scope). Set replaces every
	// supplied key at once and removes any key in the bucket that's
	// not present in the input — pass an empty map to wipe the bucket.
	SetConfig(ctx context.Context, scope, name string, vars map[string]string) error

	// PatchConfig merges keys without removing others — used by
	// `vd config set` which only sets the listed pairs. Empty value
	// removes the key (idiomatic shell-style "unset").
	PatchConfig(ctx context.Context, scope, name string, vars map[string]string) error

	// GetConfig returns every key:value in the (scope, name) bucket.
	// nil/empty when nothing is set.
	GetConfig(ctx context.Context, scope, name string) (map[string]string, error)

	// ResolveConfig returns the merged scope-level + app-level env
	// for a resource. App-level keys override scope-level keys on
	// conflict — same precedence apps already expect from /etc/
	// environment vs ~/.profile.
	ResolveConfig(ctx context.Context, scope, name string) (map[string]string, error)

	// DeleteConfigKey removes a single key from a bucket. Returns
	// (true, nil) when the key existed, (false, nil) when it didn't.
	DeleteConfigKey(ctx context.Context, scope, name, key string) (bool, error)

	// DeleteConfig wipes the entire (scope, name) bucket — every
	// key under the prefix. Used by `vd delete --prune` to nuke a
	// resource's config alongside its manifest, and by the scope-
	// wipe path to clear scope-level + every per-app bucket. No-op
	// when the bucket is empty.
	DeleteConfig(ctx context.Context, scope, name string) error
}

// WatchEvent is a single change observed on /desired/*.
type WatchEvent struct {
	Type     WatchEventType
	Kind     Kind
	Scope    string
	Name     string
	Manifest *Manifest // populated for Put; nil for Delete
	Revision int64
}

type WatchEventType string

const (
	WatchPut    WatchEventType = "put"
	WatchDelete WatchEventType = "delete"
)

// EtcdStore is a Store backed by an etcd v3 client.
type EtcdStore struct {
	client *clientv3.Client
}

// NewEtcdStore wraps a client. The caller keeps ownership of the client
// (Close() on the store is a no-op for the client itself).
func NewEtcdStore(client *clientv3.Client) *EtcdStore {
	return &EtcdStore{client: client}
}

// Close is a no-op; the client is owned by the caller.
func (s *EtcdStore) Close() error { return nil }

// Put serialises the manifest as JSON and writes it at
// /desired/<kind>s/<name>. Metadata is populated from the etcd revision.
func (s *EtcdStore) Put(ctx context.Context, m *Manifest) (*Manifest, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}

	now := time.Now().UTC()

	if m.Metadata == nil {
		m.Metadata = &Metadata{CreatedAt: now}
	}

	m.Metadata.UpdatedAt = now

	data, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}

	resp, err := s.client.Put(ctx, DesiredKey(m.Kind, m.Scope, m.Name), string(data))
	if err != nil {
		return nil, fmt.Errorf("etcd put: %w", err)
	}

	m.Metadata.Revision = resp.Header.Revision

	return m, nil
}

func (s *EtcdStore) Get(ctx context.Context, kind Kind, scope, name string) (*Manifest, error) {
	resp, err := s.client.Get(ctx, DesiredKey(kind, scope, name))
	if err != nil {
		return nil, fmt.Errorf("etcd get: %w", err)
	}

	if resp.Count == 0 {
		return nil, nil
	}

	return decodeManifest(resp.Kvs[0].Value, resp.Kvs[0].ModRevision)
}

func (s *EtcdStore) Delete(ctx context.Context, kind Kind, scope, name string) (bool, error) {
	resp, err := s.client.Delete(ctx, DesiredKey(kind, scope, name))
	if err != nil {
		return false, fmt.Errorf("etcd delete: %w", err)
	}

	return resp.Deleted > 0, nil
}

func (s *EtcdStore) List(ctx context.Context, kind Kind) ([]*Manifest, error) {
	return s.listPrefix(ctx, DesiredPrefix(kind))
}

func (s *EtcdStore) ListByScope(ctx context.Context, kind Kind, scope string) ([]*Manifest, error) {
	if !IsScoped(kind) {
		return s.List(ctx, kind)
	}

	return s.listPrefix(ctx, ScopedPrefix(kind, scope))
}

func (s *EtcdStore) ListAll(ctx context.Context) ([]*Manifest, error) {
	return s.listPrefix(ctx, AllDesiredPrefix())
}

func (s *EtcdStore) listPrefix(ctx context.Context, prefix string) ([]*Manifest, error) {
	resp, err := s.client.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd list: %w", err)
	}

	out := make([]*Manifest, 0, len(resp.Kvs))

	for _, kv := range resp.Kvs {
		m, err := decodeManifest(kv.Value, kv.ModRevision)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", string(kv.Key), err)
		}

		out = append(out, m)
	}

	return out, nil
}

func (s *EtcdStore) PutStatus(ctx context.Context, kind Kind, name string, data []byte) error {
	if _, err := s.client.Put(ctx, StatusKey(kind, name), string(data)); err != nil {
		return fmt.Errorf("etcd put status: %w", err)
	}

	return nil
}

func (s *EtcdStore) GetStatus(ctx context.Context, kind Kind, name string) ([]byte, error) {
	resp, err := s.client.Get(ctx, StatusKey(kind, name))
	if err != nil {
		return nil, fmt.Errorf("etcd get status: %w", err)
	}

	if resp.Count == 0 {
		return nil, nil
	}

	return resp.Kvs[0].Value, nil
}

func (s *EtcdStore) DeleteStatus(ctx context.Context, kind Kind, name string) error {
	if _, err := s.client.Delete(ctx, StatusKey(kind, name)); err != nil {
		return fmt.Errorf("etcd delete status: %w", err)
	}

	return nil
}

// SetConfig replaces every key in the (scope, name) bucket with the
// supplied set. Keys present in etcd but not in `vars` are removed —
// idempotent "make the bucket equal to this map" semantics.
func (s *EtcdStore) SetConfig(ctx context.Context, scope, name string, vars map[string]string) error {
	current, err := s.GetConfig(ctx, scope, name)
	if err != nil {
		return err
	}

	prefix := ConfigPrefix(scope, name)

	// Delete keys not in the input.
	for k := range current {
		if _, keep := vars[k]; !keep {
			if _, err := s.client.Delete(ctx, prefix+k); err != nil {
				return fmt.Errorf("etcd delete config %s: %w", k, err)
			}
		}
	}

	// Upsert the rest.
	for k, v := range vars {
		if _, err := s.client.Put(ctx, prefix+k, v); err != nil {
			return fmt.Errorf("etcd put config %s: %w", k, err)
		}
	}

	return nil
}

// PatchConfig merges keys without removing others. Empty values
// remove the key (matches shell `unset` ergonomics).
func (s *EtcdStore) PatchConfig(ctx context.Context, scope, name string, vars map[string]string) error {
	prefix := ConfigPrefix(scope, name)

	for k, v := range vars {
		if v == "" {
			if _, err := s.client.Delete(ctx, prefix+k); err != nil {
				return fmt.Errorf("etcd delete config %s: %w", k, err)
			}

			continue
		}

		if _, err := s.client.Put(ctx, prefix+k, v); err != nil {
			return fmt.Errorf("etcd put config %s: %w", k, err)
		}
	}

	return nil
}

// GetConfig returns every key:value in the (scope, name) bucket.
func (s *EtcdStore) GetConfig(ctx context.Context, scope, name string) (map[string]string, error) {
	prefix := ConfigPrefix(scope, name)

	resp, err := s.client.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd get config: %w", err)
	}

	out := make(map[string]string, resp.Count)

	for _, kv := range resp.Kvs {
		key := string(kv.Key)[len(prefix):]
		out[key] = string(kv.Value)
	}

	return out, nil
}

// ResolveConfig returns scope-level + app-level merged. App-level
// wins on conflict.
func (s *EtcdStore) ResolveConfig(ctx context.Context, scope, name string) (map[string]string, error) {
	scopeLevel, err := s.GetConfig(ctx, scope, "")
	if err != nil {
		return nil, err
	}

	appLevel, err := s.GetConfig(ctx, scope, name)
	if err != nil {
		return nil, err
	}

	merged := make(map[string]string, len(scopeLevel)+len(appLevel))

	for k, v := range scopeLevel {
		merged[k] = v
	}

	for k, v := range appLevel {
		merged[k] = v
	}

	return merged, nil
}

// DeleteConfigKey removes one key from a bucket.
func (s *EtcdStore) DeleteConfigKey(ctx context.Context, scope, name, key string) (bool, error) {
	resp, err := s.client.Delete(ctx, ConfigKey(scope, name, key))
	if err != nil {
		return false, fmt.Errorf("etcd delete config: %w", err)
	}

	return resp.Deleted > 0, nil
}

// DeleteConfig wipes every key under the (scope, name) bucket
// prefix. Used by --prune flows. Idempotent — empty bucket is a
// no-op success.
func (s *EtcdStore) DeleteConfig(ctx context.Context, scope, name string) error {
	if _, err := s.client.Delete(ctx, ConfigPrefix(scope, name), clientv3.WithPrefix()); err != nil {
		return fmt.Errorf("etcd delete config bucket: %w", err)
	}

	return nil
}

// Watch returns a channel of events on /desired/*. The channel is closed
// when ctx is cancelled or the underlying watcher errors out.
func (s *EtcdStore) Watch(ctx context.Context) <-chan WatchEvent {
	out := make(chan WatchEvent, 16)

	go func() {
		defer close(out)

		ch := s.client.Watch(ctx, AllDesiredPrefix(), clientv3.WithPrefix(), clientv3.WithPrevKV())

		for wresp := range ch {
			if err := wresp.Err(); err != nil {
				return
			}

			for _, ev := range wresp.Events {
				evt := WatchEvent{Revision: ev.Kv.ModRevision}

				kind, scope, name, ok := parseDesiredKey(string(ev.Kv.Key))
				if !ok {
					continue
				}

				evt.Kind = kind
				evt.Scope = scope
				evt.Name = name

				switch ev.Type.String() {
				case "PUT":
					evt.Type = WatchPut

					if m, err := decodeManifest(ev.Kv.Value, ev.Kv.ModRevision); err == nil {
						evt.Manifest = m
					}
				case "DELETE":
					evt.Type = WatchDelete
				default:
					continue
				}

				select {
				case out <- evt:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out
}

func decodeManifest(data []byte, rev int64) (*Manifest, error) {
	var m Manifest

	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}

	if m.Metadata == nil {
		m.Metadata = &Metadata{}
	}

	m.Metadata.Revision = rev

	return &m, nil
}

// parseDesiredKey splits an etcd key back into its kind / scope / name
// parts. Scoped kinds match "/desired/<kind>s/<scope>/<name>"; unscoped
// kinds match "/desired/<kind>s/<name>" with an empty scope in the
// return value.
func parseDesiredKey(key string) (Kind, string, string, bool) {
	if len(key) <= len(prefixDesired) {
		return "", "", "", false
	}

	rest := key[len(prefixDesired):]

	slash := -1

	for i, c := range rest {
		if c == '/' {
			slash = i
			break
		}
	}

	if slash <= 0 || slash >= len(rest)-1 {
		return "", "", "", false
	}

	kindPlural := rest[:slash]
	tail := rest[slash+1:]

	kind, err := ParseKind(kindPlural)
	if err != nil {
		return "", "", "", false
	}

	if !IsScoped(kind) {
		return kind, "", tail, true
	}

	// Scoped: tail is "<scope>/<name>".
	next := -1

	for i, c := range tail {
		if c == '/' {
			next = i
			break
		}
	}

	if next <= 0 || next >= len(tail)-1 {
		return "", "", "", false
	}

	return kind, tail[:next], tail[next+1:], true
}
