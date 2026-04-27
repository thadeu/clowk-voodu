package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/controller"
)

// describeMockServer stamps every incoming request and returns a
// fixed envelope so tests can assert on the wire shape without
// duplicating boilerplate. The captured query is the most important
// thing — that's where scope/name routing lives.
type describeMockState struct {
	method   string
	path     string
	rawQuery string
}

func newDescribeMockServer(t *testing.T, body any) (*httptest.Server, *describeMockState) {
	t.Helper()

	state := &describeMockState{}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.method = r.Method
		state.path = r.URL.Path
		state.rawQuery = r.URL.RawQuery

		_ = json.NewEncoder(w).Encode(body)
	}))

	return ts, state
}

// runDescribeCmd runs the CLI with describe args and captures stdout
// via the cobra writer hooks. Note the describe command itself writes
// directly to os.Stdout for the rendered output — this helper covers
// the cobra-side path (errors, json mode envelope) but not the text
// renderer; that's exercised separately through renderDescribe.
func runDescribeCmd(t *testing.T, ts *httptest.Server, args ...string) error {
	t.Helper()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"describe"}, args...))

	return root.Execute()
}

// TestDescribeWireContractScoped verifies that `voodu describe
// deployment scope/name` issues GET /describe with kind, scope, and
// name query params correctly split. This is the primary route every
// scoped describe takes.
func TestDescribeWireContractScoped(t *testing.T) {
	ts, state := newDescribeMockServer(t, map[string]any{
		"status": "ok",
		"data": map[string]any{
			"manifest": &controller.Manifest{
				Kind: controller.KindDeployment, Scope: "api", Name: "web",
				Spec: json.RawMessage(`{"image":"x:1"}`),
			},
		},
	})
	defer ts.Close()

	if err := runDescribeCmd(t, ts, "deployment", "api/web"); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if state.method != http.MethodGet {
		t.Errorf("method=%q want GET", state.method)
	}

	if state.path != "/describe" {
		t.Errorf("path=%q want /describe", state.path)
	}

	if !strings.Contains(state.rawQuery, "kind=deployment") {
		t.Errorf("query missing kind=deployment: %q", state.rawQuery)
	}

	if !strings.Contains(state.rawQuery, "scope=api") {
		t.Errorf("query missing scope=api: %q", state.rawQuery)
	}

	if !strings.Contains(state.rawQuery, "name=web") {
		t.Errorf("query missing name=web: %q", state.rawQuery)
	}
}

// TestDescribeWireContractBareName: bare name (no slash) must NOT
// emit ?scope= so the server's resolveScope kicks in. A stray empty
// scope= would defeat the auto-disambiguator on the controller side.
func TestDescribeWireContractBareName(t *testing.T) {
	ts, state := newDescribeMockServer(t, map[string]any{
		"status": "ok",
		"data": map[string]any{
			"manifest": &controller.Manifest{
				Kind: controller.KindDeployment, Scope: "only", Name: "web",
				Spec: json.RawMessage(`{}`),
			},
		},
	})
	defer ts.Close()

	if err := runDescribeCmd(t, ts, "deployment", "web"); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !strings.Contains(state.rawQuery, "name=web") {
		t.Errorf("query missing name=web: %q", state.rawQuery)
	}

	if strings.Contains(state.rawQuery, "scope=") {
		t.Errorf("bare-name describe must omit scope= param: %q", state.rawQuery)
	}
}

// TestDescribeUnscopedKind: database is unscoped — passing a scope
// must be rejected client-side BEFORE any HTTP call so a typo can't
// produce a confusing 404.
func TestDescribeRejectsScopeOnUnscopedKind(t *testing.T) {
	// Server should never be reached.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be reached; got %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
	}))
	defer ts.Close()

	err := runDescribeCmd(t, ts, "database", "scope/main")
	if err == nil {
		t.Fatal("expected error for scope-on-unscoped-kind")
	}

	if !strings.Contains(err.Error(), "unscoped") {
		t.Errorf("error=%q expected mention of unscoped", err.Error())
	}
}

