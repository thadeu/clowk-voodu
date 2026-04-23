package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// seedManifest persists a Manifest in the store. Used by ingress tests
// to satisfy the upstream-existence check that IngressHandler now
// enforces (an ingress that names a service/deployment nobody applied
// is rejected at reconcile time).
func seedManifest(t *testing.T, store Store, kind Kind, name string, spec any) {
	t.Helper()

	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	scope := ""
	if IsScoped(kind) {
		scope = "test"
	}

	if _, err := store.Put(context.Background(), &Manifest{
		Kind:  kind,
		Scope: scope,
		Name:  name,
		Spec:  json.RawMessage(raw),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestServiceHandler_PersistsMetadata(t *testing.T) {
	store := newMemStore()

	h := &ServiceHandler{Store: store, Log: quietLogger()}

	ev := putEvent(t, KindService, "api", serviceSpec{
		Target: "api-deployment",
		Port:   8080,
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	raw, _ := store.GetStatus(context.Background(), KindService, "api")
	if raw == nil {
		t.Fatal("service status not persisted")
	}

	var status ServiceStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}

	if status.Data["target"] != "api-deployment" {
		t.Errorf("target missing: %+v", status.Data)
	}

	// Port is json-numbered; check both the nominal types we expect from
	// round-tripping through json.Marshal → Unmarshal into any.
	switch p := status.Data["port"].(type) {
	case float64:
		if int(p) != 8080 {
			t.Errorf("port: %v", p)
		}
	default:
		t.Errorf("unexpected port type %T: %v", p, p)
	}
}

func TestServiceHandler_MissingTargetIsError(t *testing.T) {
	store := newMemStore()

	h := &ServiceHandler{Store: store, Log: quietLogger()}

	ev := putEvent(t, KindService, "bad", serviceSpec{})

	err := h.Handle(context.Background(), ev)
	if err == nil {
		t.Fatal("expected error when target is empty")
	}

	if !strings.Contains(err.Error(), "target") {
		t.Errorf("error should mention missing target: %v", err)
	}
}

func TestServiceHandler_DeleteClearsStatus(t *testing.T) {
	store := newMemStore()

	// Seed.
	pre, _ := json.Marshal(ServiceStatus{Data: map[string]any{"target": "api"}})
	_ = store.PutStatus(context.Background(), KindService, "api", pre)

	h := &ServiceHandler{Store: store, Log: quietLogger()}

	if err := h.Handle(context.Background(), WatchEvent{Type: WatchDelete, Kind: KindService, Name: "api"}); err != nil {
		t.Fatal(err)
	}

	raw, _ := store.GetStatus(context.Background(), KindService, "api")
	if raw != nil {
		t.Errorf("service status not cleared: %s", raw)
	}
}

func TestIngressHandler_ApplyDispatchesToPlugin(t *testing.T) {
	store := newMemStore()

	// The ingress references service "api" — resolveUpstream now requires
	// the target to exist, so seed it first.
	seedManifest(t, store, KindService, "api", serviceSpec{Target: "api", Port: 8080})

	inv := &fakeInvoker{
		results: map[string]*plugins.Result{
			"caddy.apply": envelopeResult(map[string]any{
				"url": "https://api.example.com",
			}),
		},
	}

	h := &IngressHandler{Store: store, Invoker: inv, Log: quietLogger()}

	ev := putEvent(t, KindIngress, "public", ingressSpec{
		Host:    "api.example.com",
		Service: "api",
		Port:    8080,
		TLS:     &ingressTLS{Enabled: true, Provider: "letsencrypt", Email: "ops@example.com"},
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	if len(inv.calls) != 1 {
		t.Fatalf("expected 1 plugin call, got %d", len(inv.calls))
	}

	call := inv.calls[0]

	if call.Plugin != "caddy" || call.Command != "apply" {
		t.Errorf("wrong plugin/command: %s/%s", call.Plugin, call.Command)
	}

	if call.Env[plugin.EnvIngressHost] != "api.example.com" || call.Env[plugin.EnvIngressService] != "api" {
		t.Errorf("env missing spec fields: %+v", call.Env)
	}

	if call.Env[plugin.EnvIngressTLS] != "true" || call.Env[plugin.EnvIngressTLSProvider] != "letsencrypt" {
		t.Errorf("tls env not forwarded: %+v", call.Env)
	}

	// Status should carry plugin name + envelope data (so
	// ${ref.ingress.public.url} resolves downstream).
	raw, _ := store.GetStatus(context.Background(), KindIngress, "public")
	if raw == nil {
		t.Fatal("ingress status not persisted")
	}

	var status IngressStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}

	if status.Plugin != "caddy" {
		t.Errorf("plugin name not recorded: %q", status.Plugin)
	}

	if status.Data["url"] != "https://api.example.com" {
		t.Errorf("url missing from status: %+v", status.Data)
	}
}

func TestIngressHandler_ApplyForwardsOnDemandTLS(t *testing.T) {
	store := newMemStore()

	seedManifest(t, store, KindService, "app", serviceSpec{Target: "app", Port: 3000})

	inv := &fakeInvoker{
		results: map[string]*plugins.Result{
			"caddy.apply": envelopeResult(map[string]any{"url": "https://*.clowk.in"}),
		},
	}

	h := &IngressHandler{Store: store, Invoker: inv, Log: quietLogger()}

	ev := putEvent(t, KindIngress, "wildcard", ingressSpec{
		Host:    "*.clowk.in",
		Service: "app",
		Port:    3000,
		TLS: &ingressTLS{
			Enabled:  true,
			Provider: "letsencrypt",
			Email:    "ssl@clowk.dev",
			OnDemand: true,
			Ask:      "http://app:3000/internal/allow_domain",
		},
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	call := inv.calls[0]

	if call.Env[plugin.EnvIngressTLSOnDemand] != "true" {
		t.Errorf("on_demand flag not forwarded: %+v", call.Env)
	}

	if call.Env[plugin.EnvIngressTLSAsk] != "http://app:3000/internal/allow_domain" {
		t.Errorf("ask URL not forwarded: %q", call.Env[plugin.EnvIngressTLSAsk])
	}
}

func TestIngressHandler_ServiceDefaultsToIngressName(t *testing.T) {
	// The overwhelming common case is `ingress "api" {}` paired with a
	// `deployment "api"` — service name matches ingress name. Requiring
	// `service = "api"` every time is pure boilerplate, so an omitted
	// service defaults to the ingress's own name. Explicit service
	// (cross-app routing) still wins.
	store := newMemStore()

	// Seed a deployment named "vd-web" so resolveUpstream's fail-fast
	// check passes. That's the app the ingress implicitly targets.
	seedManifest(t, store, KindDeployment, "vd-web", deploymentSpec{Image: "vd-web:latest"})

	inv := &fakeInvoker{
		results: map[string]*plugins.Result{
			"caddy.apply": envelopeResult(map[string]any{"url": "https://vd-web.lvh.me"}),
		},
	}

	h := &IngressHandler{Store: store, Invoker: inv, Log: quietLogger()}

	// No `Service` field — expect handler to substitute ev.Name ("vd-web").
	ev := putEvent(t, KindIngress, "vd-web", ingressSpec{
		Host: "vd-web.lvh.me",
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("ingress with defaulted service should succeed: %v", err)
	}

	if len(inv.calls) != 1 {
		t.Fatalf("expected 1 caddy.apply, got %d", len(inv.calls))
	}

	if got := inv.calls[0].Env[plugin.EnvIngressService]; got != "vd-web" {
		t.Errorf("defaulted service not forwarded to plugin: got %q, want %q", got, "vd-web")
	}
}

func TestIngressHandler_ExplicitServiceOverridesDefault(t *testing.T) {
	// Cross-app routing: ingress "public" exposes service "api". The
	// default-to-ingress-name shortcut must not clobber an explicit
	// service field, otherwise declarative intent gets silently lost.
	store := newMemStore()

	seedManifest(t, store, KindService, "api", serviceSpec{Target: "api", Port: 8080})

	inv := &fakeInvoker{
		results: map[string]*plugins.Result{
			"caddy.apply": envelopeResult(map[string]any{"url": "https://api.example.com"}),
		},
	}

	h := &IngressHandler{Store: store, Invoker: inv, Log: quietLogger()}

	ev := putEvent(t, KindIngress, "public", ingressSpec{
		Host:    "api.example.com",
		Service: "api",
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("explicit service should still work: %v", err)
	}

	if got := inv.calls[0].Env[plugin.EnvIngressService]; got != "api" {
		t.Errorf("explicit service got clobbered: got %q, want %q", got, "api")
	}
}

func TestIngressHandler_ForwardsLocations(t *testing.T) {
	// Path-based routing: the operator declares multiple `location` blocks
	// so one ingress can serve different path prefixes (classic case: API
	// v1 + v2 both hitting the same backend, or docs under /docs pointing
	// at a static-site container). Handler must forward them as a JSON
	// array in VOODU_INGRESS_LOCATIONS so the caddy plugin can generate
	// the matchers.
	store := newMemStore()

	seedManifest(t, store, KindService, "voodu-docs", serviceSpec{Target: "voodu-docs", Port: 80})

	inv := &fakeInvoker{
		results: map[string]*plugins.Result{
			"caddy.apply": envelopeResult(map[string]any{"url": "https://clowk.in"}),
		},
	}

	h := &IngressHandler{Store: store, Invoker: inv, Log: quietLogger()}

	ev := putEvent(t, KindIngress, "voodu-docs", ingressSpec{
		Host:    "clowk.in",
		Service: "voodu-docs",
		Locations: []ingressLocation{
			{Path: "/docs/voodu", Strip: false},
			{Path: "/api/v1", Strip: true},
		},
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("ingress with locations should succeed: %v", err)
	}

	raw := inv.calls[0].Env[plugin.EnvIngressLocations]
	if raw == "" {
		t.Fatal("VOODU_INGRESS_LOCATIONS not forwarded")
	}

	var got []ingressLocation
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("locations env not valid JSON: %v (raw=%q)", err, raw)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(got))
	}

	if got[0].Path != "/docs/voodu" || got[0].Strip {
		t.Errorf("first location wrong: %+v", got[0])
	}

	if got[1].Path != "/api/v1" || !got[1].Strip {
		t.Errorf("second location wrong (strip should propagate): %+v", got[1])
	}
}

func TestIngressHandler_OmitsLocationsEnvWhenEmpty(t *testing.T) {
	// Backward compat: older caddy plugin versions parse
	// VOODU_INGRESS_LOCATIONS as JSON unconditionally. An empty key would
	// make them fail on "" → unmarshal error. Skip the key entirely when
	// no locations are declared so those plugins see no difference.
	store := newMemStore()

	seedManifest(t, store, KindService, "api", serviceSpec{Target: "api", Port: 8080})

	inv := &fakeInvoker{
		results: map[string]*plugins.Result{
			"caddy.apply": envelopeResult(map[string]any{"url": "https://api.example.com"}),
		},
	}

	h := &IngressHandler{Store: store, Invoker: inv, Log: quietLogger()}

	ev := putEvent(t, KindIngress, "public", ingressSpec{
		Host:    "api.example.com",
		Service: "api",
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	if _, ok := inv.calls[0].Env[plugin.EnvIngressLocations]; ok {
		t.Errorf("VOODU_INGRESS_LOCATIONS must be absent when no location blocks declared")
	}
}

func TestIngressHandler_HostStillRequired(t *testing.T) {
	// Host has no sensible default — an ingress without host can't be
	// routed to. Handler must reject it with a clear error instead of
	// silently accepting a broken manifest.
	h := &IngressHandler{
		Store:   newMemStore(),
		Invoker: &fakeInvoker{},
		Log:     quietLogger(),
	}

	ev := putEvent(t, KindIngress, "vd-web", ingressSpec{})

	err := h.Handle(context.Background(), ev)
	if err == nil {
		t.Fatal("expected error for ingress missing host")
	}

	if !strings.Contains(err.Error(), "host is required") {
		t.Errorf("error message should mention host requirement, got: %v", err)
	}
}

func TestIngressHandler_RemoveCallsPluginAndClearsStatus(t *testing.T) {
	store := newMemStore()

	pre, _ := json.Marshal(IngressStatus{Plugin: "caddy", Data: map[string]any{"host": "x"}})
	_ = store.PutStatus(context.Background(), KindIngress, "public", pre)

	inv := &fakeInvoker{}

	h := &IngressHandler{Store: store, Invoker: inv, Log: quietLogger()}

	if err := h.Handle(context.Background(), WatchEvent{Type: WatchDelete, Kind: KindIngress, Name: "public"}); err != nil {
		t.Fatal(err)
	}

	if len(inv.calls) != 1 || inv.calls[0].Command != "remove" {
		t.Fatalf("remove not called: %+v", inv.calls)
	}

	raw, _ := store.GetStatus(context.Background(), KindIngress, "public")
	if raw != nil {
		t.Errorf("ingress status not cleared")
	}
}

// The generalised refLookup (see DeploymentHandler.refLookup) must
// resolve service and ingress refs the same way it resolves database
// refs — the uniform status envelope is what enables this.
func TestRefLookupResolvesServiceAndIngressStatus(t *testing.T) {
	store := newMemStore()

	svcStatus, _ := json.Marshal(ServiceStatus{Data: map[string]any{"target": "api"}})
	_ = store.PutStatus(context.Background(), KindService, "web", svcStatus)

	ingStatus, _ := json.Marshal(IngressStatus{Plugin: "caddy", Data: map[string]any{"url": "https://x"}})
	_ = store.PutStatus(context.Background(), KindIngress, "public", ingStatus)

	h := &DeploymentHandler{Store: store, Log: quietLogger()}

	lookup := h.refLookup(context.Background())

	if v, ok := lookup("service", "web", "target"); !ok || v != "api" {
		t.Errorf("service ref: got (%q, %v)", v, ok)
	}

	if v, ok := lookup("ingress", "public", "url"); !ok || v != "https://x" {
		t.Errorf("ingress ref: got (%q, %v)", v, ok)
	}
}

func TestIngressHandler_PortResolvedFromService(t *testing.T) {
	store := newMemStore()

	// Service declares port 8080; ingress omits it. The handler should
	// fill in 8080 before dispatching to the plugin.
	seedManifest(t, store, KindService, "api", serviceSpec{Target: "api", Port: 8080})

	inv := &fakeInvoker{
		results: map[string]*plugins.Result{
			"caddy.apply": envelopeResult(map[string]any{"url": "https://api.x"}),
		},
	}

	h := &IngressHandler{Store: store, Invoker: inv, Log: quietLogger()}

	ev := putEvent(t, KindIngress, "public", ingressSpec{
		Host:    "api.example.com",
		Service: "api",
		// Port intentionally omitted.
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	if len(inv.calls) != 1 {
		t.Fatalf("expected 1 plugin call, got %d", len(inv.calls))
	}

	if got := inv.calls[0].Env[plugin.EnvIngressPort]; got != "8080" {
		t.Errorf("port not resolved from service: env=%q", got)
	}
}

func TestIngressHandler_PortResolvedFromDeployment(t *testing.T) {
	store := newMemStore()

	// No service with this name — fall back to a deployment that
	// happens to share the name. "8080:3000" is the docker port map
	// syntax; we want the container side (3000).
	seedManifest(t, store, KindDeployment, "api", deploymentSpec{
		Image: "example/api:latest",
		Ports: []string{"8080:3000"},
	})

	inv := &fakeInvoker{
		results: map[string]*plugins.Result{
			"caddy.apply": envelopeResult(map[string]any{"url": "https://api.x"}),
		},
	}

	h := &IngressHandler{Store: store, Invoker: inv, Log: quietLogger()}

	ev := putEvent(t, KindIngress, "public", ingressSpec{
		Host:    "api.example.com",
		Service: "api",
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	if got := inv.calls[0].Env[plugin.EnvIngressPort]; got != "3000" {
		t.Errorf("port not resolved from deployment container side: env=%q", got)
	}
}

func TestIngressHandler_MissingTargetIsTransient(t *testing.T) {
	store := newMemStore()

	h := &IngressHandler{Store: store, Invoker: &fakeInvoker{}, Log: quietLogger()}

	ev := putEvent(t, KindIngress, "public", ingressSpec{
		Host:    "api.example.com",
		Service: "api",
		Port:    8080,
	})

	err := h.Handle(context.Background(), ev)
	if err == nil {
		t.Fatal("expected error when target does not exist")
	}

	// Must be Transient so the reconciler retries once the operator
	// applies the deployment/service. A hard error would require manual
	// re-apply of the ingress.
	if !isTransient(err) {
		t.Errorf("expected transient error, got %T: %v", err, err)
	}
}

func TestIngressHandler_ExplicitPortStillRequiresTarget(t *testing.T) {
	store := newMemStore()

	h := &IngressHandler{Store: store, Invoker: &fakeInvoker{}, Log: quietLogger()}

	// Even with an explicit port, routing to a non-existent target just
	// yields a 502 later. Reject at reconcile time instead.
	ev := putEvent(t, KindIngress, "public", ingressSpec{
		Host:    "api.example.com",
		Service: "api",
		Port:    9999,
	})

	if err := h.Handle(context.Background(), ev); err == nil {
		t.Fatal("expected error for missing target even with explicit port")
	}
}

func TestIngressHandler_PortDefaultsTo80(t *testing.T) {
	// Ingress is always HTTP routing (caddy-only) and every common
	// base image that fronts web traffic — caddy, nginx, httpd,
	// kestrel — listens on 80. So when nothing else resolves a port,
	// 80 is the sane default. Apps on weird ports (rails:3000,
	// flask:5000) declare port explicitly; default-wrong is loud
	// (502 / connection refused on first request) so this isn't a
	// silent misroute hazard.
	cases := []struct {
		name string
		seed func(t *testing.T, store Store)
	}{
		{
			name: "service exists with no port",
			seed: func(t *testing.T, s Store) {
				seedManifest(t, s, KindService, "api", serviceSpec{Target: "api"})
			},
		},
		{
			name: "deployment exists with no ports",
			seed: func(t *testing.T, s Store) {
				seedManifest(t, s, KindDeployment, "api", deploymentSpec{Image: "img:1"})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			tc.seed(t, store)

			inv := &fakeInvoker{
				results: map[string]*plugins.Result{
					"caddy.apply": envelopeResult(map[string]any{"url": "http://api.example.com"}),
				},
			}

			h := &IngressHandler{Store: store, Invoker: inv, Log: quietLogger()}

			ev := putEvent(t, KindIngress, "public", ingressSpec{
				Host:    "api.example.com",
				Service: "api",
			})

			if err := h.Handle(context.Background(), ev); err != nil {
				t.Fatalf("expected default port fallback, got error: %v", err)
			}

			if len(inv.calls) != 1 {
				t.Fatalf("expected plugin call, got %d", len(inv.calls))
			}

			if got := inv.calls[0].Env[plugin.EnvIngressPort]; got != "80" {
				t.Errorf("ingress port should default to 80, got %q", got)
			}
		})
	}
}

func TestFirstContainerPort(t *testing.T) {
	cases := []struct {
		in   []string
		want int
		ok   bool
	}{
		{nil, 0, false},
		{[]string{""}, 0, false},
		{[]string{"3000"}, 3000, true},
		{[]string{"8080:3000"}, 3000, true},
		{[]string{"3000/udp"}, 3000, true},
		{[]string{"8080:3000/tcp"}, 3000, true},
		{[]string{"not-a-port", "4000"}, 4000, true},
	}

	for _, c := range cases {
		got, ok := firstContainerPort(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("firstContainerPort(%v) = (%d, %v); want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
