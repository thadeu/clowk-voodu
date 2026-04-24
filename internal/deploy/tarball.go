package deploy

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.voodu.clowk.in/internal/paths"
)

// BuildIDLen is the number of hex characters kept from the tarball's
// sha256 to name a release. 12 chars = 48 bits = collision-safe for the
// build volume any single app will ever have, and short enough to keep
// release paths readable in logs and shell prompts.
const BuildIDLen = 12

// DefaultKeepReleases is how many past releases survive the post-deploy
// GC. 5 covers "last known good + 1-2 canaries + a rollback window"
// without letting /opt/voodu/apps/<app>/releases grow unbounded on a
// busy deploy loop. Will be overridable per-deployment via a future
// DeploymentSpec.KeepReleases field.
const DefaultKeepReleases = 5

// RunFromTarball ingests a gzipped tar stream as the source of a release
// and runs the build/swap/container pipeline against it. This is the
// only path into the pipeline — the CLI pipes a tar over SSH and we
// drive from here.
//
// The build-id is the content hash of the tarball (sha256, first
// BuildIDLen hex chars). If the resulting release dir already exists and
// opts.Force is false, the pipeline is skipped and only the `current`
// symlink is updated — same source produced the same image last time, a
// rebuild would be wasted cycles.
//
// On error the partial release directory is removed, leaving prior
// releases intact.
func RunFromTarball(app string, src io.Reader, opts Options) error {
	if app == "" {
		return fmt.Errorf("app name is required")
	}

	if src == nil {
		return fmt.Errorf("tarball source is nil")
	}

	opts.log("-----> Receiving build context for '%s'...", app)

	buildID, tmpPath, err := bufferTarball(src)
	if err != nil {
		return fmt.Errorf("buffer tarball: %w", err)
	}

	defer os.Remove(tmpPath)

	releaseDir := filepath.Join(paths.AppReleasesDir(app), buildID)

	if existing, err := os.Stat(releaseDir); err == nil && existing.IsDir() && !opts.Force {
		opts.log("-----> Release %s already exists — skipping rebuild (use --force to override)", buildID)

		if err := swapCurrentSymlink(app, releaseDir); err != nil {
			return fmt.Errorf("swap current symlink: %w", err)
		}

		return startContainers(app, releaseDir, &opts)
	}

	opts.log("-----> Creating release %s", buildID)

	if err := os.MkdirAll(releaseDir, 0755); err != nil {
		return fmt.Errorf("create release dir: %w", err)
	}

	if err := extractTarball(tmpPath, releaseDir); err != nil {
		_ = os.RemoveAll(releaseDir)
		return fmt.Errorf("extract tarball: %w", err)
	}

	if err := runPipeline(app, releaseDir, &opts); err != nil {
		// Leave the release dir in place — useful for forensics — but
		// don't repoint `current` (swap happens inside the pipeline
		// *before* this error if it got that far).
		return err
	}

	if pruned, err := gcReleases(app, DefaultKeepReleases); err != nil {
		// GC failure is non-fatal: the deploy succeeded, the user sees
		// a warning, disk cleanup can be done by hand later.
		opts.log("Warning: release GC failed: %v", err)
	} else if pruned > 0 {
		opts.log("-----> Pruned %d old release(s)", pruned)
	}

	return nil
}

// gcReleases removes the oldest releases in /opt/voodu/apps/<app>/releases
// so that at most `keep` directories remain. The `current` symlink
// target is always retained regardless of age — deleting it would
// break the running container. Returns the number of release dirs
// removed.
func gcReleases(app string, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}

	releasesDir := paths.AppReleasesDir(app)

	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return 0, err
	}

	// Resolve the current symlink once so we can skip it regardless of
	// where it falls in the sort order.
	currentTarget, _ := os.Readlink(paths.AppCurrentLink(app))
	currentBase := filepath.Base(currentTarget)

	type release struct {
		name string
		mod  int64
	}

	dirs := make([]release, 0, len(entries))

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		dirs = append(dirs, release{name: e.Name(), mod: info.ModTime().Unix()})
	}

	if len(dirs) <= keep {
		return 0, nil
	}

	// Newest first. Oldest beyond `keep` get pruned. ModTime is a
	// proxy for deploy order — a content-addressed build-id carries
	// no inherent timestamp.
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].mod > dirs[j].mod
	})

	pruned := 0

	for i, d := range dirs {
		if i < keep {
			continue
		}

		if d.name == currentBase {
			// Active release hidden below the cutoff (unusual but
			// possible if the operator rolled back to an older
			// release). Never delete it.
			continue
		}

		if err := os.RemoveAll(filepath.Join(releasesDir, d.name)); err != nil {
			return pruned, err
		}

		pruned++
	}

	return pruned, nil
}

