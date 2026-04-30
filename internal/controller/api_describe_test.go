package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// describeEnvelope mirrors the /describe response shape so tests can
// decode the data block without redefining it inline at every call
// site. Manifest is a pointer so an absent value still decodes — the
// 404 path doesn't return one and we test that branch.
type describeEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		Manifest *Manifest       `json:"manifest"`
		Status   json.RawMessage `json:"status,omitempty"`
		Pods     []Pod           `json:"pods"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

func describeGet(t *testing.T, ts *httptest.Server, query string) (*http.Response, describeEnvelope) {
	t.Helper()

	resp, err := http.Get(ts.URL + "/describe?" + query)
	if err != nil {
		t.Fatalf("GET /describe: %v", err)
	}

	var env describeEnvelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()

	return resp, env
}

// TestDescribeRejectsNonGet locks in the method guard — describe is
// strictly read-only, so anything other than GET is 405 with an Allow
// header. Same shape as every other read-only endpoint.
func TestDescribeRejectsNonGet(t *testing.T) {
	api, _ := newTestAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/describe", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status=%d want 405", resp.StatusCode)
	}

	if got := resp.Header.Get("Allow"); got != "GET" {
		t.Errorf("Allow=%q want GET", got)
	}
}

// TestDescribeMissingParams: kind and name are both required. Without
// them describe has no resource to fetch — we surface 400 immediately
// rather than guess.
func TestDescribeMissingParams(t *testing.T) {
	api, _ := newTestAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	cases := []string{
		"",
		"kind=deployment",
		"name=api",
	}

	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			resp, env := describeGet(t, ts, q)

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status=%d want 400", resp.StatusCode)
			}

			if !strings.Contains(env.Error, "kind and name") {
				t.Errorf("error=%q expected mention of required fields", env.Error)
			}
		})
	}
}

// TestDescribeUnknownKind: ParseKind rejects "potato" and the handler
// returns 400 — we don't want a typo to become a silent 404 that
// suggests the resource just doesn't exist yet.
func TestDescribeUnknownKind(t *testing.T) {
	api, _ := newTestAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, _ := describeGet(t, ts, "kind=potato&name=x")

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
}

// TestDescribeMissingManifest: manifest absent in store → 404.
// resolveScope succeeded (or wasn't needed), but Get returned nil; the
// CLI relies on this distinction to print "not found" vs "decode
// error".
func TestDescribeMissingManifest(t *testing.T) {
	api, _ := newTestAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, _ := describeGet(t, ts, "kind=deployment&scope=test&name=ghost")

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d want 404", resp.StatusCode)
	}
}

// TestDescribeAmbiguousScope: a scoped kind without ?scope= triggers
// resolveScope. With matches in two scopes, we can't pick one safely —
// 400 with the candidates named so the operator knows what to retry.
func TestDescribeAmbiguousScope(t *testing.T) {
	api, store := newTestAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Scope: "alpha", Name: "web"})
	_, _ = store.Put(t.Context(), &Manifest{Kind: KindDeployment, Scope: "beta", Name: "web"})

	resp, env := describeGet(t, ts, "kind=deployment&name=web")

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}

	if !strings.Contains(env.Error, "alpha") || !strings.Contains(env.Error, "beta") {
		t.Errorf("error=%q expected both scopes named", env.Error)
	}
}

// TestDescribeAutoResolveScope: scoped kind + name unique across
// scopes → resolveScope picks the single match without forcing the
// caller to pass ?scope=. This is the convenience path that makes
// `voodu describe deployment web` ergonomic when there's only one.
func TestDescribeAutoResolveScope(t *testing.T) {
	api, store := newTestAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, _ = store.Put(t.Context(), &Manifest{
		Kind: KindDeployment, Scope: "only", Name: "web",
		Spec: json.RawMessage(`{"image":"x:1"}`),
	})

	resp, env := describeGet(t, ts, "kind=deployment&name=web")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	if env.Data.Manifest == nil {
		t.Fatal("manifest missing from response")
	}

	if env.Data.Manifest.Scope != "only" {
		t.Errorf("scope=%q want only", env.Data.Manifest.Scope)
	}
}

// TestDescribeReturnsManifestStatusAndPods is the happy-path
// integration: a deployment with status and matching pods produces a
// full describe envelope. We assert each branch (manifest fields,
// status decoded, pods filtered correctly).
func TestDescribeReturnsManifestStatusAndPods(t *testing.T) {
	api, store := newTestAPI(t)

	api.Pods = &fakePodsLister{
		pods: []Pod{
			{Name: "test-api.aaaa", Kind: "deployment", Scope: "test", ResourceName: "api", ReplicaID: "aaaa", Image: "img:1", Running: true},
			{Name: "test-api.bbbb", Kind: "deployment", Scope: "test", ResourceName: "api", ReplicaID: "bbbb", Image: "img:1", Running: true},
			// Different name — must NOT appear in the describe pods slice.
			{Name: "test-worker.cccc", Kind: "deployment", Scope: "test", ResourceName: "worker", ReplicaID: "cccc"},
			// Different scope — also filtered out.
			{Name: "other-api.dddd", Kind: "deployment", Scope: "other", ResourceName: "api", ReplicaID: "dddd"},
			// Different kind — filtered out.
			{Name: "test-api.eeee", Kind: "job", Scope: "test", ResourceName: "api", ReplicaID: "eeee"},
		},
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, _ = store.Put(t.Context(), &Manifest{
		Kind: KindDeployment, Scope: "test", Name: "api",
		Spec: json.RawMessage(`{"image":"img:1"}`),
	})

	statusBlob, _ := json.Marshal(DeploymentStatus{Image: "img:1", SpecHash: "abc"})
	_ = store.PutStatus(t.Context(), KindDeployment, "test-api", statusBlob)

	resp, env := describeGet(t, ts, "kind=deployment&scope=test&name=api")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%+v", resp.StatusCode, env)
	}

	if env.Status != "ok" {
		t.Errorf("envelope status=%q want ok", env.Status)
	}

	if env.Data.Manifest == nil || env.Data.Manifest.Name != "api" {
		t.Fatalf("manifest missing or wrong: %+v", env.Data.Manifest)
	}

	if len(env.Data.Status) == 0 {
		t.Error("status blob missing from response")
	} else {
		var st DeploymentStatus
		if err := json.Unmarshal(env.Data.Status, &st); err != nil {
			t.Fatalf("decode status: %v", err)
		}

		if st.Image != "img:1" {
			t.Errorf("status.image=%q want img:1", st.Image)
		}
	}

	if len(env.Data.Pods) != 2 {
		t.Fatalf("pods=%d want 2 (only matching kind+scope+name)", len(env.Data.Pods))
	}

	for _, p := range env.Data.Pods {
		if p.ResourceName != "api" || p.Scope != "test" || p.Kind != "deployment" {
			t.Errorf("unexpected pod in result: %+v", p)
		}
	}
}

// TestDescribePodListerErrorTolerated: matchingPods returns nil on
// lister failure. The describe response should still be 200 with an
// empty pods slice — the manifest+status are the heart of describe's
// value.
func TestDescribePodListerErrorTolerated(t *testing.T) {
	api, store := newTestAPI(t)

	api.Pods = &fakePodsLister{err: errFakePodsBroken}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, _ = store.Put(t.Context(), &Manifest{
		Kind: KindDeployment, Scope: "test", Name: "api",
		Spec: json.RawMessage(`{"image":"x:1"}`),
	})

	resp, env := describeGet(t, ts, "kind=deployment&scope=test&name=api")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200 even when pods lister fails", resp.StatusCode)
	}

	if env.Data.Manifest == nil {
		t.Error("manifest missing despite pods failure")
	}

	if len(env.Data.Pods) != 0 {
		t.Errorf("pods=%d want 0 when lister failed", len(env.Data.Pods))
	}
}

// TestDescribeCronJobStatusRoundTrip is the cronjob-specific shape
// check: the status blob includes history with run records. The
// envelope must preserve that nested structure intact for the CLI to
// render the history table.
func TestDescribeCronJobStatusRoundTrip(t *testing.T) {
	api, store := newTestAPI(t)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, _ = store.Put(t.Context(), &Manifest{
		Kind: KindCronJob, Scope: "ops", Name: "purge",
		Spec: json.RawMessage(`{"schedule":"*/5 * * * *","job":{"image":"img:1"}}`),
	})

	last := time.Date(2026, 4, 24, 9, 0, 0, 0, time.UTC)
	statusBlob, _ := json.Marshal(CronJobStatus{
		Schedule: "*/5 * * * *",
		Image:    "img:1",
		LastRun:  &last,
		History: []JobRun{
			{RunID: "r1", StartedAt: last, EndedAt: last.Add(2 * time.Second), ExitCode: 0, Status: "succeeded"},
		},
	})
	_ = store.PutStatus(t.Context(), KindCronJob, "ops-purge", statusBlob)

	resp, env := describeGet(t, ts, "kind=cronjob&scope=ops&name=purge")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	var st CronJobStatus
	if err := json.Unmarshal(env.Data.Status, &st); err != nil {
		t.Fatalf("decode cronjob status: %v", err)
	}

	if st.Schedule != "*/5 * * * *" {
		t.Errorf("schedule=%q", st.Schedule)
	}

	if len(st.History) != 1 || st.History[0].RunID != "r1" {
		t.Errorf("history not preserved: %+v", st.History)
	}
}

// errFakePodsBroken is a sentinel returned by fakePodsLister to drive
// the "lister error → empty pods" branch in TestDescribePodListerErrorTolerated.
var errFakePodsBroken = &fakePodsErr{msg: "fake pods lister failure"}

type fakePodsErr struct{ msg string }

func (e *fakePodsErr) Error() string { return e.msg }
