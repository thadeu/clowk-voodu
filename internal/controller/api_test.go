package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAPI(t *testing.T) (*API, *memStore) {
	t.Helper()

	store := newMemStore()

	return &API{Store: store, Version: "test"}, store
}

func TestApplyPostSingleManifest(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	body := `{"kind":"deployment","scope":"test","name":"api","spec":{"image":"x:1"}}`

	resp, err := http.Post(ts.URL+"/apply", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	got, err := store.Get(t.Context(), KindDeployment, "test", "api")
	if err != nil || got == nil {
		t.Fatalf("manifest not stored: %v", err)
	}

	if got.Metadata == nil || got.Metadata.Revision == 0 {
		t.Errorf("metadata not populated: %+v", got.Metadata)
	}
}

func TestApplyPostArrayOfManifests(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	body := `[
		{"kind":"deployment","scope":"test","name":"api","spec":{}},
		{"kind":"statefulset","scope":"data","name":"pg","spec":{"image":"postgres:15"}}
	]`

	resp, err := http.Post(ts.URL+"/apply", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	list, _ := store.ListAll(t.Context())
	if len(list) != 2 {
		t.Errorf("expected 2 manifests, got %d", len(list))
	}
}

func TestApplyRejectsUnknownKind(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/apply", "application/json", strings.NewReader(`{"kind":"potato","name":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", resp.StatusCode)
	}
}

func TestApplyGetListsAll(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Scope: "test", Name: "api"})
	_, _ = store.Put(t.Context(), &Manifest{Kind: KindStatefulset, Scope: "data", Name: "pg", Spec: json.RawMessage(`{"image":"postgres:15"}`)})

	resp, err := http.Get(ts.URL + "/apply")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var env struct {
		Status string
		Data   []Manifest
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if env.Status != "ok" || len(env.Data) != 2 {
		t.Errorf("unexpected response: %+v", env)
	}
}

func TestApplyDeleteRemovesManifest(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Scope: "test", Name: "api"})

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/apply?kind=deployment&name=api", nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	got, _ := store.Get(t.Context(), KindDeployment, "test", "api")
	if got != nil {
		t.Errorf("manifest still present after delete: %+v", got)
	}
}