// TestDescribeUnknownKindClientSide: the CLI parses kind eagerly so a
// typo errors before we waste a round trip.
func TestDescribeUnknownKindClientSide(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be reached for unknown kind")
	}))
	defer ts.Close()

	err := runDescribeCmd(t, ts, "potato", "x")
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

// TestDescribeSurfacesServerError: a 404 with envelope error must
// bubble up as a CLI error containing the server's message.
func TestDescribeSurfacesServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "error",
			"error":  "deployment/api/ghost not found",
		})
	}))
	defer ts.Close()

	err := runDescribeCmd(t, ts, "deployment", "api/ghost")
	if err == nil {
		t.Fatal("expected error from 404")
	}

	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error=%q expected to surface server message", err.Error())
	}
}

// --- Renderer unit tests --------------------------------------------
//
// renderDescribe takes io.Writer so tests don't need to wrestle with
// os.Stdout. Each per-kind branch gets a focused test that checks the
// summary lines + spec dump + (where applicable) history table.

func TestRenderDescribeDeployment(t *testing.T) {
	manifest := &controller.Manifest{
		Kind: controller.KindDeployment, Scope: "api", Name: "web",
		Spec: json.RawMessage(`{"image":"img:1"}`),
	}

	statusBlob, _ := json.Marshal(controller.DeploymentStatus{Image: "img:1", SpecHash: "abc123"})

	var buf bytes.Buffer
	if err := renderDescribe(&buf, manifest, statusBlob, nil, false); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, "deployment/api/web") {
		t.Errorf("missing header: %q", out)
	}

	if !strings.Contains(out, "image:     img:1") {
		t.Errorf("missing image line: %q", out)
	}

	if !strings.Contains(out, "spec_hash: abc123") {
		t.Errorf("missing spec_hash line: %q", out)
	}

	if strings.Contains(out, "spec:") {
		t.Errorf("default text view should NOT include spec dump: %q", out)
	}
}

// TestRenderDescribeDeploymentWithSpecDump locks in the `-o spec`
// behaviour: same view as text PLUS the raw manifest spec under a
// "spec:" heading.
func TestRenderDescribeDeploymentWithSpecDump(t *testing.T) {
	manifest := &controller.Manifest{
		Kind: controller.KindDeployment, Scope: "api", Name: "web",
		Spec: json.RawMessage(`{"image":"img:1"}`),
	}

	var buf bytes.Buffer
	_ = renderDescribe(&buf, manifest, nil, nil, true)

	out := buf.String()

	if !strings.Contains(out, "spec:") {
		t.Errorf("`-o spec` mode must include spec dump: %q", out)
	}

	if !strings.Contains(out, `"image": "img:1"`) {
		t.Errorf("spec dump missing manifest content: %q", out)
	}
}

func TestRenderDescribeDatabase(t *testing.T) {
	manifest := &controller.Manifest{
		Kind: controller.KindDatabase, Name: "main",
		Spec: json.RawMessage(`{"engine":"postgres","version":"16"}`),
	}

	statusBlob, _ := json.Marshal(controller.DatabaseStatus{
		Engine: "postgres", Version: "16",
		Params: map[string]string{"DATABASE_URL": "postgres://..."},
		Data:   map[string]any{"backup_count": 3},
	})

	var buf bytes.Buffer
	if err := renderDescribe(&buf, manifest, statusBlob, nil, false); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, "database/main") {
		t.Errorf("missing unscoped header: %q", out)
	}

	if strings.Contains(out, "database//main") {
		t.Errorf("unscoped header has double slash: %q", out)
	}

	if !strings.Contains(out, "engine:  postgres") {
		t.Errorf("missing engine line: %q", out)
	}

	if !strings.Contains(out, "DATABASE_URL") {
		t.Errorf("missing params: %q", out)
	}

	if !strings.Contains(out, "backup_count") {
		t.Errorf("missing data section: %q", out)
	}
}

