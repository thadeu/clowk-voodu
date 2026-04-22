package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

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
