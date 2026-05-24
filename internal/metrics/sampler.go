package metrics

import (
	"context"
	"sync"
	"time"

	"go.voodu.clowk.in/internal/systemstats"
)

// SystemSource is the seam the sampler calls each tick for the
// host snapshot. Matches systemstats.Collector's signature so the
// production wiring just passes that through; tests inject a fake
// that returns canned snapshots.
type SystemSource interface {
	Snapshot(ctx context.Context) (systemstats.Snapshot, error)
}

// PodSource is the seam for per-pod stats. Returns one entry per
// running container; mirrors the shape StatsCollector.Collect
// emits. Sampler doesn't import controller (cycle) — production
// wires a small adapter that calls StatsCollector and reshapes.
type PodSource interface {
	Collect(ctx context.Context) ([]PodRuntime, error)
}

// PodRuntime is the per-pod snapshot the sampler consumes. Mirror
// of controller.PodStats' Usage + Identity, but locally defined to
// avoid importing controller (which would import this package via
// the lifecycle wiring — cycle).
//
// The adapter in internal/controller/metrics_adapter.go (next
// commit) does the field-by-field copy.
type PodRuntime struct {
	Container string
	Kind      string
	Scope     string
	Name      string
	ReplicaID string

	CPUPercent    float64
	MemUsageBytes uint64
	MemLimitBytes uint64

	NetRxBytes      uint64
	NetTxBytes      uint64
	BlockReadBytes  uint64
	BlockWriteBytes uint64
}

// Sampler is the long-lived ticker that pulls system + pod stats
// every Tick and appends them to the Writer. Mirrors the autoscaler
// (internal/controller/autoscaler.go:141-165) wiring pattern:
// immediate-first-eval, select on ctx.Done() + ticker.C, per-tick
// errors logged but not propagated.
type Sampler struct {
	Tick      time.Duration
	Retention time.Duration
	Now       func() time.Time
	System    SystemSource
	Pods      PodSource
	Writer    *Writer
	Logger    Logger

	// baselines holds the previous cumulative counters per container
	// so we can compute deltas. Reset implicitly when a container
	// disappears (entry not refreshed → eventually cleaned by
	// pruneBaselines after a few ticks).
	mu        sync.Mutex
	baselines map[string]baseline
}

// baseline stores the last seen cumulative counters plus a marker
// for "this container just reset" so the NEXT sample suppresses
// the delta entirely (otherwise the post-reset cumulative looks
// like a giant negative delta which we'd clamp to 0, masking the
// reset signal).
type baseline struct {
	netRx     uint64
	netTx     uint64
	blockRead uint64
	blockWr   uint64

	// postReset is set true when the previous tick noticed
	// current < previous on any counter (container restarted).
	// Cleared on the NEXT successful tick.
	postReset bool

	// firstSeen is true until we've recorded one full sample with
	// real baseline numbers. First-ever tick for a container omits
	// deltas (no baseline to subtract).
	firstSeen bool

	// lastSeenTick is the tick number this baseline was refreshed
	// at. Used by pruneBaselines to drop entries for vanished
	// containers (5 ticks = 75s of absence → forget).
	lastSeenTick uint64
}

const baselineStaleAfter = 5 // ticks without sighting → drop baseline

// Run is the main loop. Blocks until ctx is cancelled. Cleanup
// runs once per tick (cheap — `os.ReadDir` + a few `os.Remove`).
func (s *Sampler) Run(ctx context.Context) {
	tick := s.Tick
	if tick <= 0 {
		tick = DefaultInterval
	}

	t := time.NewTicker(tick)
	defer t.Stop()

	if s.Now == nil {
		s.Now = time.Now
	}

	if s.baselines == nil {
		s.baselines = make(map[string]baseline)
	}

	// First tick fires immediately so chart starts populating
	// within seconds of controller boot, not after the first
	// Tick interval.
	var tickN uint64
	s.evaluate(ctx, tickN)

	for {
		select {
		case <-ctx.Done():
			return

		case <-t.C:
			tickN++
			s.evaluate(ctx, tickN)
		}
	}
}

// evaluate samples once. ts is stamped BEFORE collecting docker
// stats (which takes ~1-2s) so bucket boundaries stay aligned
// across pods regardless of daemon latency.
func (s *Sampler) evaluate(ctx context.Context, tickN uint64) {
	ts := s.Now().UTC()

	s.sampleSystem(ctx, ts)
	s.samplePods(ctx, ts, tickN)
	s.runCleanup(ctx, ts)
}

func (s *Sampler) sampleSystem(ctx context.Context, ts time.Time) {
	if s.System == nil || s.Writer == nil {
		return
	}

	snap, err := s.System.Snapshot(ctx)
	if err != nil {
		s.logf("metrics: system snapshot: %v", err)
		return
	}

	row := SystemSample{
		Ts:            ts,
		CPUPercent:    snap.CPU.Percent,
		MemUsedBytes:  snap.Mem.UsedBytes,
		MemTotalBytes: snap.Mem.TotalBytes,
	}

	// Disk[] may be empty in fake/test setups; we surface the first
	// mount only (currently `/`, see systemstats comment).
	if len(snap.Disk) > 0 {
		row.DiskUsedBytes = snap.Disk[0].UsedBytes
		row.DiskTotalBytes = snap.Disk[0].TotalBytes
	}

	if err := s.Writer.WriteSystem(row); err != nil {
		s.logf("metrics: write system: %v", err)
	}
}

