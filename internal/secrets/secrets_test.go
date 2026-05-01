package secrets

import (
	"os"
	"path/filepath"
	"testing"

	"go.voodu.clowk.in/internal/envfile"
	"go.voodu.clowk.in/internal/paths"
)

// withTempVooduRoot points VOODU_ROOT at a fresh tempdir for the
// duration of the test so the secrets functions write to a
// throwaway location instead of /opt/voodu.
func withTempVooduRoot(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	prev, had := os.LookupEnv(paths.EnvRoot)
	t.Setenv(paths.EnvRoot, dir)

	t.Cleanup(func() {
		if had {
			_ = os.Setenv(paths.EnvRoot, prev)
		} else {
			_ = os.Unsetenv(paths.EnvRoot)
		}
	})

	return dir
}

// TestSet_OverlaysOnTop pins the existing Set semantic that
// `vd config set FOO=bar` relies on: pre-existing keys are
// preserved, new ones are added, conflicting ones are updated.
// This is operator-facing behaviour, NOT what reconcilers need
// (see TestReplace).
func TestSet_OverlaysOnTop(t *testing.T) {
	withTempVooduRoot(t)

	app := "scope-app"

	// Seed the .env with one key.
	if _, err := Set(app, []string{"EXISTING=keep-me"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Set a different key — existing one must remain.
	if _, err := Set(app, []string{"NEW=hello"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	got, err := envfile.Load(paths.AppEnvFile(app))
	if err != nil {
		t.Fatal(err)
	}

	if got["EXISTING"] != "keep-me" {
		t.Errorf("existing key lost: %v", got)
	}

	if got["NEW"] != "hello" {
		t.Errorf("new key not added: %v", got)
	}
}

// TestReplace_SetsCompleteState pins the bug-fix contract:
// reconciler-driven writes via Replace produce a .env file that
// EXACTLY matches the pairs argument. Pre-existing keys absent
// from pairs are removed.
//
// Without this test, a regression that swaps Replace back to
// Set in WriteEnv would silently restore the linger-forever
// bug: `vd config unset` and `vd <plugin>:unlink` would stop
// removing keys from the .env file.
func TestReplace_SetsCompleteState(t *testing.T) {
	withTempVooduRoot(t)

	app := "scope-app"

	// Seed with two keys.
	if _, err := Set(app, []string{"OLD=stale", "ANOTHER=alsostale"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Replace with a different set — both old keys MUST go.
	if _, err := Replace(app, []string{"NEW=fresh"}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got, err := envfile.Load(paths.AppEnvFile(app))
	if err != nil {
		t.Fatal(err)
	}

	if _, exists := got["OLD"]; exists {
		t.Error("OLD should have been removed by Replace")
	}

	if _, exists := got["ANOTHER"]; exists {
		t.Error("ANOTHER should have been removed by Replace")
	}

	if got["NEW"] != "fresh" {
		t.Errorf("NEW not written: %v", got)
	}

	if len(got) != 1 {
		t.Errorf("expected exactly 1 key, got %d: %v", len(got), got)
	}
}

// TestReplace_EmptyPairsClearsFile: passing an empty pairs
// slice produces an empty .env file. This is the actual
// regression scenario from the unlink bug — config bucket
// emptied to zero keys, reconciler called writeEnv with no
// pairs, and the .env file should reflect "nothing".
func TestReplace_EmptyPairsClearsFile(t *testing.T) {
	withTempVooduRoot(t)

	app := "scope-app"

	if _, err := Set(app, []string{"REDIS_URL=redis://x"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Replace(app, nil); err != nil {
		t.Fatalf("replace empty: %v", err)
	}

	got, err := envfile.Load(paths.AppEnvFile(app))
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 0 {
		t.Errorf("expected empty .env, got: %v", got)
	}

	// The file itself should still exist (so docker run --env-file
	// doesn't fail on missing file). It's just zero bytes / empty.
	envFile := paths.AppEnvFile(app)

	info, err := os.Stat(envFile)
	if err != nil {
		t.Errorf("env file should exist: %v", err)
	} else if info.Size() != 0 {
		raw, _ := os.ReadFile(envFile)
		t.Errorf("env file should be empty, got %d bytes:\n%s", info.Size(), raw)
	}
}

// TestReplace_RejectsMalformed: a malformed pair (no `=`)
// surfaces as an error instead of silently dropping the key.
// Plugin authors who emit garbage actions get loud feedback.
func TestReplace_RejectsMalformed(t *testing.T) {
	withTempVooduRoot(t)

	_, err := Replace("scope-app", []string{"OK=fine", "MALFORMED"})
	if err == nil {
		t.Fatal("expected error on malformed pair")
	}
}

var _ = filepath.Join
