package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakePuller records every Pull and lets a test script the local image
// id a ref resolves to before and after that pull. ids[ref] is what
// ImageID returns; after[ref], when set, replaces it once Pull has run
// — the shape of "the registry had something newer".
type fakePuller struct {
	mu sync.Mutex

	pulled []string
	ids    map[string]string
	after  map[string]string
	fail   map[string]error
}

func newFakePuller() *fakePuller {
	return &fakePuller{
		ids:   map[string]string{},
		after: map[string]string{},
		fail:  map[string]error{},
	}
}

func (f *fakePuller) Pull(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pulled = append(f.pulled, ref)

	if err, ok := f.fail[ref]; ok {
		return err
	}

	if id, ok := f.after[ref]; ok {
		f.ids[ref] = id
	}

	return nil
}

func (f *fakePuller) ImageID(ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.ids[ref], nil
}

func (f *fakePuller) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.pulled...)
}

// applyForce POSTs body to /apply with the given query and returns the
// response status plus the decoded `data` envelope.
func applyForce(t *testing.T, api *API, query, body string) (int, map[string]any) {
	t.Helper()

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/apply?"+query, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var env struct {
		Data map[string]any `json:"data"`
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	return resp.StatusCode, env.Data
}

// TestApplyForcePullsRegistryImages is the bug this flag exists for: CI
// overwrites an ECR tag, the HCL never changes, and without a pull the
// host keeps resolving the tag to the digest it cached on first deploy.
func TestApplyForcePullsRegistryImages(t *testing.T) {
	api, _ := newTestAPI(t)

	puller := newFakePuller()
	puller.ids["123.dkr.ecr.us-east-1.amazonaws.com/app:latest"] = "sha256:old"
	puller.after["123.dkr.ecr.us-east-1.amazonaws.com/app:latest"] = "sha256:new"
	api.Images = puller

	body := `{"kind":"deployment","scope":"test","name":"api","spec":{"image":"123.dkr.ecr.us-east-1.amazonaws.com/app:latest"}}`

	status, data := applyForce(t, api, "force=true", body)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}

	if got := puller.calls(); len(got) != 1 || got[0] != "123.dkr.ecr.us-east-1.amazonaws.com/app:latest" {
		t.Fatalf("pulled = %v, want the ECR ref exactly once", got)
	}

	pulls, ok := data["image_pulls"].([]any)
	if !ok || len(pulls) != 1 {
		t.Fatalf("image_pulls = %v, want one entry", data["image_pulls"])
	}

	entry, _ := pulls[0].(map[string]any)
	if updated, _ := entry["updated"].(bool); !updated {
		t.Errorf("updated = false, want true (the tag moved from sha256:old to sha256:new)")
	}
}

// TestApplyWithoutForceSkipsPull keeps the default apply cheap: a
// steady-state reconcile must not shell out to the registry.
func TestApplyWithoutForceSkipsPull(t *testing.T) {
	api, _ := newTestAPI(t)

	puller := newFakePuller()
	api.Images = puller

	status, data := applyForce(t, api, "", `{"kind":"deployment","scope":"test","name":"api","spec":{"image":"nginx:alpine"}}`)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}

	if got := puller.calls(); len(got) != 0 {
		t.Errorf("pulled = %v, want none without --force", got)
	}

	if _, present := data["image_pulls"]; present {
		t.Errorf("image_pulls present on a non-force apply: %v", data["image_pulls"])
	}
}

// TestApplyForceSkipsPullOnDryRun — `vd diff` renders a plan. Pulling
// gigabytes to answer "what would change?" would be a surprising cost,
// and mutating the host during a dry run is a contract break.
func TestApplyForceSkipsPullOnDryRun(t *testing.T) {
	api, _ := newTestAPI(t)

	puller := newFakePuller()
	api.Images = puller

	status, _ := applyForce(t, api, "force=true&dry_run=true", `{"kind":"deployment","scope":"test","name":"api","spec":{"image":"nginx:alpine"}}`)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}

	if got := puller.calls(); len(got) != 0 {
		t.Errorf("pulled = %v, want none on dry-run", got)
	}
}

