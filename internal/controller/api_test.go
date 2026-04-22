package controller

import (
	"bytes"
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

	body := `{"kind":"deployment","name":"api","spec":{"image":"x:1"}}`

	resp, err := http.Post(ts.URL+"/apply", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	got, err := store.Get(t.Context(), KindDeployment, "api")
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
		{"kind":"deployment","name":"api","spec":{}},
		{"kind":"database","name":"main","spec":{"engine":"postgres"}}
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

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Name: "api"})
	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDatabase, Name: "main"})

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

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Name: "api"})

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/apply?kind=deployment&name=api", nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	got, _ := store.Get(t.Context(), KindDeployment, "api")
	if got != nil {
		t.Errorf("manifest still present after delete: %+v", got)
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

func TestExecReturns404BecausePluginsAreM5(t *testing.T) {
	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	body := `{"args":["postgres","create","main"]}`

	resp, err := http.Post(ts.URL+"/exec", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}

	var env envelope

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if env.Status != "error" || !strings.Contains(env.Error, "M5") {
		t.Errorf("expected M5 pointer, got %+v", env)
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

func TestStatusReturnsDesiredAndActual(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Name: "api"})

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)

	if !strings.Contains(buf.String(), "desired") || !strings.Contains(buf.String(), "actual") {
		t.Errorf("status body missing keys: %s", buf.String())
	}
}