func TestRenderDescribeJobWithHistory(t *testing.T) {
	manifest := &controller.Manifest{
		Kind: controller.KindJob, Scope: "api", Name: "migrate",
		Spec: json.RawMessage(`{"image":"img:1","command":["bun","/app/migrate.js"],"timeout":"5m","env":{"FOO":"bar"}}`),
	}

	last := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	statusBlob, _ := json.Marshal(controller.JobStatus{
		Image:   "img:1",
		LastRun: &last,
		History: []controller.JobRun{
			{RunID: "r1", StartedAt: last, EndedAt: last.Add(2 * time.Second), ExitCode: 0, Status: "succeeded"},
			{RunID: "r2", StartedAt: last.Add(-time.Hour), EndedAt: last.Add(-time.Hour + time.Second), ExitCode: 1, Status: "failed"},
		},
	})

	var buf bytes.Buffer
	if err := renderDescribe(&buf, manifest, statusBlob, nil, false); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, "job/api/migrate") {
		t.Errorf("missing header: %q", out)
	}

	if !strings.Contains(out, "command:  bun /app/migrate.js") {
		t.Errorf("missing command line: %q", out)
	}

	if !strings.Contains(out, "timeout:  5m") {
		t.Errorf("missing timeout line: %q", out)
	}

	if !strings.Contains(out, "env:      1 var(s)") {
		t.Errorf("missing env count line: %q", out)
	}

	if !strings.Contains(out, "history:  2 run(s)") {
		t.Errorf("missing history summary: %q", out)
	}

	if !strings.Contains(out, "history (2, newest first)") {
		t.Errorf("missing history table heading: %q", out)
	}

	if !strings.Contains(out, "RUN_ID") || !strings.Contains(out, "r1") || !strings.Contains(out, "r2") {
		t.Errorf("history table missing rows: %q", out)
	}

	// The default text view must NOT dump the spec — those fields just
	// surfaced in the summary already.
	if strings.Contains(out, "spec:") {
		t.Errorf("default text view should not include spec dump: %q", out)
	}
}

// TestRenderDescribeJobShowsImageDriftWhenStatusDiffers covers the
// "operator edited the manifest, reconciler hasn't run yet" diagnostic
// branch: status.Image != spec.Image surfaces an extra "image (last
// run)" line so the operator notices the pending drift.
func TestRenderDescribeJobShowsImageDriftWhenStatusDiffers(t *testing.T) {
	manifest := &controller.Manifest{
		Kind: controller.KindJob, Scope: "api", Name: "migrate",
		Spec: json.RawMessage(`{"image":"img:2"}`),
	}

	statusBlob, _ := json.Marshal(controller.JobStatus{Image: "img:1"})

	var buf bytes.Buffer
	_ = renderDescribe(&buf, manifest, statusBlob, nil, false)

	out := buf.String()

	if !strings.Contains(out, "image:    img:2") {
		t.Errorf("missing current image line: %q", out)
	}

	if !strings.Contains(out, "image (last run): img:1") {
		t.Errorf("expected image-drift line: %q", out)
	}
}

