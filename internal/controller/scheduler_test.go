package controller

import (
	"context"
	"testing"
	"time"
)

// newTestScheduler is the common scaffolding for scheduler tests:
// fixed clock (so we control "now" exactly), synchronous Dispatch (so
// we can assert on side effects without waiting for goroutines), and
// a real CronJobHandler backed by fakeContainers — that way every
// "did this fire?" assertion bottoms out on cm.recreates rather than
// peeking at private scheduler state.
func newTestScheduler(t *testing.T, store Store) (*CronScheduler, *CronJobHandler, *fakeContainers, *time.Time) {
	t.Helper()

	cm := &fakeContainers{}

	handler := &CronJobHandler{
		Store:      store,
		Log:        quietLogger(),
		Containers: cm,
	}

	now := time.Date(2026, 4, 24, 10, 0, 0, 0, time.UTC)
	clock := &now

	s := &CronScheduler{
		Store:    store,
		Handler:  handler,
		Logger:   quietLogger(),
		Interval: time.Minute,
		Now:      func() time.Time { return *clock },
		Dispatch: func(fn func()) { fn() },
	}

	return s, handler, cm, clock
}

// applyCronJob seeds a manifest and runs it through CronJobHandler.apply
// so the apply-time validation path runs (matches what the reconciler
// does in production).
func applyCronJob(t *testing.T, store Store, h *CronJobHandler, name string, spec map[string]any) {
	t.Helper()

	seedManifest(t, store, KindCronJob, name, spec)

	if err := h.Handle(context.Background(), putEvent(t, KindCronJob, name, spec)); err != nil {
		t.Fatalf("apply cronjob/%s: %v", name, err)
	}
}

// TestCronScheduler_FirstPassSeedsButDoesNotFire is the
// missed-tick-skipping rule: the very first iteration computes the
// next fire time but doesn't dispatch anything. A controller that
// just started must not retroactively run cronjobs whose schedules
// already passed.
func TestCronScheduler_FirstPassSeedsButDoesNotFire(t *testing.T) {
	store := newMemStore()

	s, h, cm, _ := newTestScheduler(t, store)

	applyCronJob(t, store, h, "purge", map[string]any{
		"schedule": "* * * * *",
		"job":      map[string]any{"image": "img:1"},
	})

	s.evaluate(context.Background())

	if len(cm.recreates) != 0 {
		t.Errorf("first pass must seed only, got %d recreates", len(cm.recreates))
	}

	// And state should now hold a parsed entry for the cronjob.
	s.mu.Lock()
	st, ok := s.state["test/purge"]
	s.mu.Unlock()

	if !ok || st == nil {
		t.Fatal("expected cronjob state to be seeded")
	}

	if st.expr != "* * * * *" {
		t.Errorf("seeded expr: got %q", st.expr)
	}
}

// TestCronScheduler_FiresWhenWallClockReachesNextFire walks the
// scheduler through two iterations: the first seeds, the second —
// after the clock has crossed nextFire — dispatches Tick. The Tick
// call shows up as a recreate on the fake container manager.
func TestCronScheduler_FiresWhenWallClockReachesNextFire(t *testing.T) {
	store := newMemStore()

	s, h, cm, clock := newTestScheduler(t, store)

	applyCronJob(t, store, h, "purge", map[string]any{
		"schedule": "* * * * *",
		"job":      map[string]any{"image": "img:1"},
	})

	// First evaluate: nextFire = 10:01:00. No fire on this pass.
	s.evaluate(context.Background())

	// Advance clock by two minutes — past nextFire.
	*clock = clock.Add(2 * time.Minute)

	s.evaluate(context.Background())

	if len(cm.recreates) != 1 {
		t.Fatalf("expected one Tick spawn, got %d", len(cm.recreates))
	}

	// nextFire should have advanced to the next minute boundary so a
	// follow-up evaluation at the same wall clock doesn't re-fire.
	prevFire := mustState(t, s, "test/purge").nextFire

	s.evaluate(context.Background())

	if len(cm.recreates) != 1 {
		t.Errorf("scheduler re-fired without clock advancing: now have %d spawns", len(cm.recreates))
	}

	if got := mustState(t, s, "test/purge").nextFire; !got.Equal(prevFire) {
		t.Errorf("nextFire moved despite no clock advance: %s -> %s", prevFire, got)
	}
}

// TestCronScheduler_AdvancesNextFireOnEachTick locks in the
// "advance-then-dispatch" order. Without it, a Tick whose Dispatch
// goroutine takes longer than Interval would be re-fired on the next
// iteration — a subtle bug that produces phantom duplicate runs.
func TestCronScheduler_AdvancesNextFireOnEachTick(t *testing.T) {
	store := newMemStore()

	s, h, cm, clock := newTestScheduler(t, store)

	applyCronJob(t, store, h, "every-min", map[string]any{
		"schedule": "* * * * *",
		"job":      map[string]any{"image": "img:1"},
	})

	s.evaluate(context.Background())

	*clock = clock.Add(2 * time.Minute)
	s.evaluate(context.Background())

	*clock = clock.Add(2 * time.Minute)
	s.evaluate(context.Background())

	*clock = clock.Add(2 * time.Minute)
	s.evaluate(context.Background())

	// Three "due" iterations → three spawns.
	if len(cm.recreates) != 3 {
		t.Errorf("expected 3 spawns across 3 due iterations, got %d", len(cm.recreates))
	}
}

