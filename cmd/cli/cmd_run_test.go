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

// TestSplitJobRef covers the small parser the CLI uses to split the
// "scope/name" shorthand into separate query parameters. Tiny but
// hot-pathed (every `vd run / vd exec / vd logs` ref hits it), so
// the unit test guards against silent semantics drift.
func TestSplitJobRef(t *testing.T) {
	cases := []struct {
		in        string
		wantScope string
		wantName  string
	}{
		{"api/migrate", "api", "migrate"},
		{"migrate", "", "migrate"},
		{"   api/migrate   ", "api", "migrate"},
		{"a/b/c", "a", "b/c"}, // first slash wins
		{"", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			scope, name := splitJobRef(tc.in)

			if scope != tc.wantScope || name != tc.wantName {
				t.Errorf("splitJobRef(%q) = (%q, %q), want (%q, %q)",
					tc.in, scope, name, tc.wantScope, tc.wantName)
			}
		})
	}
}

// TestSplitReplicaRef covers the per-replica parser used by
// logs / exec / describe pod to recognise `<scope>/<name>.<replica>`
// shape. Hot path — every CLI invocation that takes a pod ref
// runs this. The cases below pin the boundary conditions
// explicitly so a future edit can't accidentally widen ok=true
// to non-replica refs (which would silently bypass the
// /pods listing path).
func TestSplitReplicaRef(t *testing.T) {
	cases := []struct {
		in    string
		scope string
		name  string
		repl  string
		ok    bool
	}{
		// Statefulset ordinal — the user's reported shape.
		{"clowk-lp/redis.0", "clowk-lp", "redis", "0", true},
		{"clowk-lp/redis.7", "clowk-lp", "redis", "7", true},

		// Deployment hex replica id — same shape works.
		{"clowk-lp/web.a3f9", "clowk-lp", "web", "a3f9", true},

		// Whitespace tolerated.
		{"  clowk-lp/redis.0  ", "clowk-lp", "redis", "0", true},

		// scope/name (no dot) → not a replica ref, fall through.
		{"clowk-lp/redis", "", "", "", false},

		// Bare resource name — no slash, no replica.
		{"redis", "", "", "", false},

		// Bare full container name (clowk-lp-redis.0) — no slash,
		// not the per-replica shape. The caller's "has '.' but no '/'"
		// branch handles this verbatim.
		{"clowk-lp-redis.0", "", "", "", false},

		// Pathological: empty replica part (`scope/name.`).
		{"clowk-lp/redis.", "", "", "", false},

		// Pathological: empty basename (`scope/.0`) — the dot is at
		// position 0 of the name part, no resource name to address.
		{"clowk-lp/.0", "", "", "", false},

		// Empty.
		{"", "", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			scope, name, repl, ok := splitReplicaRef(tc.in)

			if ok != tc.ok {
				t.Fatalf("splitReplicaRef(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}

			if !ok {
				return
			}

			if scope != tc.scope || name != tc.name || repl != tc.repl {
				t.Errorf("splitReplicaRef(%q) = (%q, %q, %q), want (%q, %q, %q)",
					tc.in, scope, name, repl, tc.scope, tc.name, tc.repl)
			}
		})
	}
}

// runRouter is a stub controller for the unified run tests. Each
// test wires a different combination of:
//
//   - what the manifest probe returns for each kind (job / cronjob)
//   - what /jobs/run, /cronjobs/run, /pods*, /pods/{name}/exec do
//
// Helpers below let the test build the right server in 4 lines.
type runRouter struct {
	jobManifest     *controller.Manifest // returned for GET /apply?kind=job
	cronjobManifest *controller.Manifest // returned for GET /apply?kind=cronjob

	// Records the (path, method) for the post-resolution call so
	// tests can assert "did we hit /jobs/run vs /cronjobs/run vs
	// /pods/{name}/exec".
	mu       sync.Mutex
	hits     []string
	jobsBody []byte
}

func (r *runRouter) handler(t *testing.T) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits = append(r.hits, req.Method+" "+req.URL.Path)
		r.mu.Unlock()

		switch {
		// Manifest probe — runUnified calls fetchRemote(kind),
		// which the server answers with ALL manifests of that kind
		// as an array. fetchRemote then filters client-side by
		// scope+name.
		case req.Method == http.MethodGet && req.URL.Path == "/apply":
			kind := req.URL.Query().Get("kind")

			var data []controller.Manifest

			if kind == "job" && r.jobManifest != nil {
				data = []controller.Manifest{*r.jobManifest}
			}

			if kind == "cronjob" && r.cronjobManifest != nil {
				data = []controller.Manifest{*r.cronjobManifest}
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"data":   data,
			})

		// Trigger paths.
		case req.Method == http.MethodPost && req.URL.Path == "/jobs/run":
			r.jobsBody = nil

			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"data": controller.JobRun{
					RunID:     "abcd",
					Status:    controller.JobStatusSucceeded,
					StartedAt: time.Now(),
					EndedAt:   time.Now().Add(time.Second),
				},
			})

		case req.Method == http.MethodPost && req.URL.Path == "/cronjobs/run":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"data": controller.JobRun{
					RunID:  "abcd",
					Status: controller.JobStatusSucceeded,
				},
			})

		// Pod listing for the exec path.
		case req.Method == http.MethodGet && req.URL.Path == "/pods":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"data": map[string]any{
					"pods": []controller.Pod{
						{Name: "test-web.aaaa", Running: true, CreatedAt: "2026-04-25T00:00:00Z"},
					},
				},
			})

		default:
			// Exec path: /pods/{name}/exec hits — we can't easily
			// hijack in a test server, so just claim 200 with a
			// minimal body. cmd_exec's own tests cover the hijack
			// shape; here we only need to confirm the route was
			// reached.
			if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/exec") {
				w.WriteHeader(http.StatusOK)
				return
			}

			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	})
}