func TestRenderDescribeCronJobComputesNextRun(t *testing.T) {
	manifest := &controller.Manifest{
		Kind: controller.KindCronJob, Scope: "ops", Name: "purge",
		Spec: json.RawMessage(`{"schedule":"*/5 * * * *","timezone":"UTC","concurrency_policy":"Forbid","successful_history_limit":5,"failed_history_limit":5,"job":{"image":"img:1","command":["bun","/app/cron.js"],"timeout":"5m"}}`),
	}

	var buf bytes.Buffer
	if err := renderDescribe(&buf, manifest, nil, nil, false); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, "cronjob/ops/purge") {
		t.Errorf("missing header: %q", out)
	}

	if !strings.Contains(out, "schedule:    */5 * * * *") {
		t.Errorf("missing schedule line: %q", out)
	}

	if !strings.Contains(out, "timezone:    UTC") {
		t.Errorf("missing timezone line: %q", out)
	}

	if !strings.Contains(out, "suspended:   false") {
		t.Errorf("missing suspended line: %q", out)
	}

	if !strings.Contains(out, "concurrency: Forbid") {
		t.Errorf("missing concurrency line: %q", out)
	}

	if !strings.Contains(out, "next_run:    ") {
		t.Errorf("next_run computed line missing: %q", out)
	}

	if !strings.Contains(out, "image:       img:1") {
		t.Errorf("missing image line from spec.job: %q", out)
	}

	if !strings.Contains(out, "command:     bun /app/cron.js") {
		t.Errorf("missing command line from spec.job: %q", out)
	}

	if !strings.Contains(out, "timeout:     5m") {
		t.Errorf("missing timeout line from spec.job: %q", out)
	}

	if !strings.Contains(out, "history limits: success=5, failed=5") {
		t.Errorf("missing history limits line: %q", out)
	}

	if !strings.Contains(out, "(no status recorded yet)") {
		t.Errorf("expected 'no status' note when blob empty: %q", out)
	}

	if strings.Contains(out, "spec:") {
		t.Errorf("default text view should not include spec dump: %q", out)
	}
}

func TestRenderDescribeCronJobSuspendedHasNoNextRun(t *testing.T) {
	manifest := &controller.Manifest{
		Kind: controller.KindCronJob, Scope: "ops", Name: "purge",
		Spec: json.RawMessage(`{"schedule":"*/5 * * * *","suspend":true,"job":{"image":"img:1"}}`),
	}

	var buf bytes.Buffer
	_ = renderDescribe(&buf, manifest, nil, nil, false)

	out := buf.String()

	if !strings.Contains(out, "suspended:   true") {
		t.Errorf("missing suspended line: %q", out)
	}

	if !strings.Contains(out, "(suspended)") {
		t.Errorf("expected (suspended) note in next_run line: %q", out)
	}
}

func TestRenderDescribePodsTable(t *testing.T) {
	manifest := &controller.Manifest{
		Kind: controller.KindDeployment, Scope: "api", Name: "web",
		Spec: json.RawMessage(`{}`),
	}

	pods := []controller.Pod{
		{Name: "api-web.aaaa", ReplicaID: "aaaa", Image: "img:1", Running: true, CreatedAt: "2026-04-24T12:00:00Z"},
		{Name: "api-web.bbbb", ReplicaID: "bbbb", Image: "img:1", Running: false, CreatedAt: "2026-04-24T11:00:00Z"},
	}

	var buf bytes.Buffer
	if err := renderDescribe(&buf, manifest, nil, pods, false); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, "pods (2):") {
		t.Errorf("missing pods header: %q", out)
	}

	if !strings.Contains(out, "NAME") || !strings.Contains(out, "REPLICA") {
		t.Errorf("missing pods table columns: %q", out)
	}

	if !strings.Contains(out, "api-web.aaaa") {
		t.Errorf("missing first pod row: %q", out)
	}

	if !strings.Contains(out, "running") {
		t.Errorf("running fallback status missing: %q", out)
	}

	if !strings.Contains(out, "stopped") {
		t.Errorf("stopped fallback status missing: %q", out)
	}
}

// TestRenderDescribeOmitsPodsSectionWhenEmpty: ingress / database often
// have zero matching pods (caddy on host, plugin-managed db). The
// renderer must skip the heading entirely so the output stays clean.
func TestRenderDescribeOmitsPodsSectionWhenEmpty(t *testing.T) {
	manifest := &controller.Manifest{
		Kind: controller.KindIngress, Scope: "test", Name: "public",
		Spec: json.RawMessage(`{"plugin":"caddy"}`),
	}

	var buf bytes.Buffer
	_ = renderDescribe(&buf, manifest, nil, nil, false)

	if strings.Contains(buf.String(), "pods (") {
		t.Errorf("empty pods slice should omit section: %q", buf.String())
	}
}

