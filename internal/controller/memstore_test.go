package controller

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// memStore is an in-memory Store implementation used only by tests in
// this package. It lets us exercise API handlers and the reconciler
// without paying the embedded-etcd startup cost.
type memStore struct {
	mu     sync.Mutex
	kv     map[string]*Manifest
	status map[string][]byte
	config map[string]map[string]string // bucket → key:value
	rev    int64

	watchers []chan WatchEvent
}

func newMemStore() *memStore {
	return &memStore{
		kv:     map[string]*Manifest{},
		status: map[string][]byte{},
		config: map[string]map[string]string{},
	}
}

// configBucketKey is the in-memory equivalent of the etcd
// (scope, name) prefix — flat for the test store, since we never
// need prefix scans here.
func configBucketKey(scope, name string) string {
	if name == "" {
		name = "_"
	}

	return scope + "/" + name
}

func (m *memStore) SetConfig(_ context.Context, scope, name string, vars map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	bucket := make(map[string]string, len(vars))
	for k, v := range vars {
		bucket[k] = v
	}

	m.config[configBucketKey(scope, name)] = bucket

	return nil
}

func (m *memStore) PatchConfig(_ context.Context, scope, name string, vars map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := configBucketKey(scope, name)

	bucket, ok := m.config[key]
	if !ok {
		bucket = map[string]string{}
		m.config[key] = bucket
	}

	for k, v := range vars {
		if v == "" {
			delete(bucket, k)
			continue
		}

		bucket[k] = v
	}

	return nil
}

func (m *memStore) GetConfig(_ context.Context, scope, name string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	bucket, ok := m.config[configBucketKey(scope, name)]
	if !ok {
		return map[string]string{}, nil
	}

	out := make(map[string]string, len(bucket))
	for k, v := range bucket {
		out[k] = v
	}

	return out, nil
}

func (m *memStore) ResolveConfig(ctx context.Context, scope, name string) (map[string]string, error) {
	scopeLevel, _ := m.GetConfig(ctx, scope, "")
	appLevel, _ := m.GetConfig(ctx, scope, name)

	merged := make(map[string]string, len(scopeLevel)+len(appLevel))
	for k, v := range scopeLevel {
		merged[k] = v
	}

	for k, v := range appLevel {
		merged[k] = v
	}

	return merged, nil
}

func (m *memStore) DeleteConfigKey(_ context.Context, scope, name, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	bucket, ok := m.config[configBucketKey(scope, name)]
	if !ok {
		return false, nil
	}

	if _, exists := bucket[key]; !exists {
		return false, nil
	}

	delete(bucket, key)

	return true, nil
}

func (m *memStore) DeleteConfig(_ context.Context, scope, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.config, configBucketKey(scope, name))

	return nil
}

func (m *memStore) PutStatus(ctx context.Context, kind Kind, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cp := make([]byte, len(data))
	copy(cp, data)
	m.status[StatusKey(kind, name)] = cp

	return nil
}

func (m *memStore) GetStatus(ctx context.Context, kind Kind, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	v, ok := m.status[StatusKey(kind, name)]
	if !ok {
		return nil, nil
	}

	cp := make([]byte, len(v))
	copy(cp, v)

	return cp, nil
}

func (m *memStore) DeleteStatus(ctx context.Context, kind Kind, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.status, StatusKey(kind, name))

	return nil
}

func (m *memStore) Put(ctx context.Context, man *Manifest) (*Manifest, error) {
	if err := man.Validate(); err != nil {
		return nil, err
	}

	m.mu.Lock()

	m.rev++
	man.Metadata = &Metadata{UpdatedAt: time.Now().UTC(), Revision: m.rev}

	copy := cloneManifest(man)
	m.kv[DesiredKey(man.Kind, man.Scope, man.Name)] = copy

	ev := WatchEvent{Type: WatchPut, Kind: man.Kind, Scope: man.Scope, Name: man.Name, Manifest: copy, Revision: m.rev}
	watchers := append([]chan WatchEvent(nil), m.watchers...)
	m.mu.Unlock()

	for _, w := range watchers {
		select {
		case w <- ev:
		default:
		}
	}

	return cloneManifest(copy), nil
}

func (m *memStore) Get(ctx context.Context, kind Kind, scope, name string) (*Manifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	v, ok := m.kv[DesiredKey(kind, scope, name)]
	if !ok {
		return nil, nil
	}

	return cloneManifest(v), nil
}

func (m *memStore) Delete(ctx context.Context, kind Kind, scope, name string) (bool, error) {
	m.mu.Lock()

	key := DesiredKey(kind, scope, name)
	_, ok := m.kv[key]

	if ok {
		delete(m.kv, key)
		m.rev++
	}

	ev := WatchEvent{Type: WatchDelete, Kind: kind, Scope: scope, Name: name, Revision: m.rev}
	watchers := append([]chan WatchEvent(nil), m.watchers...)
	m.mu.Unlock()

	if ok {
		for _, w := range watchers {
			select {
			case w <- ev:
			default:
			}
		}
	}

	return ok, nil
}

func (m *memStore) List(ctx context.Context, kind Kind) ([]*Manifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	prefix := DesiredPrefix(kind)

	out := []*Manifest{}

	for k, v := range m.kv {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, cloneManifest(v))
		}
	}

	return out, nil
}

func (m *memStore) ListByScope(ctx context.Context, kind Kind, scope string) ([]*Manifest, error) {
	if !IsScoped(kind) {
		return m.List(ctx, kind)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	prefix := ScopedPrefix(kind, scope)

	out := []*Manifest{}

	for k, v := range m.kv {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, cloneManifest(v))
		}
	}

	return out, nil
}

func (m *memStore) ListAll(ctx context.Context) ([]*Manifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := []*Manifest{}
	for _, v := range m.kv {
		out = append(out, cloneManifest(v))
	}

	return out, nil
}

func (m *memStore) Watch(ctx context.Context) <-chan WatchEvent {
	ch := make(chan WatchEvent, 32)

	m.mu.Lock()
	m.watchers = append(m.watchers, ch)
	m.mu.Unlock()

	go func() {
		<-ctx.Done()

		m.mu.Lock()
		defer m.mu.Unlock()

		for i, w := range m.watchers {
			if w == ch {
				m.watchers = append(m.watchers[:i], m.watchers[i+1:]...)
				break
			}
		}

		close(ch)
	}()

	return ch
}

func (m *memStore) Close() error { return nil }

func cloneManifest(m *Manifest) *Manifest {
	if m == nil {
		return nil
	}

	b, _ := json.Marshal(m)

	var c Manifest
	_ = json.Unmarshal(b, &c)

	return &c
}
