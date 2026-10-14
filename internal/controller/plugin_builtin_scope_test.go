package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// builtinScopeAPI wires a builtin plugin (fswapi) and a
// regular one (postgres) behind the fake registry. Both expand scripts echo
// one deployment, so a test that passes the guard also proves expansion ran.
func builtinScopeAPI(t *testing.T) *API {
	t.Helper()

	dir := t.TempDir()

	expandPath := dir + "/expand"
	writePluginScript(t, expandPath, `#!/usr/bin/env bash
read -r line
echo '{"status":"ok","data":{"kind":"deployment","scope":"plugins","name":"fswapi","spec":{"image":"voodu-fswapi:0.1.0"}}}'
`)

	fswapi := &plugins.LoadedPlugin{
		Manifest: plugin.Manifest{
			Name:   "fswapi",
			Routes: &plugin.RouteConfig{Prefix: "fswapi", PortEnv: "FREESWITCH_API_ADDR", Builtin: true},
		},
		Dir:      dir,
		Commands: map[string]string{"expand": expandPath},
	}

	postgres := &plugins.LoadedPlugin{
		Manifest: plugin.Manifest{Name: "postgres"},
		Dir:      dir,
		Commands: map[string]string{"expand": expandPath},
	}

	return &API{PluginBlocks: &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{
		"fswapi":   fswapi,
		"postgres": postgres,
	}}}
}

func TestBuiltinScope_PluginOwnResourceIsAllowed(t *testing.T) {
	a := builtinScopeAPI(t)

	// The plugin name is the kind operators write the override with.
	in := []*Manifest{{Kind: "fswapi", Scope: "plugins", Name: "fswapi", Spec: json.RawMessage(`{}`)}}

	out, _, _, err := a.expandPluginBlocks(context.Background(), in)
	if err != nil {
		t.Fatalf("the builtin plugin's own resource must apply: %v", err)
	}

	if len(out) != 1 || out[0].Kind != KindDeployment || out[0].Scope != "plugins" {
		t.Errorf("expanded = %+v, want deployment plugins/fswapi", out)
	}
}

func TestBuiltinScope_Refusals(t *testing.T) {
	tests := []struct {
		name     string
		manifest *Manifest
		wantMsg  string
	}{
		{
			name:     "core kind",
			manifest: &Manifest{Kind: KindDeployment, Scope: "plugins", Name: "web"},
			wantMsg:  "use another scope",
		},
		{
			name:     "plugin without builtin",
			manifest: &Manifest{Kind: "postgres", Scope: "plugins", Name: "postgres"},
			wantMsg:  "use another scope",
		},
		{
			name:     "builtin plugin under another name",
			manifest: &Manifest{Kind: "fswapi", Scope: "plugins", Name: "api"},
			wantMsg:  "owns only plugins/fswapi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := builtinScopeAPI(t)

			tt.manifest.Spec = json.RawMessage(`{}`)

			_, _, _, err := a.expandPluginBlocks(context.Background(), []*Manifest{tt.manifest})
			if err == nil {
				t.Fatal("apply into the reserved scope must be refused")
			}

			if !strings.Contains(err.Error(), `scope "plugins" is reserved`) || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want the reserved-scope reason with %q", err, tt.wantMsg)
			}
		})
	}
}

func TestBuiltinScope_OtherScopesUnaffected(t *testing.T) {
	a := builtinScopeAPI(t)

	in := []*Manifest{
		{Kind: KindDeployment, Scope: "fsw", Name: "web", Spec: json.RawMessage(`{}`)},
		{Kind: "postgres", Scope: "data", Name: "main", Spec: json.RawMessage(`{}`)},
	}

	if _, _, _, err := a.expandPluginBlocks(context.Background(), in); err != nil {
		t.Fatalf("the guard must only touch the plugins scope: %v", err)
	}
}