// TestApplyForceSkipsBuildModeDeployments — build-mode has no image to
// pull; its half of --force is the rebuild receive-pack already did.
func TestApplyForceSkipsBuildModeDeployments(t *testing.T) {
	api, _ := newTestAPI(t)

	puller := newFakePuller()
	api.Images = puller

	status, _ := applyForce(t, api, "force=true", `{"kind":"deployment","scope":"test","name":"web","spec":{"replicas":2}}`)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}

	if got := puller.calls(); len(got) != 0 {
		t.Errorf("pulled = %v, want none for a build-mode deployment", got)
	}
}

// TestApplyForcePullFailureAbortsBeforeStore — force asked for the
// newest bytes and we can neither fetch them nor run what's on the
// host. Failing before the first Put keeps desired state untouched, so
// re-applying after fixing the credentials is a clean retry.
func TestApplyForcePullFailureAbortsBeforeStore(t *testing.T) {
	api, store := newTestAPI(t)

	puller := newFakePuller()
	puller.fail["ghcr.io/acme/app:1"] = errors.New("unauthorized")
	api.Images = puller

	status, _ := applyForce(t, api, "force=true", `{"kind":"deployment","scope":"test","name":"api","spec":{"image":"ghcr.io/acme/app:1"}}`)

	if status != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", status)
	}

	got, err := store.Get(t.Context(), KindDeployment, "test", "api")
	if err != nil {
		t.Fatal(err)
	}

	if got != nil {
		t.Errorf("manifest was stored despite the failed pull: %+v", got)
	}
}

// TestApplyForcePullFailureWithLocalCopyWarns — a registry blip must
// not take down an apply the host can already serve. Same posture
// `docker run` takes: the local copy carries it, loudly.
func TestApplyForcePullFailureWithLocalCopyWarns(t *testing.T) {
	api, store := newTestAPI(t)

	puller := newFakePuller()
	puller.ids["ghcr.io/acme/app:1"] = "sha256:cached"
	puller.fail["ghcr.io/acme/app:1"] = errors.New("registry unreachable")
	api.Images = puller

	status, data := applyForce(t, api, "force=true", `{"kind":"deployment","scope":"test","name":"api","spec":{"image":"ghcr.io/acme/app:1"}}`)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200", status)
	}

	pulls, ok := data["image_pulls"].([]any)
	if !ok || len(pulls) != 1 {
		t.Fatalf("image_pulls = %v, want one entry", data["image_pulls"])
	}

	entry, _ := pulls[0].(map[string]any)
	if warning, _ := entry["warning"].(string); !strings.Contains(warning, "registry unreachable") {
		t.Errorf("warning = %q, want it to carry the pull error", warning)
	}

	got, err := store.Get(t.Context(), KindDeployment, "test", "api")
	if err != nil || got == nil {
		t.Errorf("manifest not stored despite a usable local image: %v", err)
	}
}

// TestCollectPullableImagesDedupes — two replicas of the same service
// image (a deployment and its cronjob twin) must not pull twice.
func TestCollectPullableImages(t *testing.T) {
	manifests := []*Manifest{
		{Kind: KindDeployment, Scope: "s", Name: "web", Spec: json.RawMessage(`{"image":"app:1"}`)},
		{Kind: KindCronJob, Scope: "s", Name: "sweep", Spec: json.RawMessage(`{"image":"app:1"}`)},
		{Kind: KindStatefulset, Scope: "s", Name: "pg", Spec: json.RawMessage(`{"image":"postgres:16"}`)},
		{Kind: KindJob, Scope: "s", Name: "seed", Spec: json.RawMessage(`{"image":"app:1"}`)},
		{Kind: KindDeployment, Scope: "s", Name: "src", Spec: json.RawMessage(`{"replicas":1}`)},
		{Kind: KindIngress, Scope: "s", Name: "in", Spec: json.RawMessage(`{"image":"never:pulled"}`)},
	}

	got := collectPullableImages(manifests)

	want := []string{"app:1", "postgres:16"}
	if len(got) != len(want) {
		t.Fatalf("images = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("images = %v, want %v (declaration order)", got, want)
		}
	}
}
