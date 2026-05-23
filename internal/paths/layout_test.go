package paths

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestEnsureAppLayout_InheritsRootOwner verifies that every dir
// created under VOODU_ROOT inherits the root's uid/gid. The
// controller runs as root via systemd; receive-pack runs over SSH
// as the operator. Without ownership propagation, whichever code
// path materialises the app first wins, and the other gets stuck
// with permission denied on subsequent mkdirs.
func TestEnsureAppLayout_InheritsRootOwner(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvRoot, root)

	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}

	rootSys := rootInfo.Sys().(*syscall.Stat_t)
	wantUID, wantGID := rootSys.Uid, rootSys.Gid

	if err := EnsureAppLayout("clowk-vd-docs"); err != nil {
		t.Fatalf("EnsureAppLayout: %v", err)
	}

	created := []string{
		AppDir("clowk-vd-docs"),
		AppReleasesDir("clowk-vd-docs"),
		AppSharedDir("clowk-vd-docs"),
		AppVolumeDir("clowk-vd-docs"),
	}

	for _, d := range created {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatalf("stat %s: %v", d, err)
		}

		sys := info.Sys().(*syscall.Stat_t)
		if sys.Uid != wantUID || sys.Gid != wantGID {
			t.Errorf("%s: uid/gid = %d/%d, want %d/%d", d, sys.Uid, sys.Gid, wantUID, wantGID)
		}
	}
}

// TestEnsureAppLayout_Idempotent verifies repeated calls don't
// error and don't change ownership of already-created dirs.
func TestEnsureAppLayout_Idempotent(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvRoot, root)

	for i := 0; i < 3; i++ {
		if err := EnsureAppLayout("idempotent-app"); err != nil {
			t.Fatalf("EnsureAppLayout iter %d: %v", i, err)
		}
	}

	if _, err := os.Stat(filepath.Join(AppDir("idempotent-app"), "releases")); err != nil {
		t.Fatalf("releases dir missing after 3 calls: %v", err)
	}
}

// TestEnsureAppLayout_MissingRoot exercises the defensive path:
// if VOODU_ROOT doesn't exist yet, EnsureAppLayout should still
// create the app tree (falling back to mkdir-default ownership)
// rather than refusing to run.
func TestEnsureAppLayout_MissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist-yet")
	t.Setenv(EnvRoot, missing)

	if err := EnsureAppLayout("fresh-app"); err != nil {
		t.Fatalf("EnsureAppLayout with missing root: %v", err)
	}

	if _, err := os.Stat(AppDir("fresh-app")); err != nil {
		t.Fatalf("app dir not created: %v", err)
	}
}