// bufferTarball writes src to a temp file while computing the sha256 of
// its bytes. Returning the hash as the build-id AND the temp path lets
// the caller decide dedup-skip vs extract without re-reading the stream.
func bufferTarball(src io.Reader) (buildID, tmpPath string, err error) {
	f, err := os.CreateTemp("", "voodu-receive-*.tar.gz")
	if err != nil {
		return "", "", err
	}

	defer f.Close()

	h := sha256.New()

	if _, err := io.Copy(f, io.TeeReader(src, h)); err != nil {
		_ = os.Remove(f.Name())
		return "", "", fmt.Errorf("copy stream: %w", err)
	}

	return buildIDFromHash(h), f.Name(), nil
}

func buildIDFromHash(h hash.Hash) string {
	sum := h.Sum(nil)

	return hex.EncodeToString(sum)[:BuildIDLen]
}

// extractTarball unpacks the gzipped tar at path into dest. Refuses path
// traversal (../), symlinks escaping dest, and absolute paths. Preserves
// file mode bits to keep scripts executable.
func extractTarball(path, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}

	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}

	defer gz.Close()

	tr := tar.NewReader(gz)

	// The tar we receive from the CLI is the contents of the build
	// context directory (tar -C <path> .), so entries look like
	// "./Dockerfile", "./cmd/api/main.go". We resolve against dest and
	// reject anything that lands outside.
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}

		if err != nil {
			return fmt.Errorf("tar header: %w", err)
		}

		target, err := safeJoin(destAbs, hdr.Name)
		if err != nil {
			return fmt.Errorf("entry %q: %w", hdr.Name, err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fileMode(hdr.Mode, 0755)); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}

		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", target, err)
			}

			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode(hdr.Mode, 0644))
			if err != nil {
				return fmt.Errorf("open %s: %w", target, err)
			}

			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}

			out.Close()

		case tar.TypeSymlink:
			// Refuse symlinks that point outside dest. Safest posture:
			// reject absolute targets and any ../ escape after resolution.
			linkTarget := hdr.Linkname

			if filepath.IsAbs(linkTarget) {
				return fmt.Errorf("refuse absolute symlink %q → %q", hdr.Name, linkTarget)
			}

			resolved := filepath.Clean(filepath.Join(filepath.Dir(target), linkTarget))

			if !strings.HasPrefix(resolved, destAbs+string(os.PathSeparator)) && resolved != destAbs {
				return fmt.Errorf("refuse escaping symlink %q → %q", hdr.Name, linkTarget)
			}

			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}

			_ = os.Remove(target)

			if err := os.Symlink(linkTarget, target); err != nil {
				return fmt.Errorf("symlink %s: %w", target, err)
			}

		default:
			// Skip pipes, sockets, devices — nothing a build context needs.
			continue
		}
	}
}

// safeJoin returns base/rel, rejecting anything that would land outside
// base. Handles leading "./", cleans double slashes, and blocks absolute
// paths (a tar header with "/etc/passwd" would otherwise overwrite it).
func safeJoin(base, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path %q not allowed", rel)
	}

	cleaned := filepath.Clean(filepath.Join(base, rel))

	if cleaned != base && !strings.HasPrefix(cleaned, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes destination", rel)
	}

	return cleaned, nil
}

// fileMode returns m masked to permission bits, or fallback if m is 0.
// Tar headers sometimes carry 0 for directories inside an archive that
// wasn't created with a real filesystem — we keep them readable.
func fileMode(m int64, fallback os.FileMode) os.FileMode {
	perm := os.FileMode(m).Perm()

	if perm == 0 {
		return fallback
	}

	return perm
}
