package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRestartPostsCorrectQuery locks in the wire shape: `vd restart
// clowk-lp/web` POSTs to /restart with kind=deployment + scope +
// name. Without this assert, a regression in splitJobRef or the
// query builder could quietly send the wrong kind and miss the
// rolling-restart path entirely.
func TestRestartPostsCorrectQuery(t *testing.T) {
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
			"data":   map[string]string{"scope": "clowk-lp", "name": "web"},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"restart", "clowk-lp/web"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method=%q want POST", gotMethod)
	}

	if gotPath != "/restart" {
		t.Errorf("path=%q want /restart", gotPath)
	}

	for _, want := range []string{"kind=deployment", "scope=clowk-lp", "name=web"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query missing %q: %q", want, gotQuery)
		}
	}
}

// TestRestartBareNameOmitsScope confirms `vd restart web` (no
// slash) sends only ?name= and lets the server's resolveScope find
// the unique match. Without this, a stray empty `scope=` would
// break the disambiguator.
func TestRestartBareNameOmitsScope(t *testing.T) {
	var gotQuery string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]string{"scope": "clowk-lp", "name": "web"},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"restart", "web"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if strings.Contains(gotQuery, "scope=") {
		t.Errorf("bare name must omit scope query, got %q", gotQuery)
	}

	if !strings.Contains(gotQuery, "name=web") {
		t.Errorf("query missing name=web: %q", gotQuery)
	}
}

// TestRestartSurfacesEnvelopeError keeps the operator-friendly
// error path: a 4xx/5xx from the server with a JSON envelope
// reaches the CLI as the server's verbatim message, not a generic
// "controller returned N".
func TestRestartSurfacesEnvelopeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "error",
			"error":  "deployment/clowk-lp/missing not found",
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"restart", "clowk-lp/missing"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for 404")
	}

	if !strings.Contains(err.Error(), "missing not found") {
		t.Errorf("error should surface server message, got %q", err.Error())
	}
}
