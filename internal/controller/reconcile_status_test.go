package controller

import (
	"encoding/json"
	"fmt"
	"testing"
)

// ingressTestManifest builds a minimal valid ingress manifest. service
// == "" exercises the blank-fill-from-name path in
// retriggerDependentIngresses.
func ingressTestManifest(scope, name, service string) *Manifest {
	spec, _ := json.Marshal(ingressSpec{Host: name + ".example.com", Service: service})

	return &Manifest{Kind: KindIngress, Scope: scope, Name: name, Spec: spec}
}

// A failed ingress reconcile used to leave NO status — `vd describe`
// showed "(no status recorded yet)" and the operator had to guess. Now
// the failure is persisted on IngressStatus.
func TestRecordReconcileResult_IngressPersistsError(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()

	ev := WatchEvent{Type: WatchPut, Kind: KindIngress, Scope: "lp2", Name: "web"}

	recordReconcileResult(ctx, store, ev, fmt.Errorf("no live replicas yet"), nil)

	raw, _ := store.GetStatus(ctx, KindIngress, AppID("lp2", "web"))
	if raw == nil {
		t.Fatal("expected an ingress status after a failed reconcile, got none")
	}

	var st IngressStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode ingress status: %v", err)
	}

	if st.LastReconcileError != "no live replicas yet" {
		t.Errorf("LastReconcileError = %q, want the reconcile error", st.LastReconcileError)
	}

	if st.LastReconcileAt.IsZero() {
		t.Error("LastReconcileAt should be stamped on a recorded failure")
	}
}

// On a later success the handler rewrites Plugin/Data; recordReconcile
// must clear the stale error WITHOUT clobbering those fields.
func TestRecordReconcileResult_IngressClearsErrorPreservingData(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()
	app := AppID("lp2", "web")

	seed, _ := json.Marshal(IngressStatus{
		Plugin:             "caddy",
		Data:               map[string]any{"host": "web.example.com"},
		LastReconcileError: "stale failure from a prior attempt",
	})
	_ = store.PutStatus(ctx, KindIngress, app, seed)

	ev := WatchEvent{Type: WatchPut, Kind: KindIngress, Scope: "lp2", Name: "web"}

	recordReconcileResult(ctx, store, ev, nil, nil)

	raw, _ := store.GetStatus(ctx, KindIngress, app)

	var st IngressStatus
	_ = json.Unmarshal(raw, &st)

	if st.LastReconcileError != "" {
		t.Errorf("error should be cleared on success, got %q", st.LastReconcileError)
	}

	if st.Plugin != "caddy" {
		t.Errorf("plugin clobbered on success: %q", st.Plugin)
	}

	if st.Data["host"] != "web.example.com" {
		t.Errorf("plugin data clobbered on success: %+v", st.Data)
	}
}

// A deployment success must re-Put an ingress that routes to it and is
// stuck (no status recorded). The re-Put bumps the manifest revision,
// which the reconciler picks up through the regular watch path.
func TestRetriggerDependentIngresses_RePutsBrokenIngress(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()

	_, _ = store.Put(ctx, &Manifest{Kind: KindDeployment, Scope: "lp2", Name: "web", Spec: json.RawMessage(`{}`)})
	_, _ = store.Put(ctx, ingressTestManifest("lp2", "web", "web"))

	before, _ := store.Get(ctx, KindIngress, "lp2", "web")
	revBefore := before.Metadata.Revision

	retriggerDependentIngresses(ctx, store, "lp2", "web", nil)

	after, _ := store.Get(ctx, KindIngress, "lp2", "web")
	if after.Metadata.Revision <= revBefore {
		t.Errorf("broken ingress should be re-Put (revision bump): before=%d after=%d", revBefore, after.Metadata.Revision)
	}
}

// An ingress carrying a recorded reconcile error is also "broken" and
// must be re-triggered.
func TestRetriggerDependentIngresses_RePutsErroredIngress(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()
	app := AppID("lp2", "web")

	_, _ = store.Put(ctx, ingressTestManifest("lp2", "web", "web"))

	errored, _ := json.Marshal(IngressStatus{Plugin: "caddy", LastReconcileError: "boom"})
	_ = store.PutStatus(ctx, KindIngress, app, errored)

	before, _ := store.Get(ctx, KindIngress, "lp2", "web")
	revBefore := before.Metadata.Revision

	retriggerDependentIngresses(ctx, store, "lp2", "web", nil)

	after, _ := store.Get(ctx, KindIngress, "lp2", "web")
	if after.Metadata.Revision <= revBefore {
		t.Errorf("errored ingress should be re-Put: before=%d after=%d", revBefore, after.Metadata.Revision)
	}
}

// A healthy ingress (status present, no error) must NOT be re-Put —
// otherwise every steady-state deployment reconcile would churn the
// router.
func TestRetriggerDependentIngresses_SkipsHealthyIngress(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()
	app := AppID("lp2", "web")

	_, _ = store.Put(ctx, ingressTestManifest("lp2", "web", "web"))

	healthy, _ := json.Marshal(IngressStatus{Plugin: "caddy", Data: map[string]any{"host": "web.example.com"}})
	_ = store.PutStatus(ctx, KindIngress, app, healthy)

	before, _ := store.Get(ctx, KindIngress, "lp2", "web")
	revBefore := before.Metadata.Revision

	retriggerDependentIngresses(ctx, store, "lp2", "web", nil)

	after, _ := store.Get(ctx, KindIngress, "lp2", "web")
	if after.Metadata.Revision != revBefore {
		t.Errorf("healthy ingress should NOT be re-Put: before=%d after=%d", revBefore, after.Metadata.Revision)
	}
}

// An ingress routing to a DIFFERENT service must not be touched when
// some other deployment in the scope reconciles, even if it's broken.
func TestRetriggerDependentIngresses_SkipsNonMatchingService(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()

	_, _ = store.Put(ctx, ingressTestManifest("lp2", "edge", "api"))

	before, _ := store.Get(ctx, KindIngress, "lp2", "edge")
	revBefore := before.Metadata.Revision

	retriggerDependentIngresses(ctx, store, "lp2", "web", nil)

	after, _ := store.Get(ctx, KindIngress, "lp2", "edge")
	if after.Metadata.Revision != revBefore {
		t.Errorf("ingress for a different service should NOT be re-Put: before=%d after=%d", revBefore, after.Metadata.Revision)
	}
}

// service == "" defaults to the ingress name (the apply() blank-fill
// rule), so an ingress "web" with no explicit service still matches a
// deployment named "web".
func TestRetriggerDependentIngresses_ServiceDefaultsToName(t *testing.T) {
	store := newMemStore()
	ctx := t.Context()

	_, _ = store.Put(ctx, ingressTestManifest("lp2", "web", ""))

	before, _ := store.Get(ctx, KindIngress, "lp2", "web")
	revBefore := before.Metadata.Revision

	retriggerDependentIngresses(ctx, store, "lp2", "web", nil)

	after, _ := store.Get(ctx, KindIngress, "lp2", "web")
	if after.Metadata.Revision <= revBefore {
		t.Errorf("ingress with blank service should default to its name and match: before=%d after=%d", revBefore, after.Metadata.Revision)
	}
}
