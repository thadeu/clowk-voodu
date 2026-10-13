package systemstats

import (
	"math"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

// fakeHost hands out cumulative /proc/stat-style counters for a 2-core box:
// each second adds 2 CPU-seconds, `busy` of them used, the rest idle.
type fakeHost struct {
	at    time.Time
	times cpu.TimesStat
}

func newFakeHost() *fakeHost {
	return &fakeHost{at: time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)}
}

func (h *fakeHost) tick(busy float64) {
	h.at = h.at.Add(time.Second)
	h.times.User += busy
	h.times.Idle += 2 - busy
}

func near(t *testing.T, label string, got, want float64) {
	t.Helper()

	if math.Abs(got-want) > 0.01 {
		t.Fatalf("%s: got %.2f%%, want %.2f%%", label, got, want)
	}
}

// THE regression: the webui polls GET /system between two sampler ticks,
// right on top of its own poll burst. With gopsutil's global "since the last
// call" baseline that call moved the sampler's baseline too, so the chart
// recorded a sub-second window taken inside the burst (~100%) instead of the
// 15s average. Every reader must get the real average over the last Window,
// no matter who else read before it.
func TestCPUWindow_OtherReadersDoNotShrinkTheWindow(t *testing.T) {
	h := newFakeHost()
	w := &CPUWindow{Window: 15 * time.Second}

	w.Observe(h.at, h.times)

	for s := 1; s <= 15; s++ {
		busy := 0.0

		if s == 14 || s == 15 {
			busy = 2
		}

		h.tick(busy)
		w.Observe(h.at, h.times)

		if s == 14 {
			// A /system read right inside the burst.
			w.Percent()
		}
	}

	// 2 fully busy seconds of 15, on 2 cores: 4 of 30 CPU-seconds.
	near(t, "sampler tick after a 2s burst", w.Percent(), 4.0/30*100)
}

// Readers between ticks report the same last-Window average, not the
// stretch since some other reader.
func TestCPUWindow_ReadsAreRepeatable(t *testing.T) {
	h := newFakeHost()
	w := &CPUWindow{Window: 10 * time.Second}

	w.Observe(h.at, h.times)

	for s := 1; s <= 10; s++ {
		h.tick(1)
		w.Observe(h.at, h.times)
	}

	near(t, "first read", w.Percent(), 50)
	near(t, "second read", w.Percent(), 50)
}

// The window slides: busy time older than Window stops counting.
func TestCPUWindow_SlidesOverOldBusyTime(t *testing.T) {
	h := newFakeHost()
	w := &CPUWindow{Window: 10 * time.Second}

	w.Observe(h.at, h.times)

	for s := 1; s <= 10; s++ {
		h.tick(2)
		w.Observe(h.at, h.times)
	}

	near(t, "fully busy window", w.Percent(), 100)

	for s := 1; s <= 10; s++ {
		h.tick(0)
		w.Observe(h.at, h.times)
	}

	near(t, "idle window after the busy one", w.Percent(), 0)
}

// Before the first full Window (controller just booted) the average covers
// what has been seen so far, never a made-up number.
func TestCPUWindow_PartialWindowAtBoot(t *testing.T) {
	h := newFakeHost()
	w := &CPUWindow{Window: 15 * time.Second}

	near(t, "no reading yet", w.Percent(), 0)

	w.Observe(h.at, h.times)

	near(t, "one reading", w.Percent(), 0)

	h.tick(2)
	w.Observe(h.at, h.times)
	h.tick(0)
	w.Observe(h.at, h.times)

	near(t, "2s seen, 1 busy", w.Percent(), 50)
}

// Concurrent readers can reach Observe out of order; an older reading must
// not rewind the window.
func TestCPUWindow_IgnoresOutOfOrderReadings(t *testing.T) {
	h := newFakeHost()
	w := &CPUWindow{Window: 10 * time.Second}

	w.Observe(h.at, h.times)

	stale := *h

	for s := 1; s <= 10; s++ {
		h.tick(1)
		w.Observe(h.at, h.times)
	}

	w.Observe(stale.at, stale.times)

	near(t, "after a stale reading", w.Percent(), 50)
}
