package metrics

import "sync"

// HostResolver maps a request hostname (from Caddy's log) to its
// (scope, name) deployment identity in voodu. The sampler calls
// Lookup once per parsed line; rows with unmanaged hosts (e.g.
// traffic to a host not declared in any voodu ingress) are skipped
// — we don't synthesise identity, we just don't count.
//
// All returns every binding the resolver currently knows about.
// Used by the sampler to emit zero-count "heartbeat" samples for
// known deployments that received no traffic in a window — keeps
// the warehouse time series continuous so HTTP charts advance in
// lockstep with resource charts (CPU/Mem/Net) instead of freezing
// at the last burst.
type HostResolver interface {
	Lookup(host string) (scope, name string, ok bool)
	All() []IngressBinding
}

// IngressLister is the view of voodu's apply store the resolver
// needs. Defined in this package (not internal/controller) so the
// metrics package stays cycle-free. The controller-side adapter
// implements this against the real Store; tests pass a stub.
type IngressLister interface {
	ListIngresses() ([]IngressBinding, error)
}

// IngressBinding is one (host → scope, name) row, the only output
// shape Phase 1 needs. Future fields (TLS provider, path locations,
// etc.) belong on the manifest-typed structs, not here.
type IngressBinding struct {
	Host  string
	Scope string
	Name  string
}

// CachedHostResolver keeps a `map[host]→IngressBinding` populated
// from IngressLister, refreshed manually by callers. The sampler
// calls Refresh on each Tick — the cost of List + map rebuild is
// ~milliseconds even at hundreds of ingresses, so re-poll is
// cheaper than wiring a watch + cache invalidation. If/when ingress
// counts grow into the thousands, swap to store.Watch events.
type CachedHostResolver struct {
	lister IngressLister

	mu    sync.RWMutex
	table map[string]IngressBinding
}

func NewCachedHostResolver(lister IngressLister) *CachedHostResolver {
	return &CachedHostResolver{
		lister: lister,
		table:  make(map[string]IngressBinding),
	}
}

// Refresh repopulates the table from the lister. Returns the error
// verbatim; the sampler logs and continues serving stale data — a
// 30s-old map of ingresses is still useful even if the latest list
// call failed (etcd flake, etc.).
func (r *CachedHostResolver) Refresh() error {
	bindings, err := r.lister.ListIngresses()
	if err != nil {
		return err
	}

	next := make(map[string]IngressBinding, len(bindings))

	for _, b := range bindings {
		if b.Host == "" || b.Name == "" {
			// Defensive — an ingress manifest without a host or name
			// is malformed and shouldn't be in the store; skip rather
			// than create an empty-string key that'd swallow other
			// unmanaged-host requests.
			continue
		}

		next[b.Host] = b
	}

	r.mu.Lock()
	r.table = next
	r.mu.Unlock()

	return nil
}

func (r *CachedHostResolver) Lookup(host string) (string, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	b, ok := r.table[host]
	if !ok {
		return "", "", false
	}

	return b.Scope, b.Name, true
}

// All returns a snapshot of every binding currently in the table.
// Used by the sampler's heartbeat-zero path so quiet deployments
// still get a sample every tick. Snapshot semantics (not live)
// since the caller iterates without holding the read lock.
func (r *CachedHostResolver) All() []IngressBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]IngressBinding, 0, len(r.table))
	for _, b := range r.table {
		out = append(out, b)
	}

	return out
}

// StaticHostResolver is a fixed map for tests + the rare case where
// the operator wants to manually pin host → deployment without
// declaring an ingress. Production wiring uses CachedHostResolver.
type StaticHostResolver struct {
	Bindings map[string]IngressBinding
}

func (s *StaticHostResolver) Lookup(host string) (string, string, bool) {
	b, ok := s.Bindings[host]
	if !ok {
		return "", "", false
	}

	return b.Scope, b.Name, true
}

func (s *StaticHostResolver) All() []IngressBinding {
	out := make([]IngressBinding, 0, len(s.Bindings))
	for _, b := range s.Bindings {
		out = append(out, b)
	}

	return out
}