func TestRenderDescribeEmptyManifestErrors(t *testing.T) {
	var buf bytes.Buffer
	err := renderDescribe(&buf, nil, nil, nil, false)

	if err == nil {
		t.Fatal("expected error when manifest is nil")
	}
}

// TestDescribeOutputModeSpec verifies the new `-o spec` flag flips on
// the raw spec dump in the rendered output. Goes through the full
// cobra path (mock server + root.Execute) so flag parsing and the
// describeOutputMode helper are both covered.
func TestDescribeOutputModeSpec(t *testing.T) {
	ts, _ := newDescribeMockServer(t, map[string]any{
		"status": "ok",
		"data": map[string]any{
			"manifest": &controller.Manifest{
				Kind: controller.KindDeployment, Scope: "api", Name: "web",
				Spec: json.RawMessage(`{"image":"img:1"}`),
			},
		},
	})
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)
	_ = root.PersistentFlags().Set("output", "spec")

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"describe", "deployment", "api/web"})

	// Output goes to os.Stdout for text/spec modes — we can only assert
	// on the no-error path here. The renderer-level tests above lock in
	// the actual content of `-o spec`.
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
}

// TestDescribeDescAliasRoutesToDescribe makes sure `voodu desc` is a
// drop-in for `voodu describe` — same wire shape, same query params.
// Without this the alias could silently route somewhere else and we'd
// only learn during an operator demo.
func TestDescribeDescAliasRoutesToDescribe(t *testing.T) {
	ts, state := newDescribeMockServer(t, map[string]any{
		"status": "ok",
		"data": map[string]any{
			"manifest": &controller.Manifest{
				Kind: controller.KindDeployment, Scope: "api", Name: "web",
				Spec: json.RawMessage(`{"image":"x:1"}`),
			},
		},
	})
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"desc", "deployment", "api/web"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute via desc alias: %v", err)
	}

	if state.path != "/describe" {
		t.Errorf("desc should route to /describe, got %q", state.path)
	}

	if !strings.Contains(state.rawQuery, "kind=deployment") || !strings.Contains(state.rawQuery, "name=web") {
		t.Errorf("desc query missing fields: %q", state.rawQuery)
	}
}

// TestDescribePodRoutesToPodsName proves `voodu describe pod <name>`
// (and the `pd` short form) hit GET /pods/{name} rather than
// /describe — pods don't fit the kind/scope/name shape and have their
// own endpoint.
func TestDescribePodRoutesToPodsName(t *testing.T) {
	cases := []struct {
		name      string
		kindToken string
	}{
		{"long form", "pod"},
		{"short form", "pd"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, state := newDescribeMockServer(t, map[string]any{
				"status": "ok",
				"data": map[string]any{
					"pod": &controller.PodDetail{
						Pod: controller.Pod{
							Name: "test-web.a3f9", Kind: "deployment",
							Scope: "test", ResourceName: "web",
							ReplicaID: "a3f9", Image: "vd-web:latest",
						},
					},
				},
			})
			defer ts.Close()

			if err := runDescribeCmd(t, ts, tc.kindToken, "test-web.a3f9"); err != nil {
				t.Fatalf("execute: %v", err)
			}

			if state.method != http.MethodGet {
				t.Errorf("method=%q want GET", state.method)
			}

			if state.path != "/pods/test-web.a3f9" {
				t.Errorf("path=%q want /pods/test-web.a3f9", state.path)
			}

			if state.rawQuery != "" {
				t.Errorf("query should be empty for /pods/{name}, got %q", state.rawQuery)
			}
		})
	}
}

