// Tests for buildRunArgs. We don't shell out to `docker run` —
// the args slice is the entire surface this layer produces, so
// asserting on it pins the wire contract without needing the
// docker daemon. Production CreateContainer wraps this and
// passes the slice to exec.Command verbatim.

package docker

import (
	"strings"
	"testing"
)

// TestCreateContainer_EmitsLogOpts pins the json-file driver
// `--log-opt` emission: when both LogMaxSize and LogMaxFiles
// are populated, exactly two flag pairs land in the args slice
// — max-size and max-file. Voodu's controller layer always
// populates both before reaching here (with platform defaults
// 10m/3), so this is the steady-state production path.
//
// Why both fields must be set together: docker's json-file
// driver accepts either flag independently, but a partial spec
// (just max-size, no max-file) leaves the OTHER value at the
// daemon default — silent inconsistency between operator intent
// and runtime behaviour. buildRunArgs gates on both fields to
// keep the contract operator-readable: "I declared the block;
// the runtime honours the block."
func TestCreateContainer_EmitsLogOpts(t *testing.T) {
	args := buildRunArgs(ContainerConfig{
		Name:        "test",
		Image:       "nginx:1.27",
		LogMaxSize:  "10m",
		LogMaxFiles: 3,
	})

	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--log-opt max-size=10m") {
		t.Errorf("expected --log-opt max-size=10m in args, got: %s", joined)
	}

	if !strings.Contains(joined, "--log-opt max-file=3") {
		t.Errorf("expected --log-opt max-file=3 in args, got: %s", joined)
	}

	// Pin the exact flag pair count: two `--log-opt` entries,
	// no more, no fewer. Catches a regression where someone
	// adds a third option (e.g. labels) without updating the
	// gating contract.
	if got := strings.Count(joined, "--log-opt"); got != 2 {
		t.Errorf("expected 2 --log-opt flags, got %d in: %s", got, joined)
	}
}

// TestCreateContainer_OmitsLogOptsWhenZero pins the inverse:
// when either field is missing the entire `--log-opt` pair is
// skipped, so the container inherits docker's daemon default.
// This is the legacy / pre-M6 path — older specs without a
// LogsSpec land here.
func TestCreateContainer_OmitsLogOptsWhenZero(t *testing.T) {
	cases := []struct {
		name string
		cfg  ContainerConfig
	}{
		{
			name: "both empty",
			cfg:  ContainerConfig{Name: "x", Image: "nginx"},
		},
		{
			name: "size only",
			cfg:  ContainerConfig{Name: "x", Image: "nginx", LogMaxSize: "10m"},
		},
		{
			name: "files only",
			cfg:  ContainerConfig{Name: "x", Image: "nginx", LogMaxFiles: 3},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := buildRunArgs(tc.cfg)
			joined := strings.Join(args, " ")

			if strings.Contains(joined, "--log-opt") {
				t.Errorf("expected no --log-opt, got: %s", joined)
			}
		})
	}
}
