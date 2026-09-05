package paths

import (
	"os"
	"strings"
	"testing"
)

// Scratch belongs under the platform root, temporary or not.
//
// A box running the controller under a hardened unit — `ProtectSystem=strict`,
// `PrivateTmp=`, a read-only rootfs — has no writable /tmp, and a deploy that
// buffered or unpacked there failed with "read-only file system" for a reason
// that had nothing to do with the deploy. Twice, in two different places,
// which is why the answer is one function rather than a fix per call site.
func TestBuildsDirLivesUnderThePlatformRoot(t *testing.T) {
	t.Setenv(EnvRoot, "/opt/voodu")

	if got := BuildsDir(); got != "/opt/voodu/builds" {
		t.Errorf("BuildsDir = %q, want it under the platform root", got)
	}
}

// It follows VOODU_ROOT like every other path here — a box that moved the
// tree moved its scratch with it.
func TestBuildsDirFollowsTheConfiguredRoot(t *testing.T) {
	t.Setenv(EnvRoot, "/srv/voodu")

	if got := BuildsDir(); got != "/srv/voodu/builds" {
		t.Errorf("BuildsDir = %q, want it to follow VOODU_ROOT", got)
	}
}

// The point of the whole change, stated as an assertion: never the system
// temp dir.
func TestBuildsDirIsNeverTheSystemTempDir(t *testing.T) {
	t.Setenv(EnvRoot, "/opt/voodu")

	if strings.HasPrefix(BuildsDir(), os.TempDir()) {
		t.Errorf("BuildsDir = %q, which is under the system temp dir", BuildsDir())
	}
}
