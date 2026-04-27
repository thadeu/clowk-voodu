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

// TestLogsScopeNameResolvesViaPodsList confirms the scope/name path:
// the CLI lists matching pods first via /pods?scope=&name=, then
// streams /pods/{name}/logs for each. Two replicas → two stream
// fetches.
func TestLogsScopeNameResolvesViaPodsList(t *testing.T) {
	var (
		mu          sync.Mutex
		listQuery   string
		streamPaths []string
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
						{Name: "clowk-lp-web.bbbb", Kind: "deployment", Scope: "clowk-lp", ResourceName: "web"},
					},
				},
			})

		case strings.HasSuffix(r.URL.Path, "/logs"):
			streamPaths = append(streamPaths, r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("line\n"))

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
		if !strings.Contains(listQuery, want) {
			t.Errorf("list query missing %q: %q", want, listQuery)
		}
	}

	wantPaths := map[string]bool{
		"/pods/clowk-lp-web.aaaa/logs": true,
		"/pods/clowk-lp-web.bbbb/logs": true,
	}

	if len(streamPaths) != 2 {
		t.Fatalf("expected 2 stream paths, got %d: %v", len(streamPaths), streamPaths)
	}

	for _, p := range streamPaths {
		if !wantPaths[p] {
			t.Errorf("unexpected stream path %q", p)
		}
	}
}

// TestLogsBareScopeListsAllPodsInScope mirrors the get pd bare scope
// behavior: no slash, no dot → treat the ref as a scope filter.
func TestLogsBareScopeListsAllPodsInScope(t *testing.T) {
	var (
		mu        sync.Mutex
		listQuery string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.URL.Path == "/pods" {
			listQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"data":   map[string]any{"pods": []controller.Pod{}},
			})

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

	// Empty match → error, but the listing must have happened with
	// scope=clowk-lp and NO name filter.
	_ = root.Execute()

	mu.Lock()
	defer mu.Unlock()

	if !strings.Contains(listQuery, "scope=clowk-lp") {
		t.Errorf("list query missing scope: %q", listQuery)
	}

	if strings.Contains(listQuery, "name=") {
		t.Errorf("bare scope must NOT carry a name filter: %q", listQuery)
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
