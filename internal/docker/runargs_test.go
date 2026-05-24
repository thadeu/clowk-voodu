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

// TestCreateContainer_DefaultUlimits pins the platform-default
// ulimit table: a config with no Ulimits set still emits the
// historical baseline (nofile=65536:65536, nproc=4096:4096). This
// matters because the values shape kernel limits inherited by
// every long-running voodu container, and a silent regression
// would surface as production fd exhaustion / fork failures.
func TestCreateContainer_DefaultUlimits(t *testing.T) {
	args := buildRunArgs(ContainerConfig{Name: "x", Image: "nginx"})
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--ulimit nofile=65536:65536") {
		t.Errorf("expected default nofile ulimit, got: %s", joined)
	}

	if !strings.Contains(joined, "--ulimit nproc=4096:4096") {
		t.Errorf("expected default nproc ulimit, got: %s", joined)
	}
}

// TestCreateContainer_UlimitsOverride pins per-key override
// semantics: an operator-declared `ulimits = { nofile = "..." }`
// REPLACES that key in the default table but leaves other defaults
// (nproc) intact, and a brand-new key (memlock) lands as an
// additional --ulimit flag.
func TestCreateContainer_UlimitsOverride(t *testing.T) {
	args := buildRunArgs(ContainerConfig{
		Name:  "x",
		Image: "nginx",
		Ulimits: map[string]string{
			"nofile":  "1048576:1048576",
			"memlock": "-1",
		},
	})
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--ulimit nofile=1048576:1048576") {
		t.Errorf("expected overridden nofile, got: %s", joined)
	}

	if !strings.Contains(joined, "--ulimit nproc=4096:4096") {
		t.Errorf("expected default nproc to survive override, got: %s", joined)
	}

	if !strings.Contains(joined, "--ulimit memlock=-1") {
		t.Errorf("expected new memlock ulimit, got: %s", joined)
	}

	if strings.Contains(joined, "--ulimit nofile=65536:65536") {
		t.Errorf("expected default nofile to be replaced, got: %s", joined)
	}
}

// TestCreateContainer_DockerOptions pins the raw-bypass surface:
// every DockerOptions entry lands in argv verbatim, in declared
// order, between the managed flags and the image. Empty strings
// are filtered (defensive — operators may build the slice via
// conditional expressions).
func TestCreateContainer_DockerOptions(t *testing.T) {
	args := buildRunArgs(ContainerConfig{
		Name:  "x",
		Image: "nginx",
		DockerOptions: []string{
			"--shm-size=64m",
			"",
			"--sysctl=net.core.somaxconn=4096",
			"--pids-limit=1000",
		},
	})

	// Locate the image — operator flags must come immediately before.
	imageIdx := -1
	for i, a := range args {
		if a == "nginx" {
			imageIdx = i
			break
		}
	}

	if imageIdx == -1 {
		t.Fatalf("image not found in args: %v", args)
	}

	want := []string{
		"--shm-size=64m",
		"--sysctl=net.core.somaxconn=4096",
		"--pids-limit=1000",
	}

	got := args[imageIdx-len(want) : imageIdx]
	for i, w := range want {
		if got[i] != w {
			t.Errorf("docker option %d: want %q got %q (full args: %v)", i, w, got[i], args)
		}
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
