package controller

import (
	"context"
	"log"
	"sync"
	"time"
)

// CronScheduler is the in-memory dispatcher that turns declared
// cronjob manifests into Tick calls on a CronJobHandler. One scheduler
// per controller; runs as a single goroutine launched alongside the
// reconciler.
//
// Operating model (M4): the scheduler keeps a per-cronjob "next fire
// time" computed from each manifest's schedule. Once a wall-clock tick
// passes the next-fire time, it dispatches a Tick goroutine and
// advances next-fire to the schedule's next match. This is "missed
// ticks are skipped" semantics — matches k8s default. A controller
// restart reseeds next-fire from now, so ticks scheduled while the
// controller was down don't fire (the operator can re-run them
// imperatively if needed).
//
// We DON'T watch etcd for cronjob changes — apply/remove flow through
// the reconciler's WatchEvent path, and the scheduler simply re-reads
// the live manifest list at every iteration. List size on a real host
// is in the dozens; the cost is negligible compared to a fragile
// watch + diff layer for an M4 feature.
type CronScheduler struct {
	Store   Store
	Handler *CronJobHandler
	Logger  *log.Logger

	// Interval controls how often the scheduler wakes up to evaluate
	// fire times. 60s is the right default for a per-minute cron
	// resolution — any finer just burns CPU. Tests inject a much
	// shorter value (e.g. 10ms) to drive deterministic ticks.
	Interval time.Duration

	// Now is overridable in tests so we can drive the scheduler off a
	// virtual clock. Production leaves it nil and the scheduler uses
	// time.Now.
	Now func() time.Time

	// Dispatch runs the per-tick goroutine. Defaults to plain `go fn()`;
	// tests inject a synchronous variant so assertions don't race the
	// async run.
	Dispatch func(func())

	// state is the per-cronjob fire-time tracker. Re-derived from the
	// manifest list on each iteration so apply/remove pick up
	// automatically; entries for removed cronjobs are pruned in place.
	state map[string]*scheduleState

	mu sync.Mutex
}

// scheduleState holds the parsed Schedule and the next time it
// should fire. Cached across iterations so the cron expression isn't
// re-parsed every minute.
type scheduleState struct {
	expr     string
	schedule *Schedule
	nextFire time.Time
}

// Run blocks until ctx is canceled. Wakes up every Interval, walks the
// cronjob list once, and dispatches due ticks. Errors are logged and
// the loop continues — a single bad manifest must not stall everyone
// else's schedules.
func (s *CronScheduler) Run(ctx context.Context) {
	if s.Interval <= 0 {
		s.Interval = time.Minute
	}

	if s.Now == nil {
		s.Now = time.Now
	}

	if s.Dispatch == nil {
		s.Dispatch = func(fn func()) { go fn() }
	}

	if s.state == nil {
		s.state = map[string]*scheduleState{}
	}

	s.logf("cron scheduler started (interval=%s)", s.Interval)

	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	// Run once immediately so a fresh start picks up the manifests
	// without waiting a full Interval. The first iteration always seeds
	// fire times — no ticks fire on the seeding pass, only on
	// subsequent iterations once wall clock crosses nextFire.
	s.evaluate(ctx)

	for {
		select {
		case <-ctx.Done():
			s.logf("cron scheduler stopping")
			return

		case <-ticker.C:
			s.evaluate(ctx)
		}
	}
}

// evaluate is one pass over the cronjob list. Public for tests.
func (s *CronScheduler) evaluate(ctx context.Context) {
	// Defaults belong here too — tests call evaluate directly without
	// going through Run, and a nil state map would panic on first
	// dispatchOne. Cheap to re-check; production sets these once in Run
	// and the conditions are no-ops thereafter.
	if s.Now == nil {
		s.Now = time.Now
	}

	if s.Dispatch == nil {
		s.Dispatch = func(fn func()) { go fn() }
	}

	if s.state == nil {
		s.state = map[string]*scheduleState{}
	}

	manifests, err := s.Store.List(ctx, KindCronJob)
	if err != nil {
		s.logf("cron scheduler list failed: %v", err)
		return
	}

	now := s.Now()

	live := make(map[string]bool, len(manifests))

	for _, m := range manifests {
		key := m.Scope + "/" + m.Name

		live[key] = true

		s.dispatchOne(ctx, m, key, now)
	}

	// Prune state entries for cronjobs that no longer exist. Otherwise a
	// removed cronjob would linger in s.state forever — small leak per
	// iteration, but predictable to fix here.
	s.mu.Lock()
	for key := range s.state {
		if !live[key] {
			delete(s.state, key)
		}
	}
	s.mu.Unlock()
}

// dispatchOne handles one cronjob's evaluation. Computes (or reuses)
// the schedule, decides whether to fire, and updates nextFire. Tick
// itself runs in the Dispatch goroutine so a slow tick can't hold up
// the rest of the scheduler.
func (s *CronScheduler) dispatchOne(ctx context.Context, m *Manifest, key string, now time.Time) {
	spec, err := decodeCronJobSpec(m)
	if err != nil {
		s.logf("cronjob/%s decode failed: %v", AppID(m.Scope, m.Name), err)
		return
	}

	if spec.Suspend {
		// Drop the schedule state — when the operator un-suspends, we
		// want a fresh nextFire from "now", not a stale one from before
		// the suspend started.
		s.mu.Lock()
		delete(s.state, key)
		s.mu.Unlock()

		return
	}

	s.mu.Lock()
	st := s.state[key]
	s.mu.Unlock()

	// Re-parse if missing or expression changed (apply edited the
	// manifest under us).
	if st == nil || st.expr != spec.Schedule {
		sched, err := ParseSchedule(spec.Schedule, spec.Timezone)
		if err != nil {
			s.logf("cronjob/%s schedule parse failed: %v", AppID(m.Scope, m.Name), err)
			return
		}

		st = &scheduleState{
			expr:     spec.Schedule,
			schedule: sched,
			// First time we see this cronjob: nextFire = first match
			// strictly after now. This is the "skip missed ticks"
			// semantics; a cronjob that was due 10 minutes before the
			// scheduler saw it doesn't fire retroactively.
			nextFire: sched.Next(now),
		}

		s.mu.Lock()
		s.state[key] = st
		s.mu.Unlock()

		return
	}

	if now.Before(st.nextFire) {
		return
	}

	// Tick is due. Advance nextFire BEFORE dispatching so a slow tick
	// doesn't accidentally re-fire on the next iteration.
	st.nextFire = st.schedule.Next(now)

	scope := m.Scope
	name := m.Name

	s.Dispatch(func() {
		if _, err := s.Handler.Tick(ctx, scope, name); err != nil {
			s.logf("cronjob/%s tick error: %v", AppID(scope, name), err)
		}
	})
}

func (s *CronScheduler) logf(format string, args ...any) {
	if s.Logger == nil {
		return
	}

	s.Logger.Printf(format, args...)
}
