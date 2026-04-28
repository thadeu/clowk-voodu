package paths

import (
	"fmt"
	"os"
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
func EnsureAppLayout(app string) error {
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
	}

	return nil
}
