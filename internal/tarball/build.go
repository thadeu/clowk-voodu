// Package tarball builds Docker-compatible gzipped tar archives of a
// build context directory, respecting a .dockerignore if present.
//
// This is the client-side counterpart of internal/deploy.RunFromTarball:
// `voodu apply` calls Stream() to produce the bytes that flow over SSH
// into `voodu receive-pack` on the server. Keeping this isolated from
// the deploy package means the CLI binary doesn't have to pull in the
// server-side Docker/lang/containers graph just to build a tar.
package tarball

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/patternmatcher"
)

// Options control which files end up in the tarball. Zero value is
// valid: no ignore patterns beyond the built-in defaults, no size cap.
type Options struct {
	// ExtraIgnores are patterns appended to whatever .dockerignore
	// declares, in the same syntax. Useful for "ignore this one thing
	// per-invocation" without touching the file on disk.
	ExtraIgnores []string

	// MaxSize caps the uncompressed tar stream in bytes. Zero = no
	// limit. Exceeding it aborts the build with an error pointing at
	// .dockerignore; the partially-written stream ends abruptly, which
	// the receive-pack side will surface as an EOF during tar parse.
	MaxSize int64
}

// Default ignores applied on top of whatever the project's
// .dockerignore says. These are patterns any sane build context
// excludes — shipping them just inflates uploads and can leak secrets
// (stray .env, .git/config) to the server. Users can still force
// inclusion by adding a negation (`!.git/config`) to .dockerignore.
var builtinIgnores = []string{
	".git",
	".git/**",
	".gitignore",
	"node_modules",
	"node_modules/**",
	".voodu",
	".DS_Store",
	"**/.DS_Store",
}

// Stream writes a gzipped tar of srcDir's contents to w. Entries are
// stored relative to srcDir (so `tar tzf -` shows ./Dockerfile, not
// /home/user/app/Dockerfile). Returns the number of uncompressed bytes
// streamed — useful for log output and for enforcing MaxSize from the
// caller when it wants a softer failure than an abrupt EOF.
//
// The walk is deterministic (lexicographic) so identical directory
// state produces identical tarball bytes → identical build-ids server
// side → dedup skips the rebuild. Any non-determinism here would defeat
// the content-hash optimization.
func Stream(w io.Writer, srcDir string, opts Options) (int64, error) {
	srcAbs, err := filepath.Abs(srcDir)
	if err != nil {
		return 0, fmt.Errorf("abs %s: %w", srcDir, err)
	}

	info, err := os.Stat(srcAbs)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", srcAbs, err)
	}

	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", srcAbs)
	}

	patterns, err := loadPatterns(srcAbs, opts.ExtraIgnores)
	if err != nil {
		return 0, err
	}

	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return 0, fmt.Errorf("compile ignore patterns: %w", err)
	}

	// When the ignore list has any negation ("!foo/bar"), we can't
	// SkipDir on a matched directory: a negated descendant might still
	// belong in the tarball. Without exclusions, SkipDir is a huge win
	// for monorepos where node_modules/ is excluded.
	hasExclusions := matcher.Exclusions()

	gz := gzip.NewWriter(w)

	defer gz.Close()

	tw := tar.NewWriter(gz)

	defer tw.Close()

	var written int64

	err = filepath.Walk(srcAbs, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if path == srcAbs {
			return nil
		}

		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}

		// .dockerignore matcher expects forward slashes regardless of
		// the host OS — same posture Docker CLI takes.
		relMatch := filepath.ToSlash(rel)

		ignored, err := matcher.MatchesOrParentMatches(relMatch)
		if err != nil {
			return fmt.Errorf("match %s: %w", rel, err)
		}

		if ignored {
			if fi.IsDir() {
				// Skip entire subtree only when no negations exist;
				// otherwise descend so a `!sub/keep.me` override can
				// reintroduce a specific child.
				if !hasExclusions {
					return filepath.SkipDir
				}

				return nil
			}

			return nil
		}

		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return fmt.Errorf("header %s: %w", rel, err)
		}

		// Normalize names: Docker-style relative paths, forward slashes,
		// no leading "./" (keeps the archive small and readable). Dirs
		// get a trailing slash per POSIX tar convention.
		hdr.Name = relMatch
		if fi.IsDir() {
			hdr.Name += "/"
		}

		// If it's a symlink, resolve its target text (not follow) and
		// record as a tar symlink entry. FileInfoHeader already set
		// Typeflag=TypeSymlink when fi reports a symlink.
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", rel, err)
			}

			hdr.Linkname = target
			hdr.Size = 0
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write header %s: %w", rel, err)
		}

		if !fi.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", rel, err)
		}

		defer f.Close()

		n, err := io.Copy(tw, f)
		if err != nil {
			return fmt.Errorf("copy %s: %w", rel, err)
		}

		written += n

		if opts.MaxSize > 0 && written > opts.MaxSize {
			return fmt.Errorf("build context exceeds %d bytes — add entries to .dockerignore or raise VOODU_BUILD_MAX_SIZE", opts.MaxSize)
		}

		return nil
	})

	if err != nil {
		return written, err
	}

	// Closing in defers above ensures the gzip footer is written.
	return written, nil
}

// loadPatterns reads <srcDir>/.dockerignore (if present) and prepends
// the built-in ignores. Built-ins go first so user negations (!pattern)
// can still reintroduce a built-in-excluded path.
func loadPatterns(srcDir string, extra []string) ([]string, error) {
	patterns := append([]string{}, builtinIgnores...)

	f, err := os.Open(filepath.Join(srcDir, ".dockerignore"))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("open .dockerignore: %w", err)
		}
	} else {
		defer f.Close()

		data, err := io.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("read .dockerignore: %w", err)
		}

		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)

			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			patterns = append(patterns, line)
		}
	}

	patterns = append(patterns, extra...)

	return patterns, nil
}
