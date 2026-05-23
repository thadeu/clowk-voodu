package paths

import (
	"fmt"
	"os"
	"syscall"
)

// EnsureAppLayout creates the per-app filesystem tree the runtime
// expects under VOODU_ROOT:
//
//	apps/<app>/
//	apps/<app>/releases/
//	apps/<app>/shared/
//	volumes/<app>/
//
// Idempotent — re-applies and re-deploys pay only the stat syscalls
// for already-existing dirs. Called by every code path that
// materialises an app on disk so build-mode (receive-pack) and
// image-mode (controller reconcile) end up with the same shape and
// the same voodu-owned permissions:
//
//   - receive-pack invokes it before unpacking the tarball, so the
//     release dir is ready to receive the new build.
//   - secrets.Set invokes it on every env-file write (which fires
//     on first apply, every config-set, every reconcile cycle), so
//     image-mode deployments pre-create their volumes/<app>/ dir
//     before docker would otherwise materialise it as root:root.
//
// Without the secrets.Set hook, image-mode apps that declared
// `volumes = [...]` in HCL would let docker create the host path
// at container-start with daemon-default ownership. Apps inside
// the container that try to write to the volume would then trip
// "permission denied" until the operator manually chowned.
//
// Ownership: every created dir inherits the uid/gid of VOODU_ROOT
// (set by the install script to the operator user). The controller
// runs as root via systemd, so without this, dirs it materialises
// would land as root:root and block the unprivileged SSH user
// (receive-pack runs over SSH as the operator) from creating new
// release dirs underneath. Inheriting from Root() keeps both code
// paths converging on the same owner regardless of who runs first.
func EnsureAppLayout(app string) error {
	uid, gid, hasOwner := rootOwner()

	dirs := []string{
		AppDir(app),
		AppReleasesDir(app),
		AppSharedDir(app),
		AppVolumeDir(app),
	}

	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}

		if hasOwner {
			if err := os.Chown(d, uid, gid); err != nil && !os.IsPermission(err) {
				return fmt.Errorf("chown %s: %w", d, err)
			}
		}
	}

	return nil
}

// rootOwner returns the uid/gid of VOODU_ROOT so EnsureAppLayout can
// propagate it down. Returns hasOwner=false if Root() doesn't exist
// yet or the stat info isn't a *syscall.Stat_t (non-unix), in which
// case the caller skips chown and falls back to mkdir-default
// ownership.
func rootOwner() (uid, gid int, hasOwner bool) {
	info, err := os.Stat(Root())
	if err != nil {
		return 0, 0, false
	}

	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}

	return int(sys.Uid), int(sys.Gid), true
}
