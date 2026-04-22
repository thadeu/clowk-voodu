package controller

import (
	"context"
	"io"
	"log"
	"os"
)

// Reconciler watches /desired/* and reacts to changes. In M3 it is a
// scaffold: it logs every event and invokes a kind-specific handler.
// Actual convergence logic (pulling images, starting containers, etc.)
// lands with the Docker SDK migration in M4+.
type Reconciler struct {
	Store  Store
	Logger *log.Logger

	// Handlers are per-kind callbacks. Nil handlers are treated as a no-op
	// (event still logged). Tests plug in handlers to assert behaviour.
	Handlers map[Kind]HandlerFunc
}

// HandlerFunc is invoked for every desired-state change. The event is
// safe to stash; the Reconciler does not reuse it.
type HandlerFunc func(context.Context, WatchEvent)

// Run blocks until ctx is cancelled. It first replays the current
// desired state as synthetic Put events (so the reconciler catches up on
// startup without needing a separate bootstrap path), then streams watch
// events.
func (r *Reconciler) Run(ctx context.Context) error {
	if r.Logger == nil {
		r.Logger = log.New(io.Discard, "", 0)
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

			r.handle(ctx, ev)
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
		})
	}

	return nil
}

func (r *Reconciler) handle(ctx context.Context, ev WatchEvent) {
	r.Logger.Printf("reconcile %s %s/%s (rev=%d)", ev.Type, ev.Kind, ev.Name, ev.Revision)

	if r.Handlers == nil {
		return
	}

	if h, ok := r.Handlers[ev.Kind]; ok && h != nil {
		h(ctx, ev)
	}
}

// DefaultLogger returns a stdlib log.Logger writing to stderr with a
// reconciler prefix. Used by the production wiring; tests pass their own.
func DefaultLogger() *log.Logger {
	return log.New(os.Stderr, "[reconciler] ", log.LstdFlags)
}