// TestDescribePodAcceptsScopeNameRef locks in the "all replicas of
// an app" shape: `vd describe pod clowk-lp/web` lists matching pods
// via /pods?scope=&name=, then fetches /pods/{name} for each. The
// operator types the scope/name they already know — no need to
// 'voodu get pods | grep' first to copy a replica id.
func TestDescribePodAcceptsScopeNameRef(t *testing.T) {
	var (
		mu             sync.Mutex
		listQuery      string
		fetchedDetails []string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.URL.Path == "/pods":
			listQuery = r.URL.RawQuery

			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"data": map[string]any{
					"pods": []controller.Pod{
						{Name: "clowk-lp-web.aaaa", Kind: "deployment", Scope: "clowk-lp", ResourceName: "web", ReplicaID: "aaaa"},
						{Name: "clowk-lp-web.bbbb", Kind: "deployment", Scope: "clowk-lp", ResourceName: "web", ReplicaID: "bbbb"},
					},
				},
			})

		case strings.HasPrefix(r.URL.Path, "/pods/"):
			name := strings.TrimPrefix(r.URL.Path, "/pods/")
			fetchedDetails = append(fetchedDetails, name)

			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"data": map[string]any{
					"pod": &controller.PodDetail{
						Pod: controller.Pod{
							Name: name, Kind: "deployment", Scope: "clowk-lp",
							ResourceName: "web",
						},
					},
				},
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	if err := runDescribeCmd(t, ts, "pod", "clowk-lp/web"); err != nil {
		t.Fatalf("execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	for _, want := range []string{"scope=clowk-lp", "name=web"} {
		if !strings.Contains(listQuery, want) {
			t.Errorf("list query missing %q: %q", want, listQuery)
		}
	}

	wantDetails := []string{"clowk-lp-web.aaaa", "clowk-lp-web.bbbb"}
	if strings.Join(fetchedDetails, ",") != strings.Join(wantDetails, ",") {
		t.Errorf("fetched details: got %v, want %v", fetchedDetails, wantDetails)
	}
}

// TestDescribePodAcceptsBareScopeRef locks in the third ref shape:
// `vd describe pod clowk-lp` (no slash, no dot) lists every pod in
// the scope across all kinds, then renders the detail for each one.
// The discriminator hinges on the dot-vs-no-dot heuristic — if a
// future refactor accidentally treats bare refs as container names
// again, this test catches it before the operator does.
func TestDescribePodAcceptsBareScopeRef(t *testing.T) {
	var (
		mu             sync.Mutex
		listQuery      string
		fetchedDetails []string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.URL.Path == "/pods":
			listQuery = r.URL.RawQuery

			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"data": map[string]any{
					"pods": []controller.Pod{
						{Name: "clowk-lp-web.aaaa", Kind: "deployment", Scope: "clowk-lp", ResourceName: "web"},
						{Name: "clowk-lp-crawler1.bbbb", Kind: "cronjob", Scope: "clowk-lp", ResourceName: "crawler1"},
						{Name: "clowk-lp-migrate.cccc", Kind: "job", Scope: "clowk-lp", ResourceName: "migrate"},
					},
				},
			})

		case strings.HasPrefix(r.URL.Path, "/pods/"):
			name := strings.TrimPrefix(r.URL.Path, "/pods/")
			fetchedDetails = append(fetchedDetails, name)

			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"data": map[string]any{
					"pod": &controller.PodDetail{
						Pod: controller.Pod{Name: name, Scope: "clowk-lp"},
					},
				},
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	if err := runDescribeCmd(t, ts, "pod", "clowk-lp"); err != nil {
		t.Fatalf("execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// The list query should carry the scope but no name — bare ref
	// means "every container in this scope regardless of resource".
	if !strings.Contains(listQuery, "scope=clowk-lp") {
		t.Errorf("list query missing scope filter: %q", listQuery)
	}

	if strings.Contains(listQuery, "name=") {
		t.Errorf("bare scope ref must NOT carry a name filter, got %q", listQuery)
	}

	wantDetails := []string{"clowk-lp-web.aaaa", "clowk-lp-crawler1.bbbb", "clowk-lp-migrate.cccc"}
	if strings.Join(fetchedDetails, ",") != strings.Join(wantDetails, ",") {
		t.Errorf("fetched details: got %v, want %v", fetchedDetails, wantDetails)
	}
}

// TestDescribePodScopeNameNoMatchErrors is the friendly-error
// counterpart: zero matching containers produces a clear "no pods
// match" message instead of a successful empty render.
func TestDescribePodScopeNameNoMatchErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]any{"pods": []controller.Pod{}},
		})
	}))
	defer ts.Close()

	err := runDescribeCmd(t, ts, "pod", "missing/app")
	if err == nil {
		t.Fatal("expected error for unmatched scope/name")
	}

	if !strings.Contains(err.Error(), "no pods match") {
		t.Errorf("error should mention no match, got %q", err.Error())
	}
}

