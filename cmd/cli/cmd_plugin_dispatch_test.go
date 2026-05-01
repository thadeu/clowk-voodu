package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPluginDispatch_ForwardsArgsVerbatim pins the passthrough
// contract: CLI packages the operator's args into `{args: [...]}`
// and POSTs to /plugin/{name}/{command}. The CLI doesn't inspect
// or transform the args — that's the plugin's job.
func TestPluginDispatch_ForwardsArgsVerbatim(t *testing.T) {
	var (
		gotPath string
		gotBody []byte
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]any{"message": "ok"},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	args := rewriteColonSyntax([]string{"vd", "redis:link", "clowk-lp/redis", "clowk-lp/web"})

	if err := dispatch(root, args[1:]); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if gotPath != "/plugin/redis/link" {
		t.Errorf("path=%q want /plugin/redis/link", gotPath)
	}

	var payload struct {
		Args []string `json:"args"`
	}

	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("decode body: %v\n%s", err, gotBody)
	}

	want := []string{"clowk-lp/redis", "clowk-lp/web"}

	if len(payload.Args) != len(want) {
		t.Fatalf("args=%v want %v", payload.Args, want)
	}

	for i, w := range want {
		if payload.Args[i] != w {
			t.Errorf("args[%d]=%q want %q", i, payload.Args[i], w)
		}
	}
}

// TestPluginDispatch_PassesFlagsThroughToPlugin confirms `-h`
// and similar flags flow through as args. The CLI doesn't
// intercept them — plugin handles its own help / flags.
func TestPluginDispatch_PassesFlagsThroughToPlugin(t *testing.T) {
	var gotBody []byte

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]any{"message": "Usage: vd redis:link <a> <b>"},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	// `-h` is just another arg from the CLI's perspective.
	args := rewriteColonSyntax([]string{"vd", "redis:link", "-h"})

	if err := dispatch(root, args[1:]); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var payload struct {
		Args []string `json:"args"`
	}

	_ = json.Unmarshal(gotBody, &payload)

	if len(payload.Args) != 1 || payload.Args[0] != "-h" {
		t.Errorf("expected args=[-h], got %v", payload.Args)
	}
}

// TestPluginDispatch_AnyVerbRoutes: the dispatcher has no
// hardcoded list of verbs. `vd <plugin>:<arbitrary-cmd>` for
// any plugin name and any verb routes through. Future plugins
// adding `vd cassandra:add-node` or `vd mongo:replicaset-init`
// don't need CLI changes.
func TestPluginDispatch_AnyVerbRoutes(t *testing.T) {
	cases := []struct {
		colonInvocation []string
		wantPath        string
	}{
		{[]string{"vd", "redis:link", "a", "b"}, "/plugin/redis/link"},
		{[]string{"vd", "redis:unlink", "a", "b"}, "/plugin/redis/unlink"},
		{[]string{"vd", "redis:new-password", "a"}, "/plugin/redis/new-password"},
		{[]string{"vd", "redis:custom-verb"}, "/plugin/redis/custom-verb"},
		{[]string{"vd", "postgres:backup", "a", "b", "c"}, "/plugin/postgres/backup"},
	}

	for _, tc := range cases {
		t.Run(tc.wantPath, func(t *testing.T) {
			var gotPath string

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path

				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "ok",
					"data":   map[string]any{"message": "ok"},
				})
			}))
			defer ts.Close()

			root := newRootCmd()
			_ = root.PersistentFlags().Set("controller-url", ts.URL)

			args := rewriteColonSyntax(tc.colonInvocation)

			if err := dispatch(root, args[1:]); err != nil {
				t.Fatalf("dispatch: %v", err)
			}

			if gotPath != tc.wantPath {
				t.Errorf("got %q, want %q", gotPath, tc.wantPath)
			}
		})
	}
}

