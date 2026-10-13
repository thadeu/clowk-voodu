package systemstats

import (
	"context"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

// DefaultCPUWindow is the span CPU% averages over when the caller does not
// pick one. Matches the metrics sampler tick, so each chart point is the
// average of exactly the 15s it stands for.
const DefaultCPUWindow = 15 * time.Second

// cpuReadEvery is how often Run reads the host counters. One /proc/stat
// read per second costs microseconds and bounds how far the window's start
// can drift from Window.
const cpuReadEvery = time.Second

// CPUWindow is the host CPU busy% averaged over the last Window, built from
// cumulative cpu.Times readings.
//
// It replaces gopsutil's cpu.Percent(0), which measures "since the last
// call" against ONE package-global baseline. With two readers (the metrics
// sampler every 15s and GET /system, polled by the WebUI) each call reset
// the other's baseline: the sampler then measured a sub-second window that
// often fell inside the WebUI's own poll burst and charted ~100% on an
// idle host. Here readers never move a baseline: Percent always answers the
// last Window, whoever asked before.
type CPUWindow struct {
	Window time.Duration

	mu  sync.Mutex
	obs []cpuObs
}

type cpuObs struct {
	at    time.Time
	total float64
	busy  float64
}

// Observe records one cumulative reading. Readings at or before the newest
// one are dropped: concurrent readers can arrive out of order, and an older
// reading must not rewind the window.
func (w *CPUWindow) Observe(at time.Time, t cpu.TimesStat) {
	total, busy := cpuTotalBusy(t)

	w.mu.Lock()
	defer w.mu.Unlock()

	if n := len(w.obs); n > 0 && !at.After(w.obs[n-1].at) {
		return
	}

	w.obs = append(w.obs, cpuObs{at: at, total: total, busy: busy})

	// Keep the newest reading at or before the window start (the base for
	// Percent) and everything after it.
	if base := w.baseIndex(); base > 0 {
		w.obs = append(w.obs[:0], w.obs[base:]...)
	}
}

// Percent is the busy% across all cores between the window base and the
// newest reading. Before a full Window has passed (boot), it covers what
// has been seen so far; with fewer than two readings it is 0.
func (w *CPUWindow) Percent() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.obs) < 2 {
		return 0
	}

	from := w.obs[w.baseIndex()]
	to := w.obs[len(w.obs)-1]

	totalDelta := to.total - from.total
	busyDelta := to.busy - from.busy

	if totalDelta <= 0 || busyDelta <= 0 {
		return 0
	}

	return math.Min(100, busyDelta/totalDelta*100)
}

// Run reads the host counters every cpuReadEvery until ctx ends, so the
// window has a reading near each of its edges. Without Run, the readings
// Snapshot takes still feed the window, at the callers' cadence.
func (w *CPUWindow) Run(ctx context.Context) {
	t := time.NewTicker(cpuReadEvery)
	defer t.Stop()

	w.read(ctx)

	for {
		select {
		case <-ctx.Done():
			return

		case <-t.C:
			w.read(ctx)
		}
	}
}

// read takes one reading from the host and records it.
func (w *CPUWindow) read(ctx context.Context) {
	times, err := cpu.TimesWithContext(ctx, false)
	if err != nil || len(times) == 0 {
		return
	}

	w.Observe(time.Now(), times[0])
}

// baseIndex is the newest reading at or before newest-Window, or the oldest
// reading when none is that old yet. Caller holds mu; obs is not empty.
func (w *CPUWindow) baseIndex() int {
	window := w.Window

	if window <= 0 {
		window = DefaultCPUWindow
	}

	start := w.obs[len(w.obs)-1].at.Add(-window)
	base := 0

	for i, o := range w.obs {
		if o.at.After(start) {
			break
		}

		base = i
	}

	return base
}

// cpuTotalBusy is gopsutil's own split (cpu.getAllBusy), so the numbers
// match what cpu.Percent reported before: guest time is already inside
// user on Linux, and idle + iowait are the only not-busy states.
func cpuTotalBusy(t cpu.TimesStat) (total, busy float64) {
	total = t.Total()

	if runtime.GOOS == "linux" {
		total -= t.Guest + t.GuestNice
	}

	busy = total - t.Idle - t.Iowait

	return total, busy
}
