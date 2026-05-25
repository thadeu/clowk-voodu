package controller

import (
	"context"
	"time"

	"go.voodu.clowk.in/internal/metrics"
)

// ingressLister adapts the controller's Store into the
// metrics.IngressLister contract so the IngressSampler can resolve
// `request.host` to `(scope, name)` without metrics importing
// controller (the cycle the sampler/adapter dance always avoids).
//
// `ListIngresses` is called on every IngressSampler tick (every 15s
// by default). The Store's List is cheap at any realistic ingress
// count — etcd by-prefix scan with no filtering — so polling beats
// wiring Watch events through another seam for v1. Move to event-
// driven invalidation if/when an ops shop ends up with thousands of
// ingresses.
type ingressLister struct {
	store Store
}

func newIngressLister(store Store) *ingressLister {
	return &ingressLister{store: store}
}

// ListIngresses enumerates every ingress manifest and returns the
// (host, scope, name) triples the host resolver indexes on. Manifests
// without a host (malformed or partially-applied) are skipped — we
// don't want to seed the map with an empty-string key.
//
// The context timeout caps cold etcd reads at 3s. The sampler logs
// and continues with the previously-cached map on failure, so a
// transient store outage doesn't drop all HTTP metrics — they just
// stop reflecting NEW ingress declarations until the store comes back.
func (l *ingressLister) ListIngresses() ([]metrics.IngressBinding, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	manifests, err := l.store.List(ctx, KindIngress)
	if err != nil {
		return nil, err
	}

	out := make([]metrics.IngressBinding, 0, len(manifests))

	for _, m := range manifests {
		host, hostErr := ingressHost(m)
		if hostErr != nil || host == "" {
			continue
		}

		out = append(out, metrics.IngressBinding{
			Host:  host,
			Scope: m.Scope,
			Name:  m.Name,
		})
	}

	return out, nil
}
