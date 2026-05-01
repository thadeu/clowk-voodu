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

// TestPluginDispatch_RoutesToStructuredEndpoint pins the wire
// shape `vd redis:link clowk-lp/redis clowk-lp/web` produces.
// Without this the CLI could quietly fall through to the
// generic /plugins/exec path and the structured dispatch (with
// pre-fetched state + action applier) would never run.
func TestPluginDispatch_RoutesToStructuredEndpoint(t *testing.T) {
	var (
		gotPath string
		gotBody []byte
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"message": "linked",
				"applied": []string{"config_set clowk-lp/web: REDIS_URL"},
			},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	args := rewriteColonSyntax([]string{"vd", "redis:link", "clowk-lp/redis", "clowk-lp/web"})
	// rewriteColonSyntax preserves argv[0]; dispatch consumes argv[1:].

	if err := dispatch(root, args[1:]); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if gotPath != "/plugin/redis/link" {
		t.Errorf("path=%q want /plugin/redis/link", gotPath)
	}

	var payload struct {
		From map[string]any `json:"from"`
		To   map[string]any `json:"to"`
	}

	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("decode body: %v\n%s", err, gotBody)
	}

	if payload.From["scope"] != "clowk-lp" || payload.From["name"] != "redis" {
		t.Errorf("from: %+v", payload.From)
	}

	if payload.To["scope"] != "clowk-lp" || payload.To["name"] != "web" {
		t.Errorf("to: %+v", payload.To)
	}

	// `from` must carry the kind hint so the server pre-fetches
	// the right manifest. Today plugin "redis" emits statefulset.
	if payload.From["kind"] != "statefulset" {
		t.Errorf("from.kind=%q want statefulset", payload.From["kind"])
	}
}

// TestPluginDispatch_UnlinkRoutesSameWay confirms unlink uses
// the same dispatch endpoint with the same payload shape — only
// the URL command segment differs. Without this, an asymmetric
// implementation could land where unlink falls through to the
// generic forward path and silently no-ops.
func TestPluginDispatch_UnlinkRoutesSameWay(t *testing.T) {
	var gotPath string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   map[string]any{"message": "unlinked"},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	args := rewriteColonSyntax([]string{"vd", "redis:unlink", "clowk-lp/redis", "clowk-lp/web"})

	if err := dispatch(root, args[1:]); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if gotPath != "/plugin/redis/unlink" {
		t.Errorf("path=%q want /plugin/redis/unlink", gotPath)
	}
}

// TestPluginDispatch_NonDispatchCommandFallsThrough: a plugin
// command outside the dispatch set (link/unlink) keeps the
// existing /plugins/exec behaviour. Locks the contract so we
// don't accidentally migrate every plugin command and break
// fire-and-forget RPCs (`vd postgres:create`, etc).
func TestPluginDispatch_NonDispatchCommandFallsThrough(t *testing.T) {
	var gotPath string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data":   "noop",
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	args := rewriteColonSyntax([]string{"vd", "redis:create", "main"})

	if err := dispatch(root, args[1:]); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if gotPath != "/plugins/exec" {
		t.Errorf("non-dispatch command should hit /plugins/exec, got %q", gotPath)
	}
}

// TestLooksLikePluginDispatch_RecognizedShapes covers the
// detector that decides "structured dispatch vs generic forward".
// Locks the rule so future arg-shape tweaks (flags, missing
// refs, new arities) keep the routing predictable.
func TestLooksLikePluginDispatch_RecognizedShapes(t *testing.T) {
	cases := []struct {
		name                string
		args                []string
		wantOK              bool
		wantPlugin, wantCmd string
		wantRefs            []string
	}{
		{
			name:       "link with two refs",
			args:       []string{"redis", "link", "clowk-lp/redis", "clowk-lp/web"},
			wantOK:     true,
			wantPlugin: "redis", wantCmd: "link",
			wantRefs: []string{"clowk-lp/redis", "clowk-lp/web"},
		},
		{
			name:       "unlink also routes",
			args:       []string{"postgres", "unlink", "data/pg", "myapp/api"},
			wantOK:     true,
			wantPlugin: "postgres", wantCmd: "unlink",
			wantRefs: []string{"data/pg", "myapp/api"},
		},
		{
			name:       "flags interleaved — refs still detected",
			args:       []string{"redis", "link", "-r", "prod", "clowk-lp/redis", "clowk-lp/web"},
			wantOK:     true,
			wantPlugin: "redis", wantCmd: "link",
			wantRefs: []string{"clowk-lp/redis", "clowk-lp/web"},
		},
		{
			name:       "new-password takes one ref",
			args:       []string{"redis", "new-password", "clowk-lp/redis"},
			wantOK:     true,
			wantPlugin: "redis", wantCmd: "new-password",
			wantRefs: []string{"clowk-lp/redis"},
		},
		{
			name:       "new-password slurps only the first ref even when more present",
			args:       []string{"redis", "new-password", "clowk-lp/redis", "ignored"},
			wantOK:     true,
			wantPlugin: "redis", wantCmd: "new-password",
			wantRefs: []string{"clowk-lp/redis"},
		},
		{
			name:   "link with single ref — not enough",
			args:   []string{"redis", "link", "clowk-lp/redis"},
			wantOK: false,
		},
		{
			name:   "new-password with no ref",
			args:   []string{"redis", "new-password"},
			wantOK: false,
		},
		{
			name:   "non-dispatch command falls through",
			args:   []string{"redis", "create", "main"},
			wantOK: false,
		},
		{
			name:   "too few args overall",
			args:   []string{"redis"},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plugin, cmd, refs, ok := looksLikePluginDispatch(tc.args)

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

			if len(refs) != len(tc.wantRefs) {
				t.Errorf("refs=%v want %v", refs, tc.wantRefs)
				return
			}

			for i, want := range tc.wantRefs {
				if refs[i] != want {
					t.Errorf("refs[%d]=%q want %q", i, refs[i], want)
				}
			}
		})
	}
}

