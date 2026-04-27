package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestConfigSet_PostsKeyValuePairs locks in the wire shape of `vd
// config set`: POST to /config?scope=&name= with a JSON object body.
// Without this, an encoding bug could send the pairs as form data
// or array, and the controller would reject silently.
func TestConfigSet_PostsKeyValuePairs(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotQuery  string
		gotBody   []byte
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)

		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"config", "set", "FOO=bar", "NODE_ENV=production", "-s", "clowk-lp", "-n", "web"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method=%q want POST", gotMethod)
	}

	if gotPath != "/config" {
		t.Errorf("path=%q want /config", gotPath)
	}

	for _, want := range []string{"scope=clowk-lp", "name=web"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query missing %q: %q", want, gotQuery)
		}
	}

	var payload map[string]string
	_ = json.Unmarshal(gotBody, &payload)

	if payload["FOO"] != "bar" || payload["NODE_ENV"] != "production" {
		t.Errorf("payload=%+v", payload)
	}
}

// TestConfigSet_RequiresScope guards the CLI input contract: scope
// is mandatory; without it the controller can't compute a key
// prefix and the error from the server would be late and confusing.
func TestConfigSet_RequiresScope(t *testing.T) {
	root := newRootCmd()

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"config", "set", "FOO=bar"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected scope-required error")
	}

	if !strings.Contains(err.Error(), "scope") {
		t.Errorf("error should mention scope: %q", err.Error())
	}
}

// TestConfigSet_NoRestartFlagPropagates confirms --no-restart
// flips restart=false on the wire so the operator can batch edits
// without triggering a reconcile per call.
func TestConfigSet_NoRestartFlagPropagates(t *testing.T) {
	var gotQuery string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery

		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"config", "set", "FOO=bar", "-s", "clowk-lp", "--no-restart"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !strings.Contains(gotQuery, "restart=false") {
		t.Errorf("--no-restart should add restart=false: %q", gotQuery)
	}
}

// TestConfigGet_ReturnsValueFromServer covers the GET path: the
// CLI prints the returned `KEY=VALUE` and exits cleanly. Stub
// returns the single-key shape that ?key= produces.
func TestConfigGet_ReturnsValueFromServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]string{"FOO": "bar"},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	out := captureStdout(t, func() {
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs([]string{"config", "get", "FOO", "-s", "clowk-lp"})

		if err := root.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
	})

	if !strings.Contains(out, "FOO=bar") {
		t.Errorf("output should contain FOO=bar: %q", out)
	}
}

// TestConfigList_PrintsAllKeysSorted confirms list order is
// deterministic — operators piping to grep/diff appreciate stable
// output that doesn't shuffle between invocations.
func TestConfigList_PrintsAllKeysSorted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"vars": map[string]string{
					"ZEBRA":      "z",
					"APPLE":      "a",
					"MIDDLE_KEY": "m",
				},
			},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	out := captureStdout(t, func() {
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs([]string{"config", "list", "-s", "clowk-lp"})

		if err := root.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
	})

	// Sorted: APPLE, MIDDLE_KEY, ZEBRA. Just check ordering.
	apple := strings.Index(out, "APPLE=")
	middle := strings.Index(out, "MIDDLE_KEY=")
	zebra := strings.Index(out, "ZEBRA=")

	if apple < 0 || middle < 0 || zebra < 0 {
		t.Fatalf("missing key: %q", out)
	}

	if !(apple < middle && middle < zebra) {
		t.Errorf("not sorted: APPLE=%d MIDDLE=%d ZEBRA=%d (%q)", apple, middle, zebra, out)
	}
}

// TestConfigAppFlag_SplitsScopeAndName covers the -a shorthand:
// `vd config X -a clowk-lp/web` must split into scope=clowk-lp,
// name=web on the wire — same shape as the long -s/-n form. A
// regression in the splitter would silently send half the address
// and the operator would mutate the wrong bucket.
func TestConfigAppFlag_SplitsScopeAndName(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantQuery []string
	}{
		{
			name:      "scope/name shape",
			args:      []string{"config", "set", "FOO=bar", "-a", "clowk-lp/web"},
			wantQuery: []string{"scope=clowk-lp", "name=web"},
		},
		{
			name:      "bare scope shape",
			args:      []string{"config", "set", "FOO=bar", "-a", "clowk-lp"},
			wantQuery: []string{"scope=clowk-lp"},
		},
		{
			name:      "list with -a",
			args:      []string{"config", "list", "-a", "clowk-lp/web"},
			wantQuery: []string{"scope=clowk-lp", "name=web"},
		},
		{
			name:      "get with -a",
			args:      []string{"config", "get", "FOO", "-a", "clowk-lp/web"},
			wantQuery: []string{"scope=clowk-lp", "name=web"},
		},
		{
			name:      "explicit -n overrides -a's name segment",
			args:      []string{"config", "set", "FOO=bar", "-a", "clowk-lp/web", "-n", "worker"},
			wantQuery: []string{"scope=clowk-lp", "name=worker"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotQuery string

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery

				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "ok",
					"data":   map[string]any{"vars": map[string]string{"FOO": "bar"}},
				})
			}))
			defer ts.Close()

			root := newRootCmd()
			_ = root.PersistentFlags().Set("controller-url", ts.URL)

			out := captureStdout(t, func() {
				var buf bytes.Buffer
				root.SetOut(&buf)
				root.SetErr(&buf)
				root.SetArgs(c.args)

				if err := root.Execute(); err != nil {
					t.Fatalf("execute: %v", err)
				}
			})
			_ = out

			for _, want := range c.wantQuery {
				if !strings.Contains(gotQuery, want) {
					t.Errorf("query missing %q: %q", want, gotQuery)
				}
			}

			// "name=worker" override case: confirm the original "web"
			// from -a does NOT show up.
			if c.name == "explicit -n overrides -a's name segment" {
				if strings.Contains(gotQuery, "name=web") {
					t.Errorf("explicit -n should win over -a, got %q", gotQuery)
				}
			}
		})
	}
}

// TestConfigUnset_DeletesEachKey checks that a multi-key unset
// fires one DELETE per key. Without this, a regression in the loop
// could silently delete only the first.
func TestConfigUnset_DeletesEachKey(t *testing.T) {
	var (
		mu      sync.Mutex
		deleted []string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}

		mu.Lock()
		deleted = append(deleted, r.URL.Query().Get("key"))
		mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	out := captureStdout(t, func() {
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs([]string{"config", "unset", "FOO", "BAR", "BAZ", "-s", "clowk-lp"})

		if err := root.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
	})

	mu.Lock()
	defer mu.Unlock()

	if len(deleted) != 3 {
		t.Errorf("expected 3 DELETEs, got %d: %v", len(deleted), deleted)
	}

	for _, want := range []string{"FOO", "BAR", "BAZ"} {
		found := false

		for _, d := range deleted {
			if d == want {
				found = true
				break
			}
		}

		if !found {
			t.Errorf("missing DELETE for %q (got %v)", want, deleted)
		}
	}

	for _, key := range []string{"FOO", "BAR", "BAZ"} {
		if !strings.Contains(out, "Unset "+key) {
			t.Errorf("output should confirm unset of %q: %q", key, out)
		}
	}
}
