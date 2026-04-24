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

	// Progress receives human-readable one-liners about the build
	// (which ignore file drove the filtering, how many entries
	// shipped, final byte count). Nil = silent. Wired to os.Stderr
	// from the CLI so users can see what's flowing without having to
	// reach for --verbose.
	Progress io.Writer
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

	patterns, source, err := loadPatternsAndSource(srcAbs, opts.ExtraIgnores)
	if err != nil {
		return 0, err
	}

	logProgress(opts.Progress, "tarball: %s (%d patterns)", ignoreSourceLabel(source), len(patterns))

	warnIfDockerfileIgnored(opts.Progress, srcAbs, patterns, source)

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

	var (
		written   int64
		filesSent int
	)

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

		filesSent++

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

	logProgress(opts.Progress, "tarball: %d files, %s uncompressed", filesSent, humanBytes(written))

	// Closing in defers above ensures the gzip footer is written.
	return written, nil
}

func logProgress(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}

	fmt.Fprintf(w, format+"\n", args...)
}

func ignoreSourceLabel(source string) string {
	switch source {
	case "dockerignore":
		return "using .dockerignore"
	case "gitignore":
		return "using .gitignore (no .dockerignore present)"
	default:
		return "no ignore file found — shipping everything except built-in blocklist"
	}
}

func humanBytes(n int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
	)

	switch {
	case n >= gb:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.2f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.2f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// loadPatterns resolves the ignore list for srcDir. Resolution order,
// first hit wins:
//
//  1. <srcDir>/.dockerignore — the Docker convention. When present, it
//     fully replaces .gitignore (matches `docker build` behavior).
//  2. <srcDir>/.gitignore    — reused as a pragmatic default because
//     most projects already keep it tidy (build dirs, secrets, caches).
//     Minor syntactic divergence (git's `/pattern` for root-only, some
//     `**` edge cases) is accepted; if a user needs strict semantics,
//     they add an explicit `.dockerignore`.
//  3. built-in floor only    — ships everything minus `.git`,
//     `node_modules`, `.DS_Store`, `.voodu`. DevOps decides what else
//     belongs in .dockerignore.
//
// Built-ins are always prepended — even with a user file — because
// shipping `.git/` to the build context leaks history and secrets,
// `node_modules/` is OS/arch-dependent garbage, and `.voodu` is our
// own state dir. Users can still reintroduce any of these with a
// negation pattern (`!.git/HEAD`) in their own file.
func loadPatternsAndSource(srcDir string, extra []string) ([]string, string, error) {
	patterns := append([]string{}, builtinIgnores...)

	userPatterns, source, err := readIgnoreFile(srcDir)
	if err != nil {
		return nil, "", err
	}

	patterns = append(patterns, userPatterns...)
	patterns = append(patterns, extra...)

	return patterns, source, nil
}

// warnIfDockerfileIgnored surfaces a loud message when the source dir
// has a `Dockerfile` on disk but the active ignore patterns would
// strip it from the tarball. Rails's `rails new --dockerfile`-
// generated `.dockerignore` contains `/Dockerfile*`, which silently
// causes the server to fall back to lang auto-gen instead of using
// the user's Dockerfile — an extremely surprising, hard-to-debug
// failure. We don't auto-override because the user may have meant it
// (e.g., local-only Dockerfile alongside a different build strategy),
// but we make sure they see what's happening.
func warnIfDockerfileIgnored(progress io.Writer, srcDir string, patterns []string, source string) {
	if progress == nil {
		return
	}

	if _, err := os.Stat(filepath.Join(srcDir, "Dockerfile")); err != nil {
		return
	}

	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return
	}

	ignored, err := matcher.MatchesOrParentMatches("Dockerfile")
	if err != nil || !ignored {
		return
	}

	label := ".dockerignore"

	if source == "gitignore" {
		label = ".gitignore"
	}

	fmt.Fprintf(progress, "!!!    WARNING: Dockerfile exists in the build context but is excluded by %s\n", label)
	fmt.Fprintf(progress, "!!!    The server will fall back to lang auto-detection. Add `!Dockerfile` to %s to ship it.\n", label)
}

// readIgnoreFile picks the first ignore file that exists in srcDir and
// returns its parsed patterns plus a label describing which file was
// read ("dockerignore", "gitignore", or "" when neither exists). The
// label lets the caller log what drove the filtering, which is the
// first thing you want to see when a build context is surprising.
func readIgnoreFile(srcDir string) ([]string, string, error) {
	candidates := []struct {
		name  string
		label string
	}{
		{".dockerignore", "dockerignore"},
		{".gitignore", "gitignore"},
	}

	for _, c := range candidates {
		data, err := os.ReadFile(filepath.Join(srcDir, c.name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return nil, "", fmt.Errorf("read %s: %w", c.name, err)
		}

		patterns := parseIgnoreLines(string(data))

		return patterns, c.label, nil
	}

	return nil, "", nil
}

// parseIgnoreLines splits raw file content into a pattern slice. Blank
// lines and `#` comments are dropped; leading/trailing whitespace is
// trimmed. A leading `/` (gitignore syntax for "root-only") is
// stripped because patternmatcher treats all patterns as root-relative
// already — `foo` and `/foo` are equivalent in dockerignore semantics.
func parseIgnoreLines(content string) []string {
	var out []string

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		line = strings.TrimPrefix(line, "/")

		out = append(out, line)
	}

	return out
}
