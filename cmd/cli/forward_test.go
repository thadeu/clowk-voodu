package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestForwardPostsArgsAndRendersJSON(t *testing.T) {
	var got forwardRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/exec" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}

		body, _ := io.ReadAll(r.Body)

		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}

		resp := forwardResponse{Status: "ok", Data: json.RawMessage(`{"name":"main","version":"16"}`)}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	t.Setenv(envControllerURL, srv.URL)

	stdout := captureStdout(t, func() {
		root := newRootCmd()

		if err := forwardToController(root, []string{"postgres", "create", "main"}); err != nil {
			t.Fatalf("forward: %v", err)
		}
	})

	if !containsAll(got.Args, "postgres", "create", "main") {
		t.Errorf("request args missing: %v", got.Args)
	}

	if !strings.Contains(stdout, `"name": "main"`) {
		t.Errorf("stdout missing rendered data: %q", stdout)
	}
}

func TestForwardSurfacesErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(forwardResponse{Status: "error", Error: "boom"})
	}))
	defer srv.Close()

	t.Setenv(envControllerURL, srv.URL)

	root := newRootCmd()
	err := forwardToController(root, []string{"hello"})

	if err == nil || err.Error() != "boom" {
		t.Errorf("expected boom, got %v", err)
	}
}

func TestForwardHandlesControllerDown(t *testing.T) {
	t.Setenv(envControllerURL, "http://127.0.0.1:1") // unreachable

	root := newRootCmd()
	err := forwardToController(root, []string{"nope"})

	if err == nil {
		t.Fatal("expected error when controller down")
	}

	if !strings.Contains(err.Error(), "unknown command") || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error missing hint: %v", err)
	}
}

func containsAll(slice []string, parts ...string) bool {
	for _, p := range parts {
		found := false
		for _, s := range slice {
			if s == p {
				found = true
				break
			}
		}

		if !found {
			return false
		}
	}

	return true
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	orig := os.Stdout
	os.Stdout = w

	done := make(chan []byte)

	go func() {
		var buf bytes.Buffer

		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()

	f()

	_ = w.Close()
	os.Stdout = orig

	return string(<-done)
}
