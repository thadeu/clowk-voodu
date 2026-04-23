package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var errTestRefMissing = errors.New("ref.database.main.url not yet reconciled")

func TestReconcilerReplaysExistingManifests(t *testing.T) {
	store := newMemStore()

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Scope: "test", Name: "api"})
	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDatabase, Name: "main"})

	var (
		mu   sync.Mutex
		seen = map[Kind]int{}
	)

	record := func(kind Kind) HandlerFunc {
		return func(_ context.Context, _ WatchEvent) error {
			mu.Lock()
			defer mu.Unlock()

			seen[kind]++

			return nil
		}
	}

	rec := &Reconciler{
		Store: store,
		Handlers: map[Kind]HandlerFunc{
			KindDeployment: record(KindDeployment),
			KindDatabase:   record(KindDatabase),
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = rec.Run(ctx)
		close(done)
	}()

	deadline := time.After(400 * time.Millisecond)
	for {
		mu.Lock()
		ok := seen[KindDeployment] == 1 && seen[KindDatabase] == 1
		mu.Unlock()

		if ok {
			break
		}

		select {
		case <-deadline:
			t.Fatalf("handlers not invoked on replay: %+v", seen)
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	<-done
}

func TestReconcilerDispatchesLiveEvents(t *testing.T) {
	store := newMemStore()

	events := make(chan WatchEvent, 4)

	rec := &Reconciler{
		Store: store,
		Handlers: map[Kind]HandlerFunc{
			KindDeployment: func(_ context.Context, ev WatchEvent) error {
				events <- ev
				return nil
			},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = rec.Run(ctx)
		close(done)
	}()

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Scope: "test", Name: "api"})

	select {
	case ev := <-events:
		if ev.Kind != KindDeployment || ev.Name != "api" || ev.Type != WatchPut {
			t.Errorf("unexpected event: %+v", ev)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no event delivered")
	}

	_, _ = store.Delete(t.Context(), KindDeployment, "test", "api")

	cancel()
	<-done
}

func TestReconcilerRetriesTransientErrors(t *testing.T) {
	store := newMemStore()

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Scope: "test", Name: "api"})

	var (
		mu       sync.Mutex
		attempts int
		done     = make(chan struct{}, 1)
	)

	handler := func(_ context.Context, _ WatchEvent) error {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()

		// Fail twice, then succeed — matches the "DB not ready yet, now
		// ready" pattern that motivates this whole mechanism.
		if n < 3 {
			return Transient(errTestRefMissing)
		}

		select {
		case done <- struct{}{}:
		default:
		}

		return nil
	}

	rec := &Reconciler{
		Store:    store,
		Handlers: map[Kind]HandlerFunc{KindDeployment: handler},
		sleep:    func(time.Duration) {}, // zero-delay backoff in tests
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		_ = rec.Run(ctx)
		close(runDone)
	}()

	select {
	case <-done:
	case <-time.After(800 * time.Millisecond):
		t.Fatalf("handler never succeeded, attempts=%d", attempts)
	}

	cancel()
	<-runDone

	mu.Lock()
	defer mu.Unlock()

	if attempts != 3 {
		t.Errorf("expected 3 attempts (2 fail + 1 ok), got %d", attempts)
	}
}

func TestReconcilerGivesUpAfterMaxAttempts(t *testing.T) {
	store := newMemStore()

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Scope: "test", Name: "api"})

	var (
		mu       sync.Mutex
		attempts int
	)

	handler := func(_ context.Context, _ WatchEvent) error {
		mu.Lock()
		attempts++
		mu.Unlock()

		return Transient(errTestRefMissing)
	}

	rec := &Reconciler{
		Store:    store,
		Handlers: map[Kind]HandlerFunc{KindDeployment: handler},
		sleep:    func(time.Duration) {},
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = rec.Run(ctx)
		close(done)
	}()

	// Wait for attempts to plateau: loop reads under lock, gives up when
	// counter stabilises (= retry chain exhausted).
	var last int

	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		cur := attempts
		mu.Unlock()

		if cur == last && cur > 0 {
			break
		}

		last = cur
	}

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()

	if attempts != maxRetryAttempts {
		t.Errorf("expected exactly %d attempts before give-up, got %d", maxRetryAttempts, attempts)
	}
}

func TestReconcilerNilHandlerIsNoop(t *testing.T) {
	store := newMemStore()

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindService, Name: "web"})

	rec := &Reconciler{Store: store}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	if err := rec.Run(ctx); err != nil {
		t.Fatalf("Run returned error with nil handlers: %v", err)
	}
}