// TestDescribePodSurfacesEnvelopeError mirrors the logs test: a 404
// envelope from the controller becomes the CLI's error verbatim, not
// an opaque "controller returned 404".
func TestDescribePodSurfacesEnvelopeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "error",
			"error":  "pod \"missing.0000\" not found",
		})
	}))
	defer ts.Close()

	err := runDescribeCmd(t, ts, "pod", "missing.0000")
	if err == nil {
		t.Fatal("expected error for 404")
	}

	if !strings.Contains(err.Error(), "missing.0000") {
		t.Errorf("error should surface server message, got %q", err.Error())
	}
}

// TestRenderPodDetailHidesEnvByDefault is the security regression
// guard: NOTHING about env appears in default text output. Not
// values, not names, not a count, not a hint about --show-env.
// Names alone leak intent — STRIPE_SECRET_KEY, AWS_ACCESS_KEY_ID,
// SENTRY_DSN tell an attacker watching a screen-share what to look
// for. The presence-or-absence of an "env" section is itself signal,
// so we elide the whole block; --show-env is the only way to see
// any env-related content.
func TestRenderPodDetailHidesEnvByDefault(t *testing.T) {
	pod := &controller.PodDetail{
		Pod: controller.Pod{
			Name: "test-web.a3f9", Kind: "deployment", Scope: "test",
			ResourceName: "web", ReplicaID: "a3f9", Image: "vd-web:latest",
		},
		Env: map[string]string{
			"NODE_ENV":          "production",
			"DATABASE_URL":      "postgres://app:s3cret@db/app",
			"STRIPE_SECRET_KEY": "sk-live-AAAA-BBBB-CCCC",
		},
	}

	var buf bytes.Buffer
	if err := renderPodDetail(&buf, pod, false); err != nil {
		t.Fatal(err)
	}

	out := buf.String()

	// Nothing env-related may appear in the default output.
	for _, leak := range []string{
		// names
		"NODE_ENV", "DATABASE_URL", "STRIPE_SECRET_KEY",
		// values
		"production", "s3cret", "sk-live-AAAA",
		// banner / count / hint — all elided
		"env", "var(s)", "--show-env",
	} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked in default output:\n%s", leak, out)
		}
	}
}

// TestRenderPodDetailShowsEnvWhenFlagged confirms --show-env actually
// reveals values — without this the flag could silently no-op.
func TestRenderPodDetailShowsEnvWhenFlagged(t *testing.T) {
	pod := &controller.PodDetail{
		Pod: controller.Pod{Name: "test-web.a3f9"},
		Env: map[string]string{
			"NODE_ENV": "production",
			"PORT":     "3000",
		},
	}

	var buf bytes.Buffer
	if err := renderPodDetail(&buf, pod, true); err != nil {
		t.Fatal(err)
	}

	out := buf.String()

	for _, want := range []string{"NODE_ENV=production", "PORT=3000"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in --show-env output:\n%s", want, out)
		}
	}

	// The hint disappears once values are already shown — would just
	// be noise.
	if strings.Contains(out, "--show-env") {
		t.Errorf("--show-env hint should not appear when already revealed:\n%s", out)
	}
}
