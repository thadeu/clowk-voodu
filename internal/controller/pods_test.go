package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakePodsLister returns a pre-canned pod list. Tests build it inline
// so we don't need to stub the docker CLI to exercise the HTTP layer.
type fakePodsLister struct {
	pods []Pod
	err  error
}

func (f fakePodsLister) ListPods() ([]Pod, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.pods, nil
}

func TestPodsGetReturnsList(t *testing.T) {
	api, _ := newTestAPI(t)

	api.Pods = fakePodsLister{
		pods: []Pod{
			{
				Name: "softphone-web.a3f9", Kind: "deployment", Scope: "softphone",
				ResourceName: "web", ReplicaID: "a3f9", Image: "softphone-web:latest",
				Running: true, Status: "Up 2 hours",
			},
			{
				Name: "softphone-web.bb01", Kind: "deployment", Scope: "softphone",
				ResourceName: "web", ReplicaID: "bb01", Image: "softphone-web:latest",
				Running: true, Status: "Up 5 minutes",
			},
		},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	var env struct {
		Status string
		Data   struct {
			Pods []Pod `json:"pods"`
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if env.Status != "ok" {
		t.Errorf("status=%q want ok", env.Status)
	}

	if len(env.Data.Pods) != 2 {
		t.Fatalf("got %d pods, want 2", len(env.Data.Pods))
	}

	if env.Data.Pods[0].ReplicaID != "a3f9" {
		t.Errorf("first replica id = %q want a3f9", env.Data.Pods[0].ReplicaID)
	}
}

func TestPodsGetFiltersByKind(t *testing.T) {
	api, _ := newTestAPI(t)

	api.Pods = fakePodsLister{
		pods: []Pod{
			{Name: "a.0001", Kind: "deployment", Scope: "x", ResourceName: "a", ReplicaID: "0001"},
			{Name: "b.0002", Kind: "job", Scope: "x", ResourceName: "b", ReplicaID: "0002"},
			{Name: "c.0003", Kind: "deployment", Scope: "y", ResourceName: "c", ReplicaID: "0003"},
		},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods?kind=deployment")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var env struct {
		Data struct {
			Pods []Pod `json:"pods"`
		}
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if len(env.Data.Pods) != 2 {
		t.Fatalf("got %d pods, want 2 deployments", len(env.Data.Pods))
	}

	for _, p := range env.Data.Pods {
		if p.Kind != "deployment" {
			t.Errorf("got kind=%q in deployment-only filter", p.Kind)
		}
	}
}

func TestPodsGetFiltersByScope(t *testing.T) {
	api, _ := newTestAPI(t)

	api.Pods = fakePodsLister{
		pods: []Pod{
			{Name: "a.0001", Kind: "deployment", Scope: "x", ResourceName: "a"},
			{Name: "b.0002", Kind: "deployment", Scope: "y", ResourceName: "b"},
		},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods?scope=x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var env struct {
		Data struct {
			Pods []Pod `json:"pods"`
		}
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if len(env.Data.Pods) != 1 {
		t.Fatalf("got %d pods, want 1 in scope=x", len(env.Data.Pods))
	}

	if env.Data.Pods[0].Scope != "x" {
		t.Errorf("got scope=%q want x", env.Data.Pods[0].Scope)
	}
}

func TestPodsGetFiltersByName(t *testing.T) {
	api, _ := newTestAPI(t)

	api.Pods = fakePodsLister{
		pods: []Pod{
			{Name: "a.0001", Kind: "deployment", Scope: "x", ResourceName: "web"},
			{Name: "a.0002", Kind: "deployment", Scope: "x", ResourceName: "web"},
			{Name: "a.0003", Kind: "deployment", Scope: "x", ResourceName: "worker"},
		},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods?name=web")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var env struct {
		Data struct {
			Pods []Pod `json:"pods"`
		}
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if len(env.Data.Pods) != 2 {
		t.Fatalf("got %d pods, want 2 web replicas", len(env.Data.Pods))
	}
}

func TestPodsGetEmpty(t *testing.T) {
	api, _ := newTestAPI(t)

	api.Pods = fakePodsLister{pods: nil}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	var env struct {
		Data struct {
			Pods []Pod `json:"pods"`
		}
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if len(env.Data.Pods) != 0 {
		t.Errorf("expected empty list, got %d", len(env.Data.Pods))
	}
}

func TestPodsGetRejectsNonGet(t *testing.T) {
	api, _ := newTestAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/pods", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status %d want 405", resp.StatusCode)
	}
}

func TestSortPodsScopedFirst(t *testing.T) {
	pods := []Pod{
		{Kind: "", Name: "legacy-1"},
		{Kind: "deployment", Scope: "", ResourceName: "z-unscoped"},
		{Kind: "deployment", Scope: "softphone", ResourceName: "web", ReplicaID: "bb01"},
		{Kind: "deployment", Scope: "softphone", ResourceName: "web", ReplicaID: "a3f9"},
		{Kind: "deployment", Scope: "alpha", ResourceName: "api"},
	}

	sortPods(pods)

	// Expected order: alpha/api → softphone/web/a3f9 → softphone/web/bb01
	// → unscoped → legacy.
	if pods[0].Scope != "alpha" {
		t.Errorf("pods[0]=%q want alpha", pods[0].Scope)
	}

	if pods[1].ReplicaID != "a3f9" {
		t.Errorf("pods[1] replica=%q want a3f9", pods[1].ReplicaID)
	}

	if pods[2].ReplicaID != "bb01" {
		t.Errorf("pods[2] replica=%q want bb01", pods[2].ReplicaID)
	}

	if pods[3].ResourceName != "z-unscoped" {
		t.Errorf("pods[3]=%q want z-unscoped (unscoped before legacy)", pods[3].ResourceName)
	}

	if pods[4].Name != "legacy-1" {
		t.Errorf("pods[4]=%q want legacy-1 (legacy last)", pods[4].Name)
	}
}
