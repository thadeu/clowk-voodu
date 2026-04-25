package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/docker"
)

// fakePodsLister returns a pre-canned pod list. Tests build it inline
// so we don't need to stub the docker CLI to exercise the HTTP layer.
//
// Implements PodDescriber as well so the same fake can stub both
// /pods and /pods/{name} — keeping describe / list tests parallel.
type fakePodsLister struct {
	pods []Pod
	err  error

	// describe lookup table keyed by container name. Tests populate
	// this for the GetPod path; ListPods ignores it.
	details   map[string]*PodDetail
	getErr    error
	getCalled []string
}

func (f *fakePodsLister) ListPods() ([]Pod, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.pods, nil
}

func (f *fakePodsLister) GetPod(name string) (*PodDetail, error) {
	f.getCalled = append(f.getCalled, name)

	if f.getErr != nil {
		return nil, f.getErr
	}

	d, ok := f.details[name]
	if !ok {
		return nil, nil
	}

	return d, nil
}

func TestPodsGetReturnsList(t *testing.T) {
	api, _ := newTestAPI(t)

	api.Pods = &fakePodsLister{
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

	api.Pods = &fakePodsLister{
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

	api.Pods = &fakePodsLister{
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

	api.Pods = &fakePodsLister{
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

	api.Pods = &fakePodsLister{pods: nil}

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

// TestPodDescribeReturnsRichDetail covers the happy path: a known
// container name resolves through the describer and the rich blob
// (state, command, networks, env) round-trips through the JSON
// envelope intact.
func TestPodDescribeReturnsRichDetail(t *testing.T) {
	api, _ := newTestAPI(t)

	api.Pods = &fakePodsLister{
		details: map[string]*PodDetail{
			"test-web.a3f9": {
				Pod: Pod{
					Name: "test-web.a3f9", Kind: "deployment", Scope: "test",
					ResourceName: "web", ReplicaID: "a3f9", Image: "vd-web:latest",
					Running: true, Status: "running",
				},
				ID: "deadbeefcafe1234567890",
				State: docker.ContainerState{
					Status: "running", Running: true,
					StartedAt: "2026-04-25T10:00:00Z",
				},
				Command:       []string{"/bin/sh", "-c", "node server.js"},
				WorkingDir:    "/app",
				Env:           map[string]string{"PORT": "3000"},
				Labels:        map[string]string{"voodu.kind": "deployment"},
				RestartPolicy: "unless-stopped",
				Networks: map[string]docker.ContainerNetwork{
					"voodu0": {IPAddress: "172.20.0.5", Gateway: "172.20.0.1"},
				},
				Mounts: []docker.ContainerMount{
					{Type: "bind", Source: "/srv/data", Destination: "/data", RW: true},
				},
				Ports: []docker.ContainerPort{
					{Container: "3000/tcp", HostIP: "127.0.0.1", HostPort: "8080"},
				},
			},
		},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/test-web.a3f9")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d want 200", resp.StatusCode)
	}

	var env struct {
		Status string
		Data   struct {
			Pod *PodDetail `json:"pod"`
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if env.Status != "ok" {
		t.Errorf("status=%q want ok", env.Status)
	}

	if env.Data.Pod == nil {
		t.Fatal("pod is nil")
	}

	got := env.Data.Pod

	if got.Pod.Name != "test-web.a3f9" {
		t.Errorf("name=%q", got.Pod.Name)
	}

	if got.State.StartedAt != "2026-04-25T10:00:00Z" {
		t.Errorf("started_at=%q", got.State.StartedAt)
	}

	if v := got.Env["PORT"]; v != "3000" {
		t.Errorf("env[PORT]=%q want 3000", v)
	}

	if _, ok := got.Networks["voodu0"]; !ok {
		t.Error("networks should contain voodu0")
	}

	if len(got.Ports) != 1 || got.Ports[0].HostPort != "8080" {
		t.Errorf("ports=%+v", got.Ports)
	}

	if len(got.Mounts) != 1 || got.Mounts[0].Destination != "/data" {
		t.Errorf("mounts=%+v", got.Mounts)
	}
}

// TestPodDescribeReturns404OnUnknownContainer makes sure typos /
// stale references get a clean 404 and not a 500 traceback.
func TestPodDescribeReturns404OnUnknownContainer(t *testing.T) {
	api, _ := newTestAPI(t)

	api.Pods = &fakePodsLister{details: map[string]*PodDetail{}}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/missing.0000")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d want 404", resp.StatusCode)
	}

	var env envelope
	_ = json.NewDecoder(resp.Body).Decode(&env)

	if env.Status != "error" {
		t.Errorf("status=%q want error", env.Status)
	}

	if !strings.Contains(env.Error, "missing.0000") {
		t.Errorf("error should name missing pod, got %q", env.Error)
	}
}

// TestPodDescribeRejectsHostileName guards against names with slashes
// or whitespace from sneaking past PathValue and confusing docker.
func TestPodDescribeRejectsHostileName(t *testing.T) {
	api, _ := newTestAPI(t)

	api.Pods = &fakePodsLister{}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	// A leading dot would be ambiguous with a hidden path component
	// and is never a valid docker container name. The handler refuses
	// before invoking the describer.
	resp, err := http.Get(ts.URL + "/pods/.hidden")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

// TestPodDescribeReturns503WhenNoDescriber locks in the
// reduced-capability behaviour: an API wired with a lister that
// doesn't satisfy PodDescriber should fail loud, not silently skip
// the rich detail.
func TestPodDescribeReturns503WhenNoDescriber(t *testing.T) {
	api, _ := newTestAPI(t)

	// listOnlyPods satisfies PodsLister but not PodDescriber, mimicking
	// a future remote-aggregator that can list pods but can't inspect
	// containers on the local host.
	api.Pods = listOnlyPods{}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/pods/test-web.a3f9")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", resp.StatusCode)
	}
}

// listOnlyPods implements PodsLister without PodDescriber so we can
// exercise the "describer not configured" branch.
type listOnlyPods struct{}

func (listOnlyPods) ListPods() ([]Pod, error) { return nil, nil }

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