func (s *Sampler) samplePods(ctx context.Context, ts time.Time, tickN uint64) {
	if s.Pods == nil || s.Writer == nil {
		return
	}

	rows, err := s.Pods.Collect(ctx)
	if err != nil {
		s.logf("metrics: pod collect: %v", err)
		return
	}

	if len(rows) > MaxPodsPerSample {
		s.logf("metrics: pod sample cap exceeded (%d > %d), truncating", len(rows), MaxPodsPerSample)
		rows = rows[:MaxPodsPerSample]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Lazy-init so tests can call evaluate() directly without
	// going through Run().
	if s.baselines == nil {
		s.baselines = make(map[string]baseline)
	}

	for _, p := range rows {
		row := PodSample{
			Ts:              ts,
			Container:       p.Container,
			Kind:            p.Kind,
			Scope:           p.Scope,
			Name:            p.Name,
			ReplicaID:       p.ReplicaID,
			CPUPercent:      p.CPUPercent,
			MemUsageBytes:   p.MemUsageBytes,
			MemLimitBytes:   p.MemLimitBytes,
			NetRxBytes:      p.NetRxBytes,
			NetTxBytes:      p.NetTxBytes,
			BlockReadBytes:  p.BlockReadBytes,
			BlockWriteBytes: p.BlockWriteBytes,
		}

		s.attachDeltas(&row, p, tickN)

		if err := s.Writer.WritePod(row); err != nil {
			s.logf("metrics: write pod %s: %v", p.Container, err)
		}
	}

	s.pruneBaselines(tickN)
}

// attachDeltas computes per-container deltas relative to the
// previous sample, with reset detection.
//
// Rules:
//   - First time we see a container: emit no deltas (firstSeen=true).
//     Record baseline for next tick.
//   - current < previous: counter rolled (container restarted).
//     Emit no deltas for THIS sample either; the difference between
//     restarts is meaningless, and clamping to 0 hides the reset
//     signal. Mark postReset so next tick omits deltas too (we want
//     two clean baseline points before resuming delta math).
//   - Normal case: delta = current - previous. Write into the row,
//     update baseline.
func (s *Sampler) attachDeltas(row *PodSample, p PodRuntime, tickN uint64) {
	prev, seen := s.baselines[p.Container]

	if !seen || prev.firstSeen {
		// First sighting OR previous tick said "post-reset, skip
		// next one too". Either way: no delta on this row, record
		// baseline so the FOLLOWING tick has one.
		s.baselines[p.Container] = baseline{
			netRx: p.NetRxBytes, netTx: p.NetTxBytes,
			blockRead: p.BlockReadBytes, blockWr: p.BlockWriteBytes,
			lastSeenTick: tickN,
			// Clear firstSeen — we've now recorded a baseline.
			firstSeen: false,
		}

		return
	}

	if prev.postReset {
		// Last tick saw a reset; this tick gets a fresh baseline
		// only. Clear postReset so next tick computes a real delta.
		s.baselines[p.Container] = baseline{
			netRx: p.NetRxBytes, netTx: p.NetTxBytes,
			blockRead: p.BlockReadBytes, blockWr: p.BlockWriteBytes,
			lastSeenTick: tickN,
		}

		return
	}

	if p.NetRxBytes < prev.netRx || p.NetTxBytes < prev.netTx ||
		p.BlockReadBytes < prev.blockRead || p.BlockWriteBytes < prev.blockWr {
		// Reset detected on ANY counter — treat the whole record
		// as fresh, omit deltas, mark postReset.
		s.baselines[p.Container] = baseline{
			netRx: p.NetRxBytes, netTx: p.NetTxBytes,
			blockRead: p.BlockReadBytes, blockWr: p.BlockWriteBytes,
			postReset:    true,
			lastSeenTick: tickN,
		}

		return
	}

	// Normal happy path: compute + record.
	netRxD := p.NetRxBytes - prev.netRx
	netTxD := p.NetTxBytes - prev.netTx
	blkRD := p.BlockReadBytes - prev.blockRead
	blkWD := p.BlockWriteBytes - prev.blockWr

	row.NetRxDeltaBytes = &netRxD
	row.NetTxDeltaBytes = &netTxD
	row.BlockReadDeltaBytes = &blkRD
	row.BlockWriteDeltaBytes = &blkWD

	s.baselines[p.Container] = baseline{
		netRx: p.NetRxBytes, netTx: p.NetTxBytes,
		blockRead: p.BlockReadBytes, blockWr: p.BlockWriteBytes,
		lastSeenTick: tickN,
	}
}

// pruneBaselines drops baselines for containers we haven't seen
// for baselineStaleAfter ticks. Bounded memory + ensures a
// container that vanishes and respawns later doesn't carry stale
// counters into a brand-new delta (replica_id changes anyway, but
// docker container names CAN repeat for statefulset ordinals).
func (s *Sampler) pruneBaselines(tickN uint64) {
	for k, v := range s.baselines {
		if tickN > v.lastSeenTick+baselineStaleAfter {
			delete(s.baselines, k)
		}
	}
}

// runCleanup is the file-retention pass. Lightweight: glob,
// parse dates, unlink old / gzip yesterday. Run inside the
// sampler's lock-free path (Writer manages its own mu).
func (s *Sampler) runCleanup(ctx context.Context, now time.Time) {
	if s.Writer == nil {
		return
	}

	retention := s.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}

	if err := Cleanup(s.Writer.dir, now, retention, s.Logger); err != nil {
		s.logf("metrics: cleanup: %v", err)
	}
}

func (s *Sampler) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}
