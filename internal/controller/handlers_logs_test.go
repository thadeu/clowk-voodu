// Tests for the logs cap folding into deploymentSpecHash. Docker
// freezes log driver options at container-create time, so the
// reconciler MUST recreate the pod when the cap changes —
// otherwise the operator edits 10m → 100m and the running
// container quietly keeps the old budget.
//
// The spec-hash mix-in is the only signal that triggers
// recreate; if Logs falls out of the hash, the rolling restart
// never fires.

package controller

import (
	"testing"
)

// TestDeploymentSpecHash_LogsChangeFlipsHash pins that a change
// to the logs cap moves the hash. The contract is "any field
// docker freezes at create time MUST be in the hash"; falling
// out of compliance is a silent-drift bug.
func TestDeploymentSpecHash_LogsChangeFlipsHash(t *testing.T) {
	base := deploymentSpec{
		Image: "nginx:1.27",
		Logs:  &logsWireSpec{MaxSize: "10m", MaxFiles: 3},
	}

	bumped := deploymentSpec{
		Image: "nginx:1.27",
		Logs:  &logsWireSpec{MaxSize: "100m", MaxFiles: 3},
	}

	baseHash := deploymentSpecHash(base, nil)
	bumpedHash := deploymentSpecHash(bumped, nil)

	if baseHash == bumpedHash {
		t.Errorf("hash did not flip on logs change: both %s — Logs is missing from the hash mix-in", baseHash)
	}

	// Sanity check the inverse: identical Logs hashes identical.
	dup := deploymentSpec{
		Image: "nginx:1.27",
		Logs:  &logsWireSpec{MaxSize: "10m", MaxFiles: 3},
	}

	if deploymentSpecHash(dup, nil) != baseHash {
		t.Errorf("hash unstable across identical specs — Logs mix-in is non-deterministic")
	}
}

// TestDeploymentSpecHash_OnDeployDoesNotFlipHash pins the inverse:
// changing the webhook URL must NOT churn replicas. The spec
// intentionally excludes OnDeploy from the hash — operators must
// be able to edit notification routes without triggering a
// rolling restart of every running pod.
func TestDeploymentSpecHash_OnDeployDoesNotFlipHash(t *testing.T) {
	base := deploymentSpec{
		Image:    "nginx:1.27",
		OnDeploy: &onDeployWireSpec{Success: "https://example/a"},
	}

	other := deploymentSpec{
		Image:    "nginx:1.27",
		OnDeploy: &onDeployWireSpec{Success: "https://example/b"},
	}

	if deploymentSpecHash(base, nil) != deploymentSpecHash(other, nil) {
		t.Errorf("hash flipped on OnDeploy URL change — operators editing webhook URLs would unintentionally churn replicas")
	}
}

// TestEffectiveLogs_FallbackOnNilSpec pins the legacy-spec
// safety net. A deployment persisted by a pre-M6 controller
// has no Logs field in its JSON; we must NOT pass through
// docker's unbounded default.
func TestEffectiveLogs_FallbackOnNilSpec(t *testing.T) {
	size, files := effectiveLogs(nil)

	if size != fallbackLogsMaxSize {
		t.Errorf("nil spec size: got %q, want %q", size, fallbackLogsMaxSize)
	}

	if files != fallbackLogsMaxFiles {
		t.Errorf("nil spec files: got %d, want %d", files, fallbackLogsMaxFiles)
	}
}

// TestEffectiveLogs_RespectsOperatorValues pins that
// effectiveLogs does NOT clobber operator declarations.
func TestEffectiveLogs_RespectsOperatorValues(t *testing.T) {
	size, files := effectiveLogs(&logsWireSpec{MaxSize: "50m", MaxFiles: 7})

	if size != "50m" {
		t.Errorf("size: got %q, want 50m", size)
	}

	if files != 7 {
		t.Errorf("files: got %d, want 7", files)
	}
}
