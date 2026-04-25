package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/controller"
)

// TestSplitJobRef covers the small parser the CLI uses to split the
// "scope/name" shorthand into separate query parameters. Tiny but
// hot-pathed (every `voodu run job` call hits it), so the unit test
// guards against silent semantics drift.
func TestSplitJobRef(t *testing.T) {
	cases := []struct {
		in        string
		wantScope string
		wantName  string
	}{
		{"api/migrate", "api", "migrate"},
		{"migrate", "", "migrate"},
		{"   api/migrate   ", "api", "migrate"},
		{"a/b/c", "a", "b/c"}, // first slash wins; names with slashes flow through
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

// TestRunJobRequestsControllerEndpoint validates the wire contract:
// `voodu run job api/migrate` POSTs to /jobs/run with the scope+name
// query split correctly, and renders the JobRun returned by the
// controller. Mock server stands in for the real /jobs/run handler so
// the test stays hermetic.
func TestRunJobRequestsControllerEndpoint(t *testing.T) {
	var (
		gotMethod   string
		gotPath     string
		gotRawQuery string
	)

	started := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ended := started.Add(2 * time.Second)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery

		resp := map[string]any{
			"status": "ok",
			"data": controller.JobRun{
				RunID:     "abcd",
				StartedAt: started,
				EndedAt:   ended,
				ExitCode:  0,
				Status:    controller.JobStatusSucceeded,
			},
		}

		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer

	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"run", "job", "api/migrate"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q, want POST", gotMethod)
	}

	if gotPath != "/jobs/run" {
		t.Errorf("path: got %q, want /jobs/run", gotPath)
	}

	if !strings.Contains(gotRawQuery, "name=migrate") || !strings.Contains(gotRawQuery, "scope=api") {
		t.Errorf("raw query missing scope/name: %q", gotRawQuery)
	}
}

// TestRunJobBareNameOmitsScope ensures `voodu run job migrate` (no
// slash) sends only ?name= and lets the server resolveScope. Without
// this, a stray empty `scope=` query param would break the
// disambiguator's "scope omitted" detection.
func TestRunJobBareNameOmitsScope(t *testing.T) {
	var gotRawQuery string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": controller.JobRun{
				RunID:  "abcd",
				Status: controller.JobStatusSucceeded,
			},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer

	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"run", "job", "migrate"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !strings.Contains(gotRawQuery, "name=migrate") {
		t.Errorf("query missing name=migrate: %q", gotRawQuery)
	}

	if strings.Contains(gotRawQuery, "scope=") {
		t.Errorf("bare-name run should omit scope query, got %q", gotRawQuery)
	}
}

// TestRunJobSurfacesServerError checks that a 500 response with a
// structured error in the envelope makes the CLI exit non-zero with
// the server's message. The data field still carries the JobRun so
// rendering shows exit code + duration alongside the failure.
func TestRunJobSurfacesServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "error",
			"error":  "job/api-migrate run abcd exited 17",
			"data": controller.JobRun{
				RunID:    "abcd",
				ExitCode: 17,
				Status:   controller.JobStatusFailed,
			},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer

	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"run", "job", "api/migrate"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}

	if !strings.Contains(err.Error(), "exited 17") {
		t.Errorf("error should surface server message verbatim, got %q", err.Error())
	}
}
