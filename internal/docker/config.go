package docker

import (
	"fmt"
	"os"
	"path/filepath"

	"go.voodu.clowk.in/internal/paths"
)

// EnvDockerConfig is the docker CLI's own knob for "read config.json
// from here instead of ~/.docker". Pointing it at a voodu-owned
// directory is what lets a sandboxed controller and the docker
// processes it forks agree on one credential file.
const EnvDockerConfig = "DOCKER_CONFIG"

// EnvECRCacheDir is the Amazon ECR credential helper's override for
// where it caches issued tokens. Its default is ${HOME}/.ecr, which
// ProtectHome=yes makes unwritable for the controller — so a helper
// that would otherwise work off the EC2 instance role fails on its
// cache write instead of on anything to do with AWS.
//
// Pointed at a subdirectory of the docker config dir for the same
// reason DOCKER_CONFIG is: VOODU_ROOT is the one tree the unit grants
// write access to.
const EnvECRCacheDir = "AWS_ECR_CACHE_DIR"

// UseVooduDockerConfig points this process — and therefore every
// `docker` child it forks — at <VOODU_ROOT>/docker/config.json.
//
// Why this exists: the controller runs as root under a hardened
// systemd unit (`ProtectHome=yes`, `ProtectSystem=strict`). /root is
// empty and unwritable for the whole service cgroup, so the
// conventional ~/.docker/config.json is both unwritable by
// RegistryHandler and unreadable by the `docker pull` it is meant to
// authenticate. The symptom is the one operators actually hit:
//
//	pull access denied ... authorization failed: no basic auth credentials
//
// even though `docker login` succeeded for some interactive user.
//
// Setting the env var (rather than threading a path through every
// exec.Command) is deliberate: os.Environ() is inherited by every
// child, so plugin lifecycle hooks and `docker build`'s implicit base
// image pull pick it up without each call site opting in.
//
// First call seeds the file from $HOME/.docker/config.json when the
// target does not exist yet and the source is readable. That keeps
// hosts that predate this change working: an operator who ran
// `docker login` as root before upgrading does not lose those
// credentials the moment the lookup path moves. On a sandboxed host
// the source is unreadable and the seed is silently skipped — there
// was nothing usable there anyway.
//
// Returns the directory now in effect. Errors are returned rather
// than fatal: a controller that cannot prepare the directory should
// still boot and reconcile everything that does not need a private
// registry.
func UseVooduDockerConfig() (dir string, seeded bool, err error) {
	dir = paths.DockerConfigDir()

	// 0700: this directory holds registry credentials in the clear
	// (docker's `auth` field is base64, not encryption). Anything
	// looser would widen the blast radius of a compromised
	// unprivileged account on the host.
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", false, fmt.Errorf("ensure docker config dir %s: %w", dir, err)
	}

	if err := os.Setenv(EnvDockerConfig, dir); err != nil {
		return "", false, fmt.Errorf("set %s: %w", EnvDockerConfig, err)
	}

	// Credential helpers are exec'd by the docker CLI, which inherits
	// this environment — so a helper that caches under $HOME by default
	// needs redirecting the same way the config file did. Best-effort:
	// the ECR helper degrades to an uncached (slower, but working) token
	// fetch if the directory is missing, and every other helper ignores
	// the variable outright.
	if err := os.MkdirAll(filepath.Join(dir, "ecr-cache"), 0700); err == nil {
		_ = os.Setenv(EnvECRCacheDir, filepath.Join(dir, "ecr-cache"))
	}

	seeded = seedFromHome(dir)

	return dir, seeded, nil
}

// seedFromHome copies ~/.docker/config.json into dir the first time,
// so pre-existing `docker login` credentials survive the move. Every
// failure path is a no-op: a missing HOME, an unreadable source (the
// sandboxed case), or an already-populated destination all mean
// "nothing to carry over".
//
// Reports whether a copy actually happened, so the caller can say so
// once in the boot log instead of leaving operators to guess where
// their credentials went.
func seedFromHome(dir string) bool {
	dst := filepath.Join(dir, "config.json")

	if _, err := os.Stat(dst); err == nil {
		return false
	}

	home := os.Getenv("HOME")
	if home == "" {
		return false
	}

	src := filepath.Join(home, ".docker", "config.json")

	body, err := os.ReadFile(src)
	if err != nil {
		return false
	}

	// 0600 for the same reason the directory is 0700 — and it matches
	// what RegistryHandler writes on every subsequent reconcile.
	if err := os.WriteFile(dst, body, 0600); err != nil {
		return false
	}

	return true
}