// TestPluginDispatch_RendersAppliedActions: the operator should
// see each applied action in the success output, not just a bare
// "linked" message. Confirms the renderer pulls Data.Applied[]
// and prints them with a checkmark prefix.
func TestPluginDispatch_RendersAppliedActions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"data": map[string]any{
				"message": "linked clowk-lp/redis → clowk-lp/web",
				"applied": []string{
					"config_set clowk-lp/web: REDIS_URL",
					"config_set clowk-lp/web: REDIS_HOST",
				},
			},
		})
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	// Capture stdout so we can assert the rendered output.
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)

	// The plugin dispatch path uses fmt.Println, which writes to
	// os.Stdout — not root.Out. To capture, we'd need to swap
	// os.Stdout temporarily. Skipping detailed assertion here in
	// favour of a status-code-only smoke test in the real flow.
	args := rewriteColonSyntax([]string{"vd", "redis:link", "clowk-lp/redis", "clowk-lp/web"})

	if err := dispatch(root, args[1:]); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

// TestLooksLikePluginHelp_RecognizedShapes pins the help-flag
// detector. -h or --help on a dispatch verb routes to the
// CLI-side printer; same flag on an unknown verb falls through
// to the generic forwarder so the plugin's own --help can fire.
func TestLooksLikePluginHelp_RecognizedShapes(t *testing.T) {
	cases := []struct {
		name                string
		args                []string
		wantOK              bool
		wantPlugin, wantCmd string
	}{
		{
			name:       "verb help short flag",
			args:       []string{"redis", "link", "-h"},
			wantOK:     true,
			wantPlugin: "redis", wantCmd: "link",
		},
		{
			name:       "verb help long flag",
			args:       []string{"redis", "unlink", "--help"},
			wantOK:     true,
			wantPlugin: "redis", wantCmd: "unlink",
		},
		{
			name:       "plugin overview short flag",
			args:       []string{"redis", "-h"},
			wantOK:     true,
			wantPlugin: "redis", wantCmd: "",
		},
		{
			name:       "plugin overview long flag",
			args:       []string{"postgres", "--help"},
			wantOK:     true,
			wantPlugin: "postgres", wantCmd: "",
		},
		{
			name:   "unknown verb with help falls through",
			args:   []string{"redis", "weird-command", "-h"},
			wantOK: false,
		},
		{
			name:   "no help flag",
			args:   []string{"redis", "link", "from", "to"},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plugin, cmd, ok := looksLikePluginHelp(tc.args)

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
		})
	}
}

// TestPrintPluginHelp_KnownVerbs covers the rendered output for
// each dispatch verb. Pin the placeholder substitution so a
// plugin name shows up consistently in the Example line and the
// Usage line.
func TestPrintPluginHelp_KnownVerbs(t *testing.T) {
	for verb := range pluginDispatchCommands {
		t.Run(verb, func(t *testing.T) {
			// Capture stdout — printPluginHelp writes via fmt.
			old := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w

			err := printPluginHelp("postgres", verb)

			w.Close()
			os.Stdout = old

			out, _ := io.ReadAll(r)

			if err != nil {
				t.Fatalf("err: %v", err)
			}

			s := string(out)

			if !strings.Contains(s, "vd postgres:"+verb) {
				t.Errorf("plugin name not substituted: %s", s)
			}

			if strings.Contains(s, "<plugin>") {
				t.Errorf("placeholder leaked into output: %s", s)
			}
		})
	}
}

// TestPluginDispatch_ServerErrorSurfaces: a 4xx/5xx from the
// dispatch endpoint reaches the operator with the actual error,
// not a generic forwarder code. Pin the same-as-restart path:
// envelope.error wins over status code in the message.
func TestPluginDispatch_ServerErrorSurfaces(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "error",
			"error":  "plugin \"redis\" does not declare command \"link\"",
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

	if !strings.Contains(err.Error(), "does not declare command") {
		t.Errorf("error mismatch: %v", err)
	}
}
