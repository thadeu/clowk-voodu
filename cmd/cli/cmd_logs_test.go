package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

// TestLogsContainerNameRoutesToPodLogs locks in the single-container
// path: a ref with a dot is treated as a docker container name and
// hits /pods/{name}/logs directly, no /pods?... lookup needed.
func TestLogsContainerNameRoutesToPodLogs(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path

		w.Header().Set("X-Voodu-Container", "test-migrate.abcd")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello\n"))
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"logs", "test-migrate.abcd"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method=%q want GET", gotMethod)
	}

	if gotPath != "/pods/test-migrate.abcd/logs" {
		t.Errorf("path=%q want /pods/test-migrate.abcd/logs", gotPath)
	}
}

// TestLogsReplicaRefRoutesDirectly pins the per-replica shape:
// `vd logs clowk-lp/redis.0` translates to the deterministic
// container name `clowk-lp-redis.0` and hits /pods/{name}/logs
// in a SINGLE round-trip — no /pods listing query, no risk of
// "no containers match" on a typo'd ordinal beating the actual
// stream fetch's "container not found" error.
//
// Without this test, a regression in splitReplicaRef would silently
// fall through to the listing path and re-introduce the user-
// reported failure mode.
func TestLogsReplicaRefRoutesDirectly(t *testing.T) {
	var (
		mu       sync.Mutex
		hits     []string
		listSeen bool
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		hits = append(hits, r.URL.Path)

		if r.URL.Path == "/pods" {
			listSeen = true
		}

		w.Header().Set("X-Voodu-Container", "clowk-lp-redis.0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from redis-0\n"))
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"logs", "clowk-lp/redis.0"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if listSeen {
		t.Errorf("per-replica ref should NOT consult /pods listing; hits=%v", hits)
	}

	if len(hits) != 1 || hits[0] != "/pods/clowk-lp-redis.0/logs" {
		t.Errorf("expected single hit on /pods/clowk-lp-redis.0/logs, got %v", hits)
	}
}

// TestLogsScopeNameUsesMultiplexedEndpoint pins the scope/name path:
// the CLI issues ONE request to /logs?scope=&name= and the server
// returns the multiplexed stream (each line prefixed with
// `[pod-name] `). The N+1 client-side fan-out is gone; this test
// catches a regression where someone reintroduces a per-pod loop.
func TestLogsScopeNameUsesMultiplexedEndpoint(t *testing.T) {
	var (
		mu             sync.Mutex
		logsQuery      string
		perPodFetchHit []string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.URL.Path == "/logs":
			logsQuery = r.URL.RawQuery
			w.Header().Set("X-Voodu-Containers", "clowk-lp-web.aaaa,clowk-lp-web.bbbb")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("[clowk-lp-web.aaaa] alpha\n[clowk-lp-web.bbbb] beta\n"))

		case strings.HasPrefix(r.URL.Path, "/pods/") && strings.HasSuffix(r.URL.Path, "/logs"):
			perPodFetchHit = append(perPodFetchHit, r.URL.Path)

		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"logs", "clowk-lp/web"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	for _, want := range []string{"scope=clowk-lp", "name=web"} {
		if !strings.Contains(logsQuery, want) {
			t.Errorf("logs query missing %q: %q", want, logsQuery)
		}
	}

	if len(perPodFetchHit) > 0 {
		t.Errorf("CLI must not fan-out per pod; saw %v", perPodFetchHit)
	}
}

// TestLogsBareScopeUsesMultiplexedEndpoint mirrors the bare-scope
// `vd logs <scope>` shape. Single request, no name filter.
func TestLogsBareScopeUsesMultiplexedEndpoint(t *testing.T) {
	var (
		mu        sync.Mutex
		logsQuery string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.URL.Path == "/logs" {
			logsQuery = r.URL.RawQuery
			w.Header().Set("X-Voodu-Containers", "clowk-lp-web.aaaa")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("[clowk-lp-web.aaaa] line\n"))

			return
		}

		t.Errorf("unexpected: %s", r.URL.Path)
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"logs", "clowk-lp"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if !strings.Contains(logsQuery, "scope=clowk-lp") {
		t.Errorf("logs query missing scope: %q", logsQuery)
	}

	if strings.Contains(logsQuery, "name=") {
		t.Errorf("bare scope must NOT carry a name filter: %q", logsQuery)
	}
}

// TestLogsZeroMatchSurfacesFriendlyError — the server signals
// "no matches" via an empty X-Voodu-Containers header. The CLI must
// turn that into the same "no containers match" error the operator
// used to see from the client-side fan-out path.
func TestLogsZeroMatchSurfacesFriendlyError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/logs" {
			w.Header().Set("X-Voodu-Containers", "")
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected: %s", r.URL.Path)
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"logs", "nope"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("expected error on zero match")
	}

	if !strings.Contains(err.Error(), "no containers match") {
		t.Errorf("error message: got %q, want substring 'no containers match'", err.Error())
	}
}

// TestLogsForwardsFollowAndTail confirms the stream-side flags
// translate to query params on the per-container endpoint. Without
// this the CLI's --follow / --tail could silently no-op.
//
// Both the long form (--tail 100) and the short form (-t 100) are
// exercised so a regression in either alias surfaces immediately.
func TestLogsForwardsFollowAndTail(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"long form", []string{"logs", "test-web.aaaa", "--follow", "--tail", "100"}},
		{"short form", []string{"logs", "test-web.aaaa", "-f", "-t", "100"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotQuery string

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/logs") {
					gotQuery = r.URL.RawQuery
				}

				w.WriteHeader(http.StatusOK)
			}))
			defer ts.Close()

			root := newRootCmd()
			_ = root.PersistentFlags().Set("controller-url", ts.URL)

			var buf bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&buf)
			root.SetArgs(c.args)

			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}

			for _, want := range []string{"follow=true", "tail=100"} {
				if !strings.Contains(gotQuery, want) {
					t.Errorf("query missing %q: %q", want, gotQuery)
				}
			}
		})
	}
}

// TestLogsSurfacesEnvelopeError confirms the JSON-envelope error
// format from /pods/{name}/logs reaches the operator verbatim, not
// wrapped behind a generic 4xx message.
func TestLogsSurfacesEnvelopeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "error",
			"error":  "pod \"missing.0000\" not found",
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"logs", "missing.0000"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for 404")
	}

	if !strings.Contains(err.Error(), "missing.0000") {
		t.Errorf("error should surface server message, got %q", err.Error())
	}
}

// TestLogsNoMatchErrors locks in the friendly empty-result behavior:
// scope/name resolves to zero containers → clear "no containers
// match" error, not a silent no-op.
func TestLogsNoMatchErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]any{"pods": []controller.Pod{}},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"logs", "missing/app"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for empty match")
	}

	if !strings.Contains(err.Error(), "no containers match") {
		t.Errorf("error should mention no match: %q", err.Error())
	}
}