func newRunRouter() *runRouter { return &runRouter{} }

// TestRunDispatchesJobWhenRefIsDeclaredJob covers the no-cmd path
// for jobs: ref resolves to a declared job, so vd run probes /apply
// then triggers /jobs/run. Operator types just `vd run scope/name`.
func TestRunDispatchesJobWhenRefIsDeclaredJob(t *testing.T) {
	r := newRunRouter()
	r.jobManifest = &controller.Manifest{
		Kind: controller.KindJob, Scope: "api", Name: "migrate",
	}

	ts := httptest.NewServer(r.handler(t))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"run", "api/migrate"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !contains(r.hits, "POST /jobs/run") {
		t.Errorf("expected POST /jobs/run in hits, got %v", r.hits)
	}

	// Must NOT have hit /cronjobs/run — once job manifest is found,
	// cronjob probe is short-circuited.
	if contains(r.hits, "POST /cronjobs/run") {
		t.Errorf("should not hit /cronjobs/run when ref is a job, got %v", r.hits)
	}
}

// TestRunDispatchesCronJobWhenRefIsDeclaredCronJob mirrors the
// previous test for cronjobs. The job probe returns 404 first;
// cronjob probe finds the manifest; /cronjobs/run fires.
func TestRunDispatchesCronJobWhenRefIsDeclaredCronJob(t *testing.T) {
	r := newRunRouter()
	r.cronjobManifest = &controller.Manifest{
		Kind: controller.KindCronJob, Scope: "ops", Name: "purge",
	}

	ts := httptest.NewServer(r.handler(t))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"run", "ops/purge"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !contains(r.hits, "POST /cronjobs/run") {
		t.Errorf("expected POST /cronjobs/run in hits, got %v", r.hits)
	}
}

// TestRunWithCommandExecsIntoContainer is the exec-path canary: a
// ref + extra args means "run cmd inside a container, regardless of
// kind". The manifest probe is skipped entirely; runExec resolves
// the ref via /pods and POSTs to /pods/{name}/exec.
func TestRunWithCommandExecsIntoContainer(t *testing.T) {
	r := newRunRouter()

	ts := httptest.NewServer(r.handler(t))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"run", "test/web", "ls", "/app"})

	// Don't care about the result; the hijack-in-test-server is
	// finicky. We just want to confirm it took the exec route.
	_ = root.Execute()

	for _, want := range []string{"GET /pods", "POST /pods/test-web.aaaa/exec"} {
		if !contains(r.hits, want) {
			t.Errorf("expected hit %q, got %v", want, r.hits)
		}
	}

	// Manifest probe should NOT have happened — the cmd presence
	// short-circuits to the exec path.
	for _, unwanted := range []string{"POST /jobs/run", "POST /cronjobs/run", "GET /apply"} {
		if contains(r.hits, unwanted) {
			t.Errorf("exec path should skip %q, got hits %v", unwanted, r.hits)
		}
	}
}

// TestRunNoCommandUnknownRefErrors locks in the "no obvious
// one-shot for this ref" path: when the ref isn't a declared job
// or cronjob and no command was given, vd run errors with a hint
// pointing at the exec form. Without this, the operator would see
// nothing happen and have to guess.
func TestRunNoCommandUnknownRefErrors(t *testing.T) {
	r := newRunRouter()
	// Nothing seeded → both probes return 404.

	ts := httptest.NewServer(r.handler(t))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"run", "clowk-lp/web"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for ref that's neither job nor cronjob")
	}

	for _, want := range []string{"clowk-lp/web", "vd run clowk-lp/web", "CMD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should hint at run-with-cmd form, missing %q: %q", want, err.Error())
		}
	}
}
