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
type Store interface {
	Put(ctx context.Context, m *Manifest) (*Manifest, error)
	Get(ctx context.Context, kind Kind, name string) (*Manifest, error)
	Delete(ctx context.Context, kind Kind, name string) (bool, error)
	List(ctx context.Context, kind Kind) ([]*Manifest, error)
	ListAll(ctx context.Context) ([]*Manifest, error)
	Watch(ctx context.Context) <-chan WatchEvent
	Close() error

	// Status I/O — stored under /status/<kind>s/<name> separately from
	// /desired so re-applying a manifest doesn't erase what the plugin
	// produced (credentials, container ids, etc.). GetStatus returns
	// (nil, nil) when no status exists — callers treat it as "not yet
	// reconciled", not an error.
	PutStatus(ctx context.Context, kind Kind, name string, data []byte) error
	GetStatus(ctx context.Context, kind Kind, name string) ([]byte, error)
	DeleteStatus(ctx context.Context, kind Kind, name string) error
}

// WatchEvent is a single change observed on /desired/*.
type WatchEvent struct {
	Type     WatchEventType
	Kind     Kind
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

	resp, err := s.client.Put(ctx, DesiredKey(m.Kind, m.Name), string(data))
	if err != nil {
		return nil, fmt.Errorf("etcd put: %w", err)
	}

	m.Metadata.Revision = resp.Header.Revision

	return m, nil
}

func (s *EtcdStore) Get(ctx context.Context, kind Kind, name string) (*Manifest, error) {
	resp, err := s.client.Get(ctx, DesiredKey(kind, name))
	if err != nil {
		return nil, fmt.Errorf("etcd get: %w", err)
	}

	if resp.Count == 0 {
		return nil, nil
	}

	return decodeManifest(resp.Kvs[0].Value, resp.Kvs[0].ModRevision)
}

func (s *EtcdStore) Delete(ctx context.Context, kind Kind, name string) (bool, error) {
	resp, err := s.client.Delete(ctx, DesiredKey(kind, name))
	if err != nil {
		return false, fmt.Errorf("etcd delete: %w", err)
	}

	return resp.Deleted > 0, nil
}

func (s *EtcdStore) List(ctx context.Context, kind Kind) ([]*Manifest, error) {
	return s.listPrefix(ctx, DesiredPrefix(kind))
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

				kind, name, ok := parseDesiredKey(string(ev.Kv.Key))
				if !ok {
					continue
				}

				evt.Kind = kind
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

// parseDesiredKey turns "/desired/deployments/api" into (deployment, api).
func parseDesiredKey(key string) (Kind, string, bool) {
	if len(key) <= len(prefixDesired) {
		return "", "", false
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
		return "", "", false
	}

	kindPlural := rest[:slash]
	name := rest[slash+1:]

	kind, err := ParseKind(kindPlural)
	if err != nil {
		return "", "", false
	}

	return kind, name, true
}
