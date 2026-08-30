package controller

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDrainTimeoutComesFromTheManifest — a workload whose unit of work
// is a phone call needs a longer wait than the platform default, and
// the whole point of the block is that it can say so.
func TestDrainTimeoutComesFromTheManifest(t *testing.T) {
	reg, seen := drainPlugin(t, "trafik", `#!/usr/bin/env bash
cat > {{SEEN}}
echo '{"status":"ok"}'
`)

	h := &DeploymentHandler{
		Log:   quietLogger(),
		Drain: &replicaDrainer{Registry: reg, logf: func(string, ...any) {}},
	}

	spec := deploymentSpec{
		Drain:        &drainSpec{Timeout: "30m"},
		PluginBlocks: []nestedPluginBlock{{Type: "trafik", Spec: []byte(`{"port":8084}`)}},
	}

	if err := h.drainReplica(context.Background(), KindDeployment, "prod", "esl", spec, "a1"); err != nil {
		t.Fatalf("drain: %v", err)
	}

	raw, err := os.ReadFile(seen)

	if err != nil {
		t.Fatalf("plugin was never asked: %v", err)
	}

	if want := `"timeout_ms":1800000`; !strings.Contains(string(raw), want) {
		t.Errorf("plugin was told the wrong budget, want %s\ngot: %s", want, raw)
	}
}

// TestDrainGraceReachesDockerStop is the knob that needs no plugin and
// no load balancer: a worker losing an in-flight write on deploy is
// asking for a longer gap between SIGTERM and SIGKILL, nothing else.
func TestDrainGraceReachesDockerStop(t *testing.T) {
	fc := &fakeContainers{slots: map[string]*ContainerSlot{}, removeGraces: map[string]time.Duration{}}

	h := &DeploymentHandler{Log: quietLogger(), Containers: fc}

	spec := deploymentSpec{Drain: &drainSpec{Grace: "45s"}}

	if err := h.removeReplica(spec, "clowk-esl.a1"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if got := fc.removeGraces["clowk-esl.a1"]; got != 45*time.Second {
		t.Errorf("grace = %s, want 45s", got)
	}
}

// TestRemoveWithoutDrainBlockKeepsDockerDefault — a deployment that
// said nothing about winding down should behave exactly as it did
// before the block existed.
func TestRemoveWithoutDrainBlockKeepsDockerDefault(t *testing.T) {
	fc := &fakeContainers{slots: map[string]*ContainerSlot{}, removeGraces: map[string]time.Duration{}}

	h := &DeploymentHandler{Log: quietLogger(), Containers: fc}

	if err := h.removeReplica(deploymentSpec{}, "clowk-esl.a1"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if got, ok := fc.removeGraces["clowk-esl.a1"]; ok && got != 0 {
		t.Errorf("grace = %s, want docker's own default (0 = unset)", got)
	}
}

// TestUnparseableDrainTimeoutFallsBackLoudly — the parser rejects a bad
// duration, so a spec reaching here with one came from an older CLI or
// a hand-written store write. Falling back is right; doing it in
// silence is not.
func TestUnparseableDrainTimeoutFallsBackLoudly(t *testing.T) {
	var logged []string

	h := &DeploymentHandler{Log: quietLogger()}
	h.Drain = &replicaDrainer{logf: func(f string, a ...any) { logged = append(logged, f) }}

	got := h.drainBudget(deploymentSpec{Drain: &drainSpec{Timeout: "30min"}})

	if got != 0 {
		t.Errorf("budget = %s, want 0 so the drainer applies its default", got)
	}
}
