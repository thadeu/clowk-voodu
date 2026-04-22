package controller

import (
	"context"
	"io"
	"log"
	"os"
	"time"
)

// Reconciler watches /desired/* and reacts to changes. Handlers report
// errors back; transient ones (race against a not-yet-reconciled
// dependency) are re-queued with backoff, the rest are logged and
// dropped — the next watch event is the canonical way to recover.
type Reconciler struct {
	Store  Store
	Logger *log.Logger

	// Handlers are per-kind callbacks. Nil handlers are treated as a no-op
	// (event still logged). Tests plug in handlers to assert behaviour.
	Handlers map[Kind]HandlerFunc

	// now/sleep are test seams — the retry goroutine uses sleep so
	// tests can advance time deterministically. Defaults to time.Sleep.
	sleep func(time.Duration)
}

// HandlerFunc is invoked for every desired-state change. A non-nil
// return is logged; wrapping it in Transient asks the reconciler to
// retry after backoff.
type HandlerFunc func(context.Context, WatchEvent) error

// Run blocks until ctx is cancelled. It first replays the current
// desired state as synthetic Put events (so the reconciler catches up on
// startup without needing a separate bootstrap path), then streams watch
// events.
func (r *Reconciler) Run(ctx context.Context) error {
	if r.Logger == nil {
		r.Logger = log.New(io.Discard, "", 0)
	}

	if r.sleep == nil {
		r.sleep = time.Sleep
	}

	if err := r.replay(ctx); err != nil {
		return err
	}

	events := r.Store.Watch(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-events:
			if !ok {
				return nil
			}

			r.handle(ctx, ev, 1)
		}
	}
}

func (r *Reconciler) replay(ctx context.Context) error {
	list, err := r.Store.ListAll(ctx)
	if err != nil {
		return err
	}

	for _, m := range list {
		r.handle(ctx, WatchEvent{
			Type:     WatchPut,
			Kind:     m.Kind,
			Name:     m.Name,
			Manifest: m,
			Revision: m.Metadata.Revision,
		}, 1)
	}

	return nil
}

func (r *Reconciler) handle(ctx context.Context, ev WatchEvent, attempt int) {
	r.Logger.Printf("reconcile %s %s/%s (rev=%d attempt=%d)", ev.Type, ev.Kind, ev.Name, ev.Revision, attempt)

	if r.Handlers == nil {
		return
	}

	h, ok := r.Handlers[ev.Kind]
	if !ok || h == nil {
		return
	}

	err := h(ctx, ev)
	if err == nil {
		return
	}

	if !isTransient(err) {
		r.Logger.Printf("reconcile %s/%s failed: %v", ev.Kind, ev.Name, err)
		return
	}

	if attempt >= maxRetryAttempts {
		r.Logger.Printf("reconcile %s/%s gave up after %d attempts: %v", ev.Kind, ev.Name, attempt, err)
		return
	}

	r.Logger.Printf("reconcile %s/%s transient (%v), retry %d/%d", ev.Kind, ev.Name, err, attempt+1, maxRetryAttempts)

	r.scheduleRetry(ctx, ev, attempt+1)
}

// scheduleRetry sleeps the backoff for `attempt`, then re-reads the
// manifest from the store before re-invoking the handler. Re-reading
// matters: if the user applied a fix or deleted the key while we were
// waiting, we want to see that — blindly re-running the stale ev would
// overwrite newer desired state.
func (r *Reconciler) scheduleRetry(ctx context.Context, ev WatchEvent, attempt int) {
	go func() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		r.sleep(retryBackoff(attempt))

		if ctx.Err() != nil {
			return
		}

		m, err := r.Store.Get(ctx, ev.Kind, ev.Name)
		if err != nil {
			r.Logger.Printf("reconcile %s/%s retry lookup failed: %v", ev.Kind, ev.Name, err)
			return
		}

		if m == nil {
			// Key was deleted while we waited. The Delete watch event
			// will (or has already) run the teardown path; dropping the
			// retry is correct.
			return
		}

		fresh := WatchEvent{
			Type:     WatchPut,
			Kind:     m.Kind,
			Name:     m.Name,
			Manifest: m,
			Revision: m.Metadata.Revision,
		}

		r.handle(ctx, fresh, attempt)
	}()
}

// DefaultLogger returns a stdlib log.Logger writing to stderr with a
// reconciler prefix. Used by the production wiring; tests pass their own.
func DefaultLogger() *log.Logger {
	return log.New(os.Stderr, "[reconciler] ", log.LstdFlags)
}
