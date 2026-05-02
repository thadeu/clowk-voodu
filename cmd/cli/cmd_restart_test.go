package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRestartPostsCorrectQuery locks in the wire shape: `vd
// restart clowk-lp/web` POSTs to /restart with scope + name and
// NO kind — the server auto-detects which kind has a manifest at
// (scope, name). The CLI no longer assumes deployment; the
// kind=deployment query was a footgun for redis/postgres that
// forced operators to remember `-k statefulset`.
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
			"data":   map[string]string{"scope": "clowk-lp", "name": "web", "kind": "deployment"},
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

	for _, want := range []string{"scope=clowk-lp", "name=web"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query missing %q: %q", want, gotQuery)
		}
	}

	if strings.Contains(gotQuery, "kind=") {
		t.Errorf("query should NOT include kind when operator omits -k (server auto-detects); got %q", gotQuery)
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

// TestRestartKindFlag pins the -k/--kind plumbing for the
// disambiguation case: when both a deployment AND a statefulset
// exist under the same name, the operator passes -k to pick.
// Bare invocation (no -k) sends NO kind on the wire — server
// auto-detects.
func TestRestartKindFlag(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantKind    string // "" when no kind should be sent
		expectKind  bool
	}{
		{
			name:       "no -k, server auto-detects",
			args:       []string{"restart", "clowk-lp/redis"},
			expectKind: false,
		},
		{
			name:       "explicit -k statefulset",
			args:       []string{"restart", "-k", "statefulset", "clowk-lp/redis"},
			wantKind:   "statefulset",
			expectKind: true,
		},
		{
			name:       "long form --kind=statefulset",
			args:       []string{"restart", "--kind=statefulset", "clowk-lp/redis"},
			wantKind:   "statefulset",
			expectKind: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery

				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "ok",
					"data":   map[string]string{"scope": "clowk-lp", "name": "redis", "kind": "statefulset"},
				})
			}))
			defer ts.Close()

			root := newRootCmd()
			_ = root.PersistentFlags().Set("controller-url", ts.URL)

			var buf bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&buf)
			root.SetArgs(tc.args)

			if err := root.Execute(); err != nil {
				t.Fatalf("execute %v: %v", tc.args, err)
			}

			if tc.expectKind {
				want := "kind=" + tc.wantKind
				if !strings.Contains(gotQuery, want) {
					t.Errorf("args=%v: query=%q, want contains %q", tc.args, gotQuery, want)
				}
			} else if strings.Contains(gotQuery, "kind=") {
				t.Errorf("args=%v: query should NOT include kind (server auto-detects); got %q", tc.args, gotQuery)
			}
		})
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