// TestApplyPruneRemovesMissing verifies that resources present under a
// (scope, kind) in etcd but absent from the input array get pruned, while
// resources in other scopes or other kinds are left untouched.
func TestApplyPruneRemovesMissing(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	seed := []*Manifest{
		{Kind: KindDeployment, Scope: "app-a", Name: "web"},
		{Kind: KindDeployment, Scope: "app-a", Name: "worker"},
		{Kind: KindDeployment, Scope: "app-b", Name: "api"},
		{Kind: KindIngress, Scope: "app-a", Name: "lb"},
	}

	for _, m := range seed {
		if _, err := store.Put(t.Context(), m); err != nil {
			t.Fatalf("seed %s/%s/%s: %v", m.Kind, m.Scope, m.Name, err)
		}
	}

	// Apply only `deployment app-a/web`. Expectation:
	//   - app-a/worker is pruned (same kind+scope, missing from input)
	//   - app-b/api is kept (different scope entirely — not touched)
	//   - app-a/lb is kept (different kind; prune is per-(scope,kind))
	body := `[{"kind":"deployment","scope":"app-a","name":"web","spec":{}}]`

	resp, err := http.Post(ts.URL+"/apply", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	// Assert the surviving set.
	got, _ := store.Get(t.Context(), KindDeployment, "app-a", "web")
	if got == nil {
		t.Error("app-a/web should still exist (it was in the input)")
	}

	if gone, _ := store.Get(t.Context(), KindDeployment, "app-a", "worker"); gone != nil {
		t.Error("app-a/worker should have been pruned")
	}

	if kept, _ := store.Get(t.Context(), KindDeployment, "app-b", "api"); kept == nil {
		t.Error("app-b/api should have been kept (different scope)")
	}

	if kept, _ := store.Get(t.Context(), KindIngress, "app-a", "lb"); kept == nil {
		t.Error("app-a/lb should have been kept (different kind; per-(scope,kind) prune)")
	}
}

// TestApplyDryRunDoesNotPersist is the core contract of ?dry_run=true:
// the store must end the call byte-identical to how it started, but the
// response still describes what would have happened. This is what
// `voodu diff` relies on to show accurate plans without side effects.
func TestApplyDryRunDoesNotPersist(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	// Seed an existing deployment with a known spec so we can compare
	// before/after and confirm dry-run didn't overwrite.
	_, _ = store.Put(t.Context(), &Manifest{
		Kind:  KindDeployment,
		Scope: "app",
		Name:  "web",
		Spec:  json.RawMessage(`{"replicas":2}`),
	})

	// Also seed a sibling that would be pruned by a non-dry-run apply
	// declaring only `web`.
	_, _ = store.Put(t.Context(), &Manifest{
		Kind:  KindDeployment,
		Scope: "app",
		Name:  "worker",
	})

	body := `[{"kind":"deployment","scope":"app","name":"web","spec":{"replicas":5}}]`

	resp, err := http.Post(ts.URL+"/apply?dry_run=true", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	// Store assertions — nothing changed.
	got, _ := store.Get(t.Context(), KindDeployment, "app", "web")
	if got == nil || string(got.Spec) != `{"replicas":2}` {
		t.Errorf("dry-run mutated store: got spec %s", got.Spec)
	}

	if got, _ := store.Get(t.Context(), KindDeployment, "app", "worker"); got == nil {
		t.Error("dry-run pruned worker despite dry_run=true")
	}

	// Response shape — must include current (for client diff) and
	// the pruned list (what would be removed).
	var env struct {
		Data struct {
			Applied []*Manifest `json:"applied"`
			Current []*Manifest `json:"current"`
			Pruned  []string    `json:"pruned"`
			DryRun  bool        `json:"dry_run"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !env.Data.DryRun {
		t.Error("response missing dry_run=true marker")
	}

	if len(env.Data.Applied) != 1 || len(env.Data.Current) != 1 {
		t.Fatalf("expected 1 applied/current, got %d/%d", len(env.Data.Applied), len(env.Data.Current))
	}

	if string(env.Data.Applied[0].Spec) != `{"replicas":5}` {
		t.Errorf("applied[0].spec = %s, want replicas:5 (the desired)", env.Data.Applied[0].Spec)
	}

	if env.Data.Current[0] == nil || string(env.Data.Current[0].Spec) != `{"replicas":2}` {
		t.Errorf("current[0].spec = %v, want replicas:2 (the before)", env.Data.Current[0])
	}

	if len(env.Data.Pruned) != 1 || env.Data.Pruned[0] != "deployment/app/worker" {
		t.Errorf("pruned = %v, want [deployment/app/worker]", env.Data.Pruned)
	}
}

// TestApplyDryRunNewResourceHasNilCurrent: a resource that doesn't
// exist yet should still round-trip through dry-run with current[i] =
// null — that's how the client-side renderer knows to print the `+`
// marker instead of a modification diff.
func TestApplyDryRunNewResourceHasNilCurrent(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	body := `[{"kind":"deployment","scope":"app","name":"brand-new","spec":{"replicas":1}}]`

	resp, err := http.Post(ts.URL+"/apply?dry_run=true", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var env struct {
		Data struct {
			Current []*Manifest `json:"current"`
		} `json:"data"`
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if len(env.Data.Current) != 1 || env.Data.Current[0] != nil {
		t.Errorf("expected current=[nil], got %v", env.Data.Current)
	}
}

// TestApplyNoPruneKeepsSiblings covers the shared-scope escape hatch:
// when several independent applies (different repos, different CI
// pipelines) each declare only a slice of the same (scope, kind),
// prune=false prevents them from clobbering each other's resources.
func TestApplyNoPruneKeepsSiblings(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	// Simulate four independent repos that have already each applied
	// their own deployment into the shared "clowk" scope.
	seed := []*Manifest{
		{Kind: KindDeployment, Scope: "clowk", Name: "app"},
		{Kind: KindDeployment, Scope: "clowk", Name: "lp"},
		{Kind: KindDeployment, Scope: "clowk", Name: "api"},
		{Kind: KindDeployment, Scope: "clowk", Name: "jobs"},
	}

	for _, m := range seed {
		if _, err := store.Put(t.Context(), m); err != nil {
			t.Fatalf("seed %s/%s/%s: %v", m.Kind, m.Scope, m.Name, err)
		}
	}

	// clowk-lp repo re-applies only its own deployment — but with
	// ?prune=false, the sibling deployments owned by other repos must
	// survive.
	body := `[{"kind":"deployment","scope":"clowk","name":"lp","spec":{}}]`

	resp, err := http.Post(ts.URL+"/apply?prune=false", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	for _, name := range []string{"app", "lp", "api", "jobs"} {
		if got, _ := store.Get(t.Context(), KindDeployment, "clowk", name); got == nil {
			t.Errorf("clowk/%s was pruned despite ?prune=false", name)
		}
	}
}

// TestApplyAllowsCrossScopeNameReuse verifies two scopes can share a
// deployment name. Identity on disk (container slots, image tags, env
// files, release dirs) is keyed by AppID(scope, name) = "<scope>-<name>",
// so there is no slot collision at reconcile time.
func TestApplyAllowsCrossScopeNameReuse(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Scope: "app-a", Name: "web"})

	body := `[{"kind":"deployment","scope":"app-b","name":"web","spec":{}}]`

	resp, err := http.Post(ts.URL+"/apply", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 got %d", resp.StatusCode)
	}

	if got, _ := store.Get(t.Context(), KindDeployment, "app-a", "web"); got == nil {
		t.Error("app-a/web should still exist — it wasn't in this apply's (scope,kind)")
	}

	if got, _ := store.Get(t.Context(), KindDeployment, "app-b", "web"); got == nil {
		t.Error("app-b/web should have been written")
	}
}

func TestApplyDelete404OnMissing(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/apply?kind=deployment&name=ghost", nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 got %d", resp.StatusCode)
	}
}

func TestExecReturns404WhenPluginSystemDisabled(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	body := `{"args":["postgres","create","main"]}`

	resp, err := http.Post(ts.URL+"/plugins/exec", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}

	var env envelope

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if env.Status != "error" || !strings.Contains(env.Error, "no plugin registered") {
		t.Errorf("expected no-plugin error, got %+v", env)
	}
}

func TestHealthReportsVersion(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got map[string]string

	_ = json.NewDecoder(resp.Body).Decode(&got)

	if got["status"] != "ok" || got["version"] != "test" {
		t.Errorf("bad health response: %+v", got)
	}
}

