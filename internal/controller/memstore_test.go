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
	mu  sync.Mutex
	kv  map[string]*Manifest
	rev int64

	watchers []chan WatchEvent
}

func newMemStore() *memStore {
	return &memStore{kv: map[string]*Manifest{}}
}

func (m *memStore) Put(ctx context.Context, man *Manifest) (*Manifest, error) {
	if err := man.Validate(); err != nil {
		return nil, err
	}

	m.mu.Lock()

	m.rev++
	man.Metadata = &Metadata{UpdatedAt: time.Now().UTC(), Revision: m.rev}

	copy := cloneManifest(man)
	m.kv[DesiredKey(man.Kind, man.Name)] = copy

	ev := WatchEvent{Type: WatchPut, Kind: man.Kind, Name: man.Name, Manifest: copy, Revision: m.rev}
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

func (m *memStore) Get(ctx context.Context, kind Kind, name string) (*Manifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	v, ok := m.kv[DesiredKey(kind, name)]
	if !ok {
		return nil, nil
	}

	return cloneManifest(v), nil
}

func (m *memStore) Delete(ctx context.Context, kind Kind, name string) (bool, error) {
	m.mu.Lock()

	key := DesiredKey(kind, name)
	_, ok := m.kv[key]

	if ok {
		delete(m.kv, key)
		m.rev++
	}

	ev := WatchEvent{Type: WatchDelete, Kind: kind, Name: name, Revision: m.rev}
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
