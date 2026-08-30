package controller

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// TestHostPortModeClassifies gives a plugin the one fact it cannot
// work out for itself without reimplementing normalizePort — and
// reimplementing it means drifting from it.
func TestHostPortModeClassifies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ports []string
		want  string
	}{
		{"nothing published", nil, hostPortsNone},
		{"docker picks the host side", []string{"8080"}, hostPortsEphemeral},
		{"explicit ephemeral", []string{"0.0.0.0::8084"}, hostPortsEphemeral},
		{"pinned", []string{"3000:80"}, hostPortsFixed},
		{"pinned with interface", []string{"0.0.0.0:8084:8084"}, hostPortsFixed},
		{"one pinned among many", []string{"8080", "3000:80"}, hostPortsFixed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostPortMode(deploymentSpec{Ports: tc.ports}); got != tc.want {
				t.Errorf("hostPortMode(%v) = %q, want %q", tc.ports, got, tc.want)
			}
		})
	}
}

// TestHostPortModeAgreesWithSurge pins the two to the same source. A
// plugin refusing a config the rollout would happily surge — or the
// reverse — is the kind of disagreement that only shows up in
// production.
func TestHostPortModeAgreesWithSurge(t *testing.T) {
	for _, ports := range [][]string{nil, {"8080"}, {"3000:80"}, {"0.0.0.0::9000"}, {"8080", "3000:80"}} {
		spec := deploymentSpec{Ports: ports}

		if (hostPortMode(spec) == hostPortsFixed) != !canSurge(spec) {
			t.Errorf("ports %v: hostPortMode says %q but canSurge says %v", ports, hostPortMode(spec), canSurge(spec))
		}
	}
}

// TestNestedExpandTellsThePluginTheHostPortMode is why it exists: the
// plugin refuses a load balancer in front of a pinned host port, and it
// should not have to parse the parent's ports to find out.
func TestNestedExpandTellsThePluginTheHostPortMode(t *testing.T) {
	dir := t.TempDir()
	seen := dir + "/req.json"

	writePluginScript(t, dir+"/expand", `#!/usr/bin/env bash
cat > `+seen+`
echo '{"status":"ok","data":{}}'
`)

	api := &API{PluginBlocks: &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{
		"trafik": {Manifest: plugin.Manifest{Name: "trafik"}, Dir: dir, Commands: map[string]string{"expand": dir + "/expand"}},
	}}}

	m := &Manifest{
		Kind: KindDeployment, Scope: "prod", Name: "esl",
		Spec: json.RawMessage(`{"image":"esl:1","ports":["3000:80"],` +
			`"plugin_blocks":[{"type":"trafik","spec":{"port":8084}}]}`),
	}

	if _, _, _, err := api.expandPluginBlocks(context.Background(), []*Manifest{m}); err != nil {
		t.Fatalf("expand: %v", err)
	}

	raw, err := os.ReadFile(seen)

	if err != nil {
		t.Fatalf("plugin never ran: %v", err)
	}

	if want := `"host_ports":"fixed"`; !strings.Contains(string(raw), want) {
		t.Errorf("request missing %s\ngot: %s", want, raw)
	}
}
