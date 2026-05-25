package metrics

import (
	"testing"
)

func TestAggregator_PushBasic(t *testing.T) {
	a := NewIngressAggregator()

	a.Push("api.example.com", "myscope", "api", IngressRequest{
		Host: "api.example.com", DurationMs: 10, Status: 200, SizeBytes: 100,
	})
	a.Push("api.example.com", "myscope", "api", IngressRequest{
		Host: "api.example.com", DurationMs: 20, Status: 200, SizeBytes: 200,
	})
	a.Push("api.example.com", "myscope", "api", IngressRequest{
		Host: "api.example.com", DurationMs: 30, Status: 500, SizeBytes: 50,
	})

	got := a.Drain()
	if len(got) != 1 {
		t.Fatalf("want 1 bucket, got %d", len(got))
	}

	b := got[ingressKey{"api.example.com", "myscope", "api"}]
	if b == nil {
		t.Fatal("bucket missing")
	}

	if b.count != 3 {
		t.Errorf("count: want 3, got %d", b.count)
	}
	if b.s2xx != 2 {
		t.Errorf("2xx: want 2, got %d", b.s2xx)
	}
	if b.s5xx != 1 {
		t.Errorf("5xx: want 1, got %d", b.s5xx)
	}
	if b.bytesOut != 350 {
		t.Errorf("bytesOut: want 350, got %d", b.bytesOut)
	}
}

// Drain MUST atomically swap the map so Push calls landing right
// after the drain don't get attributed to the previous window's
// bucket. Critical because the sampler emits + immediately allows
// new pushes in the same tick.
func TestAggregator_DrainAtomic(t *testing.T) {
	a := NewIngressAggregator()

	a.Push("h1", "s", "n", IngressRequest{Status: 200, DurationMs: 5})

	first := a.Drain()
	if len(first) != 1 {
		t.Fatalf("first drain: want 1, got %d", len(first))
	}

	// Second drain on a fresh aggregator should be empty — old map
	// not handed out again.
	second := a.Drain()
	if len(second) != 0 {
		t.Fatalf("second drain after empty: want 0, got %d", len(second))
	}

	// Push after drain lands in NEW map, not old one (which we hold a
	// reference to). The old buckets keep their count.
	a.Push("h1", "s", "n", IngressRequest{Status: 200, DurationMs: 5})
	if first[ingressKey{"h1", "s", "n"}].count != 1 {
		t.Errorf("drained map mutated by subsequent Push — atomicity violated")
	}
}

// Percentile correctness on a known sequence. Nearest-rank: with 100
// samples sorted, p99 is index 99.
func TestAggregator_Percentiles(t *testing.T) {
	a := NewIngressAggregator()

	// Push 100 requests with durations 1..100ms.
	for i := 1; i <= 100; i++ {
		a.Push("h", "s", "n", IngressRequest{Status: 200, DurationMs: float64(i)})
	}

	got := a.Drain()
	b := got[ingressKey{"h", "s", "n"}]

	p50, p90, p95, p99, max := b.Percentiles()

	if *p50 != 50 {
		t.Errorf("p50: want 50, got %v", *p50)
	}
	if *p90 != 90 {
		t.Errorf("p90: want 90, got %v", *p90)
	}
	if *p95 != 95 {
		t.Errorf("p95: want 95, got %v", *p95)
	}
	// nearest-rank with n=100: index = int(99 * 0.99) = 98 → sorted[98] = 99.
	if *p99 != 99 {
		t.Errorf("p99: want 99 (nearest-rank), got %v", *p99)
	}
	if *max != 100 {
		t.Errorf("max: want 100, got %v", *max)
	}
}

// Empty bucket → nil percentile pointers so the NDJSON line drops
// those fields entirely (omitempty). Critical to distinguish "0ms
// latency observed" from "no measurements yet".
func TestAggregator_EmptyBucketPercentilesNil(t *testing.T) {
	b := &ingressBucket{}

	p50, p90, p95, p99, max := b.Percentiles()

	for i, v := range []*float64{p50, p90, p95, p99, max} {
		if v != nil {
			t.Errorf("percentile %d: want nil for empty bucket, got %v", i, *v)
		}
	}
}

// Cap-and-drop: when MaxRequestsPerWindow exceeded, count keeps
// climbing but durations array stops growing. Count metrics stay
// accurate (5xx alerts still fire); latency loses tail precision
// (acceptable trade-off for bounded memory).
func TestAggregator_DurationsCap(t *testing.T) {
	a := NewIngressAggregator()

	pushes := MaxRequestsPerWindow + 10

	for i := 0; i < pushes; i++ {
		a.Push("h", "s", "n", IngressRequest{Status: 200, DurationMs: float64(i)})
	}

	got := a.Drain()
	b := got[ingressKey{"h", "s", "n"}]

	if int(b.count) != pushes {
		t.Errorf("count: want %d (includes capped), got %d", pushes, b.count)
	}
	if len(b.durations) != MaxRequestsPerWindow {
		t.Errorf("durations: want capped at %d, got %d", MaxRequestsPerWindow, len(b.durations))
	}
}

// Status class buckets cover the standard HTTP families. 1xx is rare
// enough that we accept the dropped-on-floor behaviour for v1 (most
// 100 Continue / 101 Switching Protocols traffic doesn't matter for
// dashboards); revisit if an operator surfaces it.
func TestAggregator_StatusBuckets(t *testing.T) {
	a := NewIngressAggregator()

	cases := []struct {
		status int
		bucket string
	}{
		{100, "none"}, // 1xx — currently dropped
		{200, "2xx"}, {204, "2xx"}, {299, "2xx"},
		{301, "3xx"}, {304, "3xx"}, {399, "3xx"},
		{400, "4xx"}, {404, "4xx"}, {429, "4xx"}, {499, "4xx"},
		{500, "5xx"}, {502, "5xx"}, {504, "5xx"}, {599, "5xx"},
	}

	for _, c := range cases {
		a.Push("h", "s", "n", IngressRequest{Status: c.status, DurationMs: 1})
	}

	got := a.Drain()
	b := got[ingressKey{"h", "s", "n"}]

	if b.s2xx != 3 {
		t.Errorf("2xx: want 3, got %d", b.s2xx)
	}
	if b.s3xx != 3 {
		t.Errorf("3xx: want 3, got %d", b.s3xx)
	}
	if b.s4xx != 4 {
		t.Errorf("4xx: want 4, got %d", b.s4xx)
	}
	if b.s5xx != 4 {
		t.Errorf("5xx: want 4, got %d", b.s5xx)
	}
	// count includes the 1xx that didn't land in any status bucket
	if b.count != uint64(len(cases)) {
		t.Errorf("count: want %d, got %d", len(cases), b.count)
	}
}
