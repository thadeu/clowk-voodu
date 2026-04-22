package controller

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestReconcilerReplaysExistingManifests(t *testing.T) {
	store := newMemStore()

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Name: "api"})
	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDatabase, Name: "main"})

	var (
		mu   sync.Mutex
		seen = map[Kind]int{}
	)

	record := func(kind Kind) HandlerFunc {
		return func(_ context.Context, _ WatchEvent) {
			mu.Lock()
			defer mu.Unlock()

			seen[kind]++
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
			KindDeployment: func(_ context.Context, ev WatchEvent) {
				events <- ev
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

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Name: "api"})

	select {
	case ev := <-events:
		if ev.Kind != KindDeployment || ev.Name != "api" || ev.Type != WatchPut {
			t.Errorf("unexpected event: %+v", ev)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no event delivered")
	}

	_, _ = store.Delete(t.Context(), KindDeployment, "api")

	cancel()
	<-done
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
