package controller

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// TestStatefulsetDrainsBeforeRemoving closes an asymmetry that is worse
// than a missing feature: the parser accepts a traffik block and a drain
// block on a statefulset, and the reconcile publishes its endpoints, so
// everything says the workload is covered. Without this the roll still
// removed the pod out from under whatever was running through it.
//
// A statefulset cannot surge — it reuses the ordinal-derived container
// name, and two containers cannot share one — so draining first is the
// only protection it can have.
func TestStatefulsetDrainsBeforeRemoving(t *testing.T) {
	dir := t.TempDir()
	seen := dir + "/req.json"

	writePluginScript(t, dir+"/drain", `#!/usr/bin/env bash
cat > `+seen+`
echo '{"status":"ok"}'
`)

	reg := &fakePluginRegistry{plugins: map[string]*plugins.LoadedPlugin{
		"traffik": {Manifest: plugin.Manifest{Name: "traffik"}, Dir: dir, Commands: map[string]string{"drain": dir + "/drain"}},
	}}

	h := &StatefulsetHandler{
		Log:   quietLogger(),
		Drain: &replicaDrainer{Registry: reg, logf: func(string, ...any) {}},
	}

	spec := statefulsetSpec{
		Drain:        &drainSpec{Timeout: "10m"},
		PluginBlocks: []nestedPluginBlock{{Type: "traffik", Spec: []byte(`{"port":5432}`)}},
	}

	if err := h.drainReplica(context.Background(), "data", "pg", spec, "0"); err != nil {
		t.Fatalf("drain: %v", err)
	}

	raw, err := os.ReadFile(seen)

	if err != nil {
		t.Fatalf("the plugin was never asked to drain: %v", err)
	}

	got := string(raw)

	for _, want := range []string{`"kind":"statefulset"`, `"name":"pg"`, `"replica_id":"0"`, `"timeout_ms":600000`} {
		if !strings.Contains(got, want) {
			t.Errorf("drain request missing %s\ngot: %s", want, got)
		}
	}
}

// TestStatefulsetHonoursDrainGrace — the SIGTERM budget matters most
// here. A database pod that gets ten seconds to flush is the original
// version of this problem.
func TestStatefulsetHonoursDrainGrace(t *testing.T) {
	fc := &fakeContainers{slots: map[string]*ContainerSlot{}, removeGraces: map[string]time.Duration{}}

	h := &StatefulsetHandler{Log: quietLogger(), Containers: fc}

	spec := statefulsetSpec{Drain: &drainSpec{Grace: "120s"}}

	if err := h.removeReplica(spec, "data-pg.0"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if got := fc.removeGraces["data-pg.0"]; got != 120*time.Second {
		t.Errorf("grace = %s, want 120s", got)
	}
}
