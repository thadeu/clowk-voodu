package metrics

import (
	"sort"
	"sync"
)

// ingressKey is the grouping key for one bucket: (host, scope, name).
// Host is the per-request identity from Caddy's log; scope + name are
// the resolved deployment identity (from the host resolver). All three
// flow into the emitted NDJSON line so the warehouse can index on any
// combination.
type ingressKey struct {
	host  string
	scope string
	name  string
}

// ingressBucket accumulates one Tick window's worth of requests for
// one (host, scope, name). Counts are exact; durations are kept as
// a slice (sorted at drain time for percentile calc) capped at
// MaxRequestsPerWindow to bound memory.
type ingressBucket struct {
	count    uint64
	s2xx     uint64
	s3xx     uint64
	s4xx     uint64
	s5xx     uint64
	bytesOut uint64
	// durations in MILLISECONDS — Caddy emits seconds (float), we
	// convert on push so the aggregator and the wire format both
	// speak ms (more readable, matches latency chart units).
	durations []float64
}

// IngressAggregator is the in-memory tick window. Thread-safe so the
// tail-reader goroutine (Push) and the sampler's drain goroutine
// (Drain) can interleave. Operations are short — Push is map index
// + slice append; Drain swaps the whole map under one lock then
// releases.
type IngressAggregator struct {
	mu      sync.Mutex
	buckets map[ingressKey]*ingressBucket
}

func NewIngressAggregator() *IngressAggregator {
	return &IngressAggregator{
		buckets: make(map[ingressKey]*ingressBucket),
	}
}

// Push records one request into its (host, scope, name) bucket. Safe
// to call concurrently from multiple goroutines; in practice the
// sampler only has one ingest path so contention is nil.
func (a *IngressAggregator) Push(host, scope, name string, req IngressRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()

	k := ingressKey{host: host, scope: scope, name: name}

	b, ok := a.buckets[k]
	if !ok {
		b = &ingressBucket{}
		a.buckets[k] = b
	}

	b.count++

	switch {
	case req.Status >= 500:
		b.s5xx++
	case req.Status >= 400:
		b.s4xx++
	case req.Status >= 300:
		b.s3xx++
	case req.Status >= 200:
		b.s2xx++
	}

	b.bytesOut += req.SizeBytes

	// Cap-and-drop on duration samples. 50K samples per window per
	// (host, scope, name) is enough for p99 accuracy on any realistic
	// workload (50K reqs in 15s = 3300 req/s sustained — most apps
	// won't sniff this); a runaway looping bot crosses the cap and
	// further latency samples are dropped, but counts/status remain
	// accurate so the operator's 5xx alert still fires.
	if len(b.durations) < MaxRequestsPerWindow {
		b.durations = append(b.durations, req.DurationMs)
	}
}

// Drain atomically swaps the bucket map for an empty one, returning
// the accumulated buckets to the caller. After Drain returns, the
// aggregator is ready for the next Tick window — Push calls land in
// the new map without coordination with the caller's processing of
// the returned data.
func (a *IngressAggregator) Drain() map[ingressKey]*ingressBucket {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := a.buckets
	a.buckets = make(map[ingressKey]*ingressBucket)

	return out
}

// Percentiles returns p50/p90/p95/p99/max from a freshly-sorted copy
// of the bucket's duration samples. Returns five nils when count is 0
// (the caller emits omitempty fields in the NDJSON line).
//
// Sort cost: O(n log n) per bucket per Tick. At 50K cap, ~5ms on a
// decent VPS — negligible vs the 15s Tick window. If this ever becomes
// hot, swap to t-digest (TODO when QPS demands it).
func (b *ingressBucket) Percentiles() (p50, p90, p95, p99, max *float64) {
	if len(b.durations) == 0 {
		return nil, nil, nil, nil, nil
	}

	sorted := make([]float64, len(b.durations))
	copy(sorted, b.durations)
	sort.Float64s(sorted)

	p50v := pctIdx(sorted, 0.50)
	p90v := pctIdx(sorted, 0.90)
	p95v := pctIdx(sorted, 0.95)
	p99v := pctIdx(sorted, 0.99)
	maxv := sorted[len(sorted)-1]

	return &p50v, &p90v, &p95v, &p99v, &maxv
}

// pctIdx — nearest-rank percentile. With n=1, every percentile is the
// sole sample. With n=100, p99 is sorted[99]. Linear interpolation
// would give nicer-looking numbers but rank is more honest for sparse
// windows (a single 200ms sample shouldn't average down to "p99 =
// 180ms" because of interpolation).
func pctIdx(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}

	idx := int(float64(len(sorted)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}

	return sorted[idx]
}
