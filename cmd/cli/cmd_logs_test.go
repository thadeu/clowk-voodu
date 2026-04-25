package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLogsRequestsControllerEndpoint checks the wire shape:
// `voodu logs job api/migrate` issues GET /logs with kind/scope/name
// query params populated. The mock server stands in for /logs so the
// test stays hermetic.
func TestLogsRequestsControllerEndpoint(t *testing.T) {
	var (
		gotMethod   string
		gotPath     string
		gotRawQuery string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery

		w.Header().Set("X-Voodu-Container", "test-migrate.abcd")
		w.Header().Set("X-Voodu-Run", "abcd")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello\n"))
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer

	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"logs", "job", "api/migrate"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method: got %q, want GET", gotMethod)
	}

	if gotPath != "/logs" {
		t.Errorf("path: got %q, want /logs", gotPath)
	}

	for _, want := range []string{"kind=job", "scope=api", "name=migrate"} {
		if !strings.Contains(gotRawQuery, want) {
			t.Errorf("query missing %q: %q", want, gotRawQuery)
		}
	}
}

// TestLogsForwardsRunFollowTail confirms the optional flags translate
// to query parameters the controller can consume. Without this the
// CLI's --follow / --tail / --run could silently no-op (hardest kind
// of bug to notice).
func TestLogsForwardsRunFollowTail(t *testing.T) {
	var gotRawQuery string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery

		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer

	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"logs", "cronjob", "test/crawler", "--run", "7e2a", "-f", "--tail", "100"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	for _, want := range []string{"kind=cronjob", "scope=test", "name=crawler", "run=7e2a", "follow=true", "tail=100"} {
		if !strings.Contains(gotRawQuery, want) {
			t.Errorf("query missing %q: %q", want, gotRawQuery)
		}
	}
}

// TestLogsBareNameOmitsScope mirrors the run-job test: a bare ref
// must NOT carry an empty scope query param, otherwise the controller's
// scope-omitted resolver path never fires.
func TestLogsBareNameOmitsScope(t *testing.T) {
	var gotRawQuery string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery

		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer

	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"logs", "job", "migrate"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if strings.Contains(gotRawQuery, "scope=") {
		t.Errorf("bare-name logs should omit scope query, got %q", gotRawQuery)
	}
}

// TestLogsSurfacesEnvelopeError checks that a JSON-envelope error from
// the controller (the shape /logs returns for resolution failures)
// becomes the CLI's error message verbatim — no opaque "controller
// returned 404" wrapper.
func TestLogsSurfacesEnvelopeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "error",
			"error":  "no run \"ffff\" found among 1 candidate(s)",
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer

	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"logs", "job", "api/migrate", "--run", "ffff"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for 404")
	}

	if !strings.Contains(err.Error(), "no run") {
		t.Errorf("error should surface server message verbatim, got %q", err.Error())
	}
}

// TestLogsRejectsUnsupportedKindClientSide locks in the per-kind
// allowlist: database / ingress / cluster don't produce voodu-managed
// containers, so we fail fast before ever opening the HTTP connection.
func TestLogsRejectsUnsupportedKindClientSide(t *testing.T) {
	var hits int

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var stdout bytes.Buffer

	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"logs", "database", "main"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unsupported kind")
	}

	if !strings.Contains(err.Error(), "does not produce containers") {
		t.Errorf("error should mention container-less kinds, got %q", err.Error())
	}

	if hits != 0 {
		t.Errorf("controller must not be hit for client-side rejection, got %d hits", hits)
	}
}
