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
// config <ref> set`: POST to /config?scope=&name= with a JSON object
// body. Without this, an encoding bug could send the pairs as form
// data or array, and the controller would reject silently.
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
	root.SetArgs([]string{"config", "clowk-lp/web", "set", "FOO=bar", "NODE_ENV=production"})

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

// TestConfigSet_RequiresRef guards the CLI input contract: ref is
// mandatory; without it the controller can't compute a key prefix
// and the error from the server would be late and confusing.
func TestConfigSet_RequiresRef(t *testing.T) {
	root := newRootCmd()

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"config"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected ref-required error")
	}
}

// TestConfigSet_RejectsBareSetWithoutRef catches the most likely
// muscle-memory mistake from operators coming off the old `-s/-n`
// shape: typing `vd config set FOO=bar` (verb first, no ref). With
// the new ref-first parse, that maps to ref="set" verb="FOO=bar"
// which lands as "unknown verb" — surface a clear-er error.
func TestConfigSet_RejectsBareSetWithoutRef(t *testing.T) {
	root := newRootCmd()

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"config", "set", "FOO=bar"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing ref")
	}

	// "set" was parsed as the ref (a bare scope), and "FOO=bar"
	// became the verb — landing on the unknown-verb branch. The
	// exact error wording isn't load-bearing; just confirm we don't
	// silently accept the legacy shape.
	if !strings.Contains(err.Error(), "unknown") && !strings.Contains(err.Error(), "verb") {
		t.Errorf("error should call out unknown verb / wrong shape: %q", err.Error())
	}
}

// TestConfigSet_NoRestartFlagPropagates confirms --no-restart
// flips restart=false on the wire so the operator can batch edits
// without triggering a reconcile per call. Three invocation shapes
// are exercised — long form, colon shorthand with the flag before
// the ref, colon shorthand with the flag after the args — because
// each goes through a different rewrite path and a regression in
// any of them would silently leave the operator restarting on
// every set.
func TestConfigSet_NoRestartFlagPropagates(t *testing.T) {
	cases := []struct {
		name string
		// argv is what the user typed (post-`voodu` prefix). We run
		// rewriteColonSyntax over it the same way main() does, so
		// the colon shorthand reaches cobra in its rewritten shape.
		argv []string
	}{
		{
			name: "long form",
			argv: []string{"voodu", "config", "clowk-lp", "set", "FOO=bar", "--no-restart"},
		},
		{
			name: "colon shorthand: flag before ref",
			argv: []string{"voodu", "config:set", "--no-restart", "clowk-lp", "FOO=bar"},
		},
		{
			name: "colon shorthand: flag after args",
			argv: []string{"voodu", "config:set", "clowk-lp", "FOO=bar", "--no-restart"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotQuery string

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery

				_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			}))
			defer ts.Close()

			root := newRootCmd()
			_ = root.PersistentFlags().Set("controller-url", ts.URL)

			rewritten := rewriteColonSyntax(c.argv)

			var buf bytes.Buffer
			root.SetOut(&buf)
			root.SetErr(&buf)
			root.SetArgs(rewritten[1:]) // drop the `voodu` argv[0]

			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}

			if !strings.Contains(gotQuery, "restart=false") {
				t.Errorf("--no-restart should add restart=false: %q (argv=%v rewritten=%v)",
					gotQuery, c.argv, rewritten)
			}
		})
	}
}

// TestConfigGet_FiltersBySubstring covers the substring-match
// contract: `vd config <ref> get LOG` returns every key whose name
// contains LOG (case-insensitive). Match-all rather than exact-key
// because operators usually remember "the var about logging" not
// the exact spelling.
func TestConfigGet_FiltersBySubstring(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"vars": map[string]string{
					"LOG_LEVEL":   "debug",
					"RAILS_LOG":   "stdout",
					"DATABASE":    "postgres",
					"NODE_ENV":    "production",
					"BUNDLE_PATH": "vendor",
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
		root.SetArgs([]string{"config", "clowk-lp/web", "get", "LOG"})

		if err := root.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
	})

	for _, want := range []string{"LOG_LEVEL=debug", "RAILS_LOG=stdout"} {
		if !strings.Contains(out, want) {
			t.Errorf("substring match should include %q: %q", want, out)
		}
	}

	for _, banned := range []string{"DATABASE", "NODE_ENV", "BUNDLE_PATH"} {
		if strings.Contains(out, banned) {
			t.Errorf("substring match should NOT include %q: %q", banned, out)
		}
	}
}

// TestConfigGet_NoMatchErrors confirms an explicit pattern with
// zero hits returns an error (not silent empty output) — operator
// typos like `get LIG` should land as "no keys match" instead of
// being indistinguishable from "no env vars set at all".
func TestConfigGet_NoMatchErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]any{"vars": map[string]string{"FOO": "bar"}},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"config", "clowk-lp", "get", "DOESNOTEXIST"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for no-match pattern")
	}

	if !strings.Contains(err.Error(), "DOESNOTEXIST") {
		t.Errorf("error should mention the pattern: %q", err.Error())
	}
}

// TestConfigDefaultVerbIsList covers the bareword shape:
// `vd config clowk-lp/web` (no verb) defaults to list. Same
// ergonomic move release/rollback already make.
func TestConfigDefaultVerbIsList(t *testing.T) {
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
		root.SetArgs([]string{"config", "clowk-lp"})

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

// TestConfigRef_SplitsScopeAndName covers the positional ref
// parser: `clowk-lp/web` becomes scope=clowk-lp, name=web; bare
// `clowk-lp` is scope-only. Same disambiguation rule the rest of
// the CLI uses, so muscle memory transfers between commands.
func TestConfigRef_SplitsScopeAndName(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantQuery []string
		bannedQ   []string
	}{
		{
			name:      "scope/name shape",
			args:      []string{"config", "clowk-lp/web", "set", "FOO=bar"},
			wantQuery: []string{"scope=clowk-lp", "name=web"},
		},
		{
			name:      "bare scope shape",
			args:      []string{"config", "clowk-lp", "set", "FOO=bar"},
			wantQuery: []string{"scope=clowk-lp"},
			bannedQ:   []string{"name="},
		},
		{
			name:      "default verb (list) with scope/name",
			args:      []string{"config", "clowk-lp/web"},
			wantQuery: []string{"scope=clowk-lp", "name=web"},
		},
		{
			name:      "get without pattern targets ref correctly",
			args:      []string{"config", "clowk-lp/web", "get"},
			wantQuery: []string{"scope=clowk-lp", "name=web"},
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

			for _, banned := range c.bannedQ {
				if strings.Contains(gotQuery, banned) {
					t.Errorf("query should NOT contain %q: %q", banned, gotQuery)
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
		root.SetArgs([]string{"config", "clowk-lp", "unset", "FOO", "BAR", "BAZ"})

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