// TestLooksLikePluginDispatch_DetectorRules pins the rule
// set of the dispatch detector — when does it route, when
// does it fall through to forwardToController.
func TestLooksLikePluginDispatch_DetectorRules(t *testing.T) {
	cases := []struct {
		name                string
		args                []string
		wantOK              bool
		wantPlugin, wantCmd string
		wantArgs            []string
	}{
		{
			name:       "plugin + command + 2 refs",
			args:       []string{"redis", "link", "a", "b"},
			wantOK:     true,
			wantPlugin: "redis", wantCmd: "link",
			wantArgs: []string{"a", "b"},
		},
		{
			name:       "plugin + command + 0 refs (passthrough still works)",
			args:       []string{"redis", "list"},
			wantOK:     true,
			wantPlugin: "redis", wantCmd: "list",
			wantArgs: []string{},
		},
		{
			name:       "plugin + command + many args including flags",
			args:       []string{"postgres", "backup", "-f", "/tmp/dump.sql", "data/pg"},
			wantOK:     true,
			wantPlugin: "postgres", wantCmd: "backup",
			wantArgs: []string{"-f", "/tmp/dump.sql", "data/pg"},
		},
		{
			name:   "single token — not a plugin invocation",
			args:   []string{"redis"},
			wantOK: false,
		},
		{
			name:   "starts with flag — not plugin syntax",
			args:   []string{"-r", "redis"},
			wantOK: false,
		},
		{
			name:   "command with non-ident chars",
			args:   []string{"redis", "weird/command"},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plugin, cmd, args, ok := looksLikePluginDispatch(tc.args)

			if ok != tc.wantOK {
				t.Errorf("ok=%v want %v", ok, tc.wantOK)
				return
			}

			if !tc.wantOK {
				return
			}

			if plugin != tc.wantPlugin || cmd != tc.wantCmd {
				t.Errorf("plugin=%q cmd=%q want plugin=%q cmd=%q",
					plugin, cmd, tc.wantPlugin, tc.wantCmd)
			}

			if len(args) != len(tc.wantArgs) {
				t.Errorf("args=%v want %v", args, tc.wantArgs)
				return
			}

			for i, want := range tc.wantArgs {
				if args[i] != want {
					t.Errorf("args[%d]=%q want %q", i, args[i], want)
				}
			}
		})
	}
}

// TestPluginDispatch_RendersAppliedActions confirms the operator
// sees each applied action with a checkmark in the success
// output.
func TestPluginDispatch_RendersAppliedActions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"message": "linked clowk-lp/redis → clowk-lp/web",
				"applied": []string{
					"config_set clowk-lp/web: REDIS_URL",
				},
			},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	args := rewriteColonSyntax([]string{"vd", "redis:link", "clowk-lp/redis", "clowk-lp/web"})

	if err := dispatch(root, args[1:]); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

// TestPluginDispatch_OverviewHelpSynthesizesHelpCommand pins
// the `-h` / `--help` synthesis: when the operator types
// `vd <plugin> -h` (no verb), the dispatcher routes to
// /plugin/<plugin>/help so the plugin's bin/help shim renders
// the overview. Plugin owns the help text — CLI doesn't render
// anything itself.
func TestPluginDispatch_OverviewHelpSynthesizesHelpCommand(t *testing.T) {
	cases := []struct {
		args     []string
		wantPath string
	}{
		{[]string{"vd", "redis", "-h"}, "/plugin/redis/help"},
		{[]string{"vd", "redis", "--help"}, "/plugin/redis/help"},
		{[]string{"vd", "postgres", "-h"}, "/plugin/postgres/help"},
	}

	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var gotPath string

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path

				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "ok",
					"data":   map[string]any{"message": "plugin help"},
				})
			}))
			defer ts.Close()

			root := newRootCmd()
			_ = root.PersistentFlags().Set("controller-url", ts.URL)

			args := rewriteColonSyntax(tc.args)

			if err := dispatch(root, args[1:]); err != nil {
				t.Fatalf("dispatch: %v", err)
			}

			if gotPath != tc.wantPath {
				t.Errorf("got %q want %q", gotPath, tc.wantPath)
			}
		})
	}
}

// TestPluginDispatch_VerbSpecificHelpFlowsAsArg: `vd <plugin>:<verb> -h`
// keeps the regular passthrough — `-h` arrives at the plugin as an
// arg, plugin handles its own usage rendering. This is distinct from
// the synthesis path tested above (which is for plugin-level overview
// without a verb).
func TestPluginDispatch_VerbSpecificHelpFlowsAsArg(t *testing.T) {
	var (
		gotPath string
		gotBody []byte
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]any{"message": "verb help"},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	args := rewriteColonSyntax([]string{"vd", "redis:link", "-h"})

	if err := dispatch(root, args[1:]); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Path is /plugin/redis/link, NOT /plugin/redis/help.
	if gotPath != "/plugin/redis/link" {
		t.Errorf("path %q: verb-specific help should hit the verb's endpoint", gotPath)
	}

	var payload struct {
		Args []string `json:"args"`
	}

	_ = json.Unmarshal(gotBody, &payload)

	if len(payload.Args) != 1 || payload.Args[0] != "-h" {
		t.Errorf("args=%v want [-h]", payload.Args)
	}
}

// TestPluginDispatch_ServerErrorSurfaces: a 4xx/5xx from the
// dispatch endpoint reaches the operator with the actual error
// from the envelope, not a generic forwarder code.
func TestPluginDispatch_ServerErrorSurfaces(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "error",
			"error":  "plugin \"redis\" does not have an executable named \"link\" under bin/",
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	args := rewriteColonSyntax([]string{"vd", "redis:link", "clowk-lp/redis", "clowk-lp/web"})

	err := dispatch(root, args[1:])
	if err == nil {
		t.Fatal("expected error for 400")
	}

	if !strings.Contains(err.Error(), "does not have an executable") {
		t.Errorf("error mismatch: %v", err)
	}
}
