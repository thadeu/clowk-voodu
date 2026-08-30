package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.voodu.clowk.in/pkg/plugin"
)

// TestRunHonoursTimeoutWhenPluginSpawnsAChild pins the budget the
// controller relies on.
//
// exec.CommandContext kills the process it started — the shell — but a
// child it spawned keeps the inherited stdout pipe open, and Wait
// blocks on that pipe until the child exits. A plugin that shells out
// to anything therefore outlived its timeout entirely, which turns
// every "wait, but not forever" in the controller into "wait forever".
//
// The rolling restart's drain gate is the caller that cannot survive
// it: a deployment that cannot finish a roll is an outage of its own.
func TestRunHonoursTimeoutWhenPluginSpawnsAChild(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "slow")

	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	p := &LoadedPlugin{
		Manifest: plugin.Manifest{Name: "slow"},
		Dir:      dir,
		Commands: map[string]string{"slow": script},
	}

	start := time.Now()

	_, err := p.Run(context.Background(), RunOptions{Command: "slow", Timeout: 200 * time.Millisecond})

	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a command that outran its timeout should report it")
	}

	if elapsed > 5*time.Second {
		t.Fatalf("Run took %s for a 200ms timeout — the child kept it alive", elapsed)
	}
}
