package deploy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/paths"
)

// MaterializeFromBuilt lands a release for `app` that REUSES an image
// already built for `sourceApp` from the SAME source tarball, instead of
// rebuilding. It is the "build-once" half of the Procfile fan-out: the
// first process goes through RunFromTarball (a full build), and every
// other process — which shares one source tree and therefore one runtime
// image — calls this to get its own tags by retagging sourceApp's image.
//
// SCOPE: Procfile-only (cmd/cli/receive_pack_procfile.go). The HCL
// build-mode path (receive-pack --spec) never calls this — it still
// builds one image per deployment via RunFromTarball, unchanged. This
// function is additive: it does not touch RunFromTarball or runDockerBuild.
//
// It reproduces every observable side effect of a real RunFromTarball
// build EXCEPT the `docker build` itself, so nothing downstream notices:
//
//   - <app>:latest           → reconciler resolves spec.Image and starts
//     the container (handlers.go applyDeploymentSpecDefaults)
//   - <app>:<buildID>         → vd rollback's fast-path retag finds it
//     (release.go retagToTargetBuild → ImageExists)
//   - releases/<buildID> dir  → rollback rebuild-fallback source + the
//   - `current` symlink         basename `current` points at is how
//     currentBuildID() learns the live buildID
//   - identical buildID        → same tarball ⇒ same sha256, so all
//     processes agree on the release id
//
// The only thing skipped is handler.Build (deps install / asset compile),
// the expensive step that would otherwise re-run once per process — even
// when BuildKit-cached, it is a full Dockerfile evaluation per process.
//
// Intentionally silent: the reuse is an implementation detail, so it
// emits no progress step. The primary's build narrative shows once, and
// each reused process still surfaces via its `... applied` result line
// (reporter.Result in runProcfileReceive). Errors propagate to the caller.
func MaterializeFromBuilt(app string, src io.Reader, sourceApp string, opts Options) error {
	if app == "" {
		return fmt.Errorf("app name is required")
	}

	if sourceApp == "" {
		return fmt.Errorf("source app name is required")
	}

	if src == nil {
		return fmt.Errorf("tarball source is nil")
	}

	if err := paths.EnsureAppLayout(app); err != nil {
		return err
	}

	// Re-hash the (reopened) tarball so the release id matches the one the
	// primary build produced — same bytes ⇒ same buildID. Mirrors what
	// RunFromTarball does per call today, so the I/O cost is unchanged;
	// only the build is skipped.
	buildID, tmpPath, err := bufferTarball(src)
	if err != nil {
		return fmt.Errorf("buffer tarball: %w", err)
	}

	defer os.Remove(tmpPath)

	releaseDir := filepath.Join(paths.AppReleasesDir(app), buildID)

	// Dedup: the same tarball already landed for this app → just repoint
	// `current`, same as RunFromTarball's skip-rebuild branch.
	if existing, statErr := os.Stat(releaseDir); statErr == nil && existing.IsDir() && !opts.Force {
		return swapCurrentSymlink(app, releaseDir)
	}

	if err := os.MkdirAll(releaseDir, 0755); err != nil {
		return fmt.Errorf("create release dir: %w", err)
	}

	if err := extractTarball(tmpPath, releaseDir); err != nil {
		_ = os.RemoveAll(releaseDir)

		return fmt.Errorf("extract tarball: %w", err)
	}

	// Retag the already-built image under THIS process's identity. Both
	// tags alias the exact image content sourceApp produced.
	srcImmutable := fmt.Sprintf("%s:%s", sourceApp, buildID)

	if !docker.ImageExists(srcImmutable) {
		return fmt.Errorf("source image %s not found — build-once expects %q to have been built first", srcImmutable, sourceApp)
	}

	for _, dst := range []string{
		fmt.Sprintf("%s:latest", app),
		fmt.Sprintf("%s:%s", app, buildID),
	} {
		if err := docker.TagImage(srcImmutable, dst); err != nil {
			return err
		}
	}

	if err := swapCurrentSymlink(app, releaseDir); err != nil {
		return err
	}

	// Per-app GC, identical to RunFromTarball — each process owns its own
	// releases dir, so this prunes only this app's history. GC failure is
	// non-fatal (matches RunFromTarball); pruned releases are silent here
	// since the reuse itself is.
	keep := DefaultKeepReleases
	if opts.Spec != nil && opts.Spec.KeepReleases > 0 {
		keep = opts.Spec.KeepReleases
	}

	if _, gcErr := gcReleases(app, keep); gcErr != nil {
		opts.warn("Warning: release GC failed: %v", gcErr)
	}

	return nil
}
