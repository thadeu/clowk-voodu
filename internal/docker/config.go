package docker

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

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

// DockerGroup is the unix group whose members can already talk to the
// docker socket. Sharing the credential file with it grants nothing
// new: anyone who can reach the socket can `docker run -v /:/host
// --privileged` and read any byte on the host, which is why Docker's
// own documentation calls the group root-equivalent. What it buys is
// the split-user case — the controller runs as root under systemd
// while `voodu receive-pack` runs as the SSH remote's ordinary
// (sudo-capable, docker-group) user, and both need the same auths.
const DockerGroup = "docker"

// shareWithDockerGroup relaxes path to <owner>:docker with mode so a
// non-root docker-group process can read it. Best-effort by design:
// chown requires privilege, and this same function runs from
// receive-pack as an unprivileged user where the correct outcome is
// "leave it exactly as the controller set it".
//
// Absent docker group (a host using rootless docker, or a socket
// guarded some other way) is likewise not an error — the file stays
// owner-only, which is the safe direction to fail.
func shareWithDockerGroup(path string, mode os.FileMode) {
	if grp, err := user.LookupGroup(DockerGroup); err == nil {
		if gid, cerr := strconv.Atoi(grp.Gid); cerr == nil {
			_ = os.Chown(path, -1, gid)
		}
	}

	// Chmod runs even when the group does not resolve, and AFTER any
	// chown (which can clear setuid/setgid bits). It is not optional
	// bookkeeping: MkdirAll and WriteFile both apply the process
	// umask, so a directory requested as 0770 lands as 0750 under the
	// common umask 022 — silently dropping the group-write bit the
	// unprivileged build user needs to create its own cache dir. An
	// explicit chmod is the only way to get the mode actually asked
	// for.
	_ = os.Chmod(path, mode)
}

// ShareConfigWithDockerGroup applies the group-readable posture to a
// file the controller just wrote. Exported for RegistryHandler, whose
// atomic write (CreateTemp + rename) lands a 0600 file that the
// build-side user could not otherwise read.
func ShareConfigWithDockerGroup(path string) {
	shareWithDockerGroup(path, 0640)
}

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

	// 0750 root:docker. The directory holds registry credentials in
	// the clear (docker's `auth` field is base64, not encryption), so
	// it stays off-limits to the host at large — but the docker group
	// is not "the host at large": its members can already read every
	// byte on the machine through the socket. Excluding them buys no
	// security and breaks every build-mode deploy, because
	// receive-pack runs as that user.
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", false, fmt.Errorf("ensure docker config dir %s: %w", dir, err)
	}

	shareWithDockerGroup(dir, 0750)

	// Adopt the directory ONLY if this process can actually read what
	// is in it. The controller creates it 0700 as root; `voodu
	// receive-pack` runs as whatever user the SSH remote is
	// configured with, and pointing a non-root build at a root-owned
	// credential file does not degrade — it breaks the build outright.
	// Docker fails to load the config, which takes its CLI-plugin
	// discovery with it, and since `build` is a plugin-provided
	// command on modern Docker the operator gets
	//
	//	unknown shorthand flag: 'f' in -f
	//
	// naming a flag that was never the problem. Leaving DOCKER_CONFIG
	// unset falls back to the docker default, which is exactly the
	// behaviour that worked before this variable existed.
	if err := readable(dir); err != nil {
		return "", false, err
	}

	if err := os.Setenv(EnvDockerConfig, dir); err != nil {
		return "", false, fmt.Errorf("set %s: %w", EnvDockerConfig, err)
	}

	// Credential helpers are exec'd by the docker CLI, which inherits
	// this environment — so a helper that caches under $HOME by default
	// needs redirecting the same way the config file did.
	//
	// The cache is per-uid. Two processes share this tree (root
	// controller, docker-group build user) and the helper writes its
	// cache 0600, so a single shared directory would leave whichever
	// process wrote second unable to read the other's file. Separate
	// subdirectories cost one extra token fetch and remove the failure
	// mode entirely.
	//
	// Best-effort throughout: the ECR helper degrades to an uncached
	// (slower, still working) fetch when the directory is missing, and
	// every other helper ignores the variable outright.
	shared := filepath.Join(dir, "ecr-cache")

	// 0770 so the unprivileged build user can create its own subdir.
	if err := os.MkdirAll(shared, 0770); err == nil {
		shareWithDockerGroup(shared, 0770)

		mine := filepath.Join(shared, strconv.Itoa(os.Geteuid()))
		if err := os.MkdirAll(mine, 0700); err == nil {
			_ = os.Setenv(EnvECRCacheDir, mine)
		}
	}

	seeded = seedFromHome(dir)

	return dir, seeded, nil
}

// readable reports whether this process can open the config file in
// dir. A missing file is fine — nothing has written one yet, and
// docker treats an absent config as "no credentials", the same as the
// default path would.
//
// The probe opens the FILE rather than stat-ing the directory: a
// 0700 directory owned by another user denies traversal, so the open
// fails with EACCES either way, and one syscall covers both the
// unreadable-directory and unreadable-file cases.
func readable(dir string) error {
	f, err := os.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("cannot read %s as uid %d: %w", filepath.Join(dir, "config.json"), os.Geteuid(), err)
	}

	return f.Close()
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

	// 0640 root:docker, matching what RegistryHandler writes on every
	// subsequent reconcile — see the directory's rationale above.
	if err := os.WriteFile(dst, body, 0640); err != nil {
		return false
	}

	ShareConfigWithDockerGroup(dst)

	return true
}
