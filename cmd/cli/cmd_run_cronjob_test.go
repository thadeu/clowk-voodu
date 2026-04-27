package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

// TestRunCronJobRoutesToEndpoint locks the wire shape: `vd run
// cronjob api/migrate` POSTs to /cronjobs/run with scope+name
// query params, and the JobRun envelope from the controller drives
// the rendered output. Without this the CLI could silently land on
// the wrong endpoint (e.g. /jobs/run) and an operator would only
// notice when the cronjob runs as a regular job and skips the
// CronJobHandler-only side effects.
func TestRunCronJobRoutesToEndpoint(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotQuery  string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": controller.JobRun{
				RunID:    "abcd",
				Status:   controller.JobStatusSucceeded,
				ExitCode: 0,
			},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"run", "cronjob", "ops/purge"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method=%q want POST", gotMethod)
	}

	if gotPath != "/cronjobs/run" {
		t.Errorf("path=%q want /cronjobs/run", gotPath)
	}

	for _, want := range []string{"scope=ops", "name=purge"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query missing %q: %q", want, gotQuery)
		}
	}
}

// TestRunCronJobBareNameOmitsScope mirrors run job: a bare ref must
// NOT carry an empty scope query param, otherwise the controller's
// scope-omitted resolver path never fires.
func TestRunCronJobBareNameOmitsScope(t *testing.T) {
	var gotQuery string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   controller.JobRun{RunID: "x", Status: controller.JobStatusSucceeded},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"run", "cronjob", "purge"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if strings.Contains(gotQuery, "scope=") {
		t.Errorf("bare name must omit scope query, got %q", gotQuery)
	}
}

// TestRunCronJobSurfacesEnvelopeError confirms a JSON-envelope error
// (cronjob not found, runner not configured) becomes the CLI's
// error message verbatim — not wrapped behind an opaque 500.
func TestRunCronJobSurfacesEnvelopeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "error",
			"error":  "cronjob/ops/purge not found",
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"run", "cronjob", "ops/purge"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for 404")
	}

	if !strings.Contains(err.Error(), "ops/purge") {
		t.Errorf("error should surface server message, got %q", err.Error())
	}
}