// TestCronScheduler_PrunesRemovedCronJobs makes sure removing a
// cronjob from the store also drops its scheduler-state entry on the
// next pass. Otherwise s.state would grow unbounded across the
// controller lifetime.
func TestCronScheduler_PrunesRemovedCronJobs(t *testing.T) {
	store := newMemStore()

	s, h, _, _ := newTestScheduler(t, store)

	applyCronJob(t, store, h, "purge", map[string]any{
		"schedule": "* * * * *",
		"job":      map[string]any{"image": "img:1"},
	})

	s.evaluate(context.Background())

	if _, ok := s.state["test/purge"]; !ok {
		t.Fatal("expected state seeded on first pass")
	}

	if _, err := store.Delete(context.Background(), KindCronJob, "test", "purge"); err != nil {
		t.Fatal(err)
	}

	s.evaluate(context.Background())

	if _, ok := s.state["test/purge"]; ok {
		t.Errorf("removed cronjob still has scheduler state")
	}
}

// TestCronScheduler_ReParsesOnExpressionChange covers the
// edit-in-place flow: an operator changes `schedule = "*/5 * * * *"`
// to `schedule = "*/10 * * * *"`. The scheduler should re-parse and
// reseed nextFire from the new expression rather than carry over the
// stale one.
func TestCronScheduler_ReParsesOnExpressionChange(t *testing.T) {
	store := newMemStore()

	s, h, _, _ := newTestScheduler(t, store)

	applyCronJob(t, store, h, "purge", map[string]any{
		"schedule": "*/5 * * * *",
		"job":      map[string]any{"image": "img:1"},
	})

	s.evaluate(context.Background())

	first := mustState(t, s, "test/purge")
	if first.expr != "*/5 * * * *" {
		t.Fatalf("first expr: %q", first.expr)
	}

	applyCronJob(t, store, h, "purge", map[string]any{
		"schedule": "*/10 * * * *",
		"job":      map[string]any{"image": "img:1"},
	})

	s.evaluate(context.Background())

	second := mustState(t, s, "test/purge")
	if second.expr != "*/10 * * * *" {
		t.Errorf("expr not reseeded after edit: got %q", second.expr)
	}
}

// TestCronScheduler_SuspendDropsState makes sure flipping
// `suspend = true` on a cronjob clears its scheduler entry so an
// unsuspend later reseeds nextFire from "now" rather than reviving a
// stale one. Also asserts that no Tick fires while suspended even if
// the wall clock has crossed the original nextFire.
func TestCronScheduler_SuspendDropsState(t *testing.T) {
	store := newMemStore()

	s, h, cm, clock := newTestScheduler(t, store)

	applyCronJob(t, store, h, "purge", map[string]any{
		"schedule": "* * * * *",
		"job":      map[string]any{"image": "img:1"},
	})

	s.evaluate(context.Background())

	applyCronJob(t, store, h, "purge", map[string]any{
		"schedule": "* * * * *",
		"suspend":  true,
		"job":      map[string]any{"image": "img:1"},
	})

	*clock = clock.Add(5 * time.Minute)

	s.evaluate(context.Background())

	if _, ok := s.state["test/purge"]; ok {
		t.Errorf("suspended cronjob must drop scheduler state")
	}

	if len(cm.recreates) != 0 {
		t.Errorf("suspended cronjob fired %d times", len(cm.recreates))
	}
}

// TestCronScheduler_BadScheduleDoesNotPoisonOthers locks in the
// fault-isolation rule: a cronjob with an unparseable schedule (which
// can only happen if it bypassed apply validation, e.g. older
// manifest in storage) must not stop the scheduler from firing other
// cronjobs.
func TestCronScheduler_BadScheduleDoesNotPoisonOthers(t *testing.T) {
	store := newMemStore()

	s, _, cm, clock := newTestScheduler(t, store)

	// Bypass apply validation: write a bad-schedule manifest directly
	// into the store. This simulates a manifest written by an older
	// controller before validation existed.
	seedManifest(t, store, KindCronJob, "broken", map[string]any{
		"schedule": "garbage",
		"job":      map[string]any{"image": "img:1"},
	})

	// And a healthy one alongside it.
	seedManifest(t, store, KindCronJob, "ok", map[string]any{
		"schedule": "* * * * *",
		"job":      map[string]any{"image": "img:1"},
	})

	s.evaluate(context.Background())

	*clock = clock.Add(2 * time.Minute)

	s.evaluate(context.Background())

	if len(cm.recreates) != 1 {
		t.Errorf("healthy cronjob did not fire alongside the broken one: %d spawns", len(cm.recreates))
	}

	if len(cm.recreates) == 1 {
		got := identityFromSpec(cm.recreates[0])
		if got.Name != "ok" {
			t.Errorf("wrong cronjob fired: %+v", got)
		}
	}
}

// TestCronScheduler_StopsOnContextCancel confirms Run returns when its
// context is canceled — the property that lets Server.Stop() block
// cleanly on s.cronDone.
func TestCronScheduler_StopsOnContextCancel(t *testing.T) {
	store := newMemStore()

	s, _, _, _ := newTestScheduler(t, store)
	// Tight interval so the test doesn't sit on the default 1m if
	// something goes wrong.
	s.Interval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	cancel()

	select {
	case <-done:
		// expected
	case <-time.After(time.Second):
		t.Fatal("scheduler did not exit within 1s after ctx cancel")
	}
}

// mustState looks up a per-cronjob scheduler state entry under lock.
// Fails the test if the entry is missing — saves a `if !ok` check at
// every callsite.
func mustState(t *testing.T, s *CronScheduler, key string) *scheduleState {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.state[key]
	if !ok || st == nil {
		t.Fatalf("expected scheduler state for %q, got none", key)
	}

	return st
}
