// deploy_build.go is the build-mode half of the executor: a workload that
// declares no image gets one built from the repository's source, on this box.
//
// THE SHAPE, and the one part of it that is not obvious.
//
// The tarball is downloaded ONCE and extracted once. Then, for EACH build-mode
// workload, its build-context subdirectory is re-tarred and handed to the same
// pipeline `vd apply` uses.
//
// Re-tarring per workload is not waste. `buildID` is the sha256 of the build
// context, and that is what makes "same content, skip the rebuild" correct.
// Feeding two workloads the whole repository archive would give them the SAME
// buildID with DIFFERENT content — and the dedup would then skip builds that
// had to happen, which is the worst kind of wrong: it works until it silently
// deploys the previous version.
//
// Re-tarring through internal/tarball also means the context honours
// .dockerignore, .gitignore and builtinIgnores exactly as it does on a laptop,
// so a deploy-plane build produces the same image as `vd apply` — rather than
// one that merely usually matches.

package controller

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.voodu.clowk.in/internal/tarball"
)

// SourceBuilder builds one workload from a build-context tarball.
//
// Injected for the same reason ManifestParser is: internal/deploy imports this
// package, so calling it directly would be a cycle. `buildSpec` is the
// workload's spec as raw JSON — the wiring in main.go decodes it into the
// deploy package's own type, which is the only place that can name it.
type SourceBuilder func(app string, context io.Reader, buildSpec json.RawMessage, force bool) error

// buildTarget is one workload that must be built before it can be applied.
type buildTarget struct {
	App  string          // <scope>-<name>, what the release tree is keyed by
	Path string          // build context, repository-relative
	Spec json.RawMessage // the workload's spec, for lang / dockerfile / build args
	Ref  string          // kind/scope/name, for messages
}

// tempRootLabel names the directory in an error, so "read-only file system"
// says WHICH one. Without it the operator has to know that an empty root means
// /tmp, which is exactly the knowledge they lack at that moment.
func tempRootLabel(root string) string {
	if root != "" {
		return root
	}

	return os.TempDir()
}

// buildTargets returns the workloads in this manifest set that need source.
//
// A workload is build mode when it declares no image: the image is produced by
// the pipeline rather than pulled. A spec that will not decode is skipped —
// the apply path validates specs properly and produces a better error than a
// guess made here.
func buildTargets(manifests []Manifest) []buildTarget {
	var out []buildTarget

	for i := range manifests {
		m := manifests[i]

		if m.Kind != KindDeployment && m.Kind != KindStatefulset {
			continue
		}

		var probe struct {
			Image string `json:"image"`
			Build *struct {
				Path string `json:"path"`
			} `json:"build"`
		}

		if err := json.Unmarshal(m.Spec, &probe); err != nil {
			continue
		}

		if strings.TrimSpace(probe.Image) != "" || probe.Build == nil {
			continue
		}

		// "." is the repository root, matching what `vd apply` means when a
		// build block names no path.
		path := strings.TrimSpace(probe.Build.Path)
		if path == "" {
			path = "."
		}

		out = append(out, buildTarget{
			App:  AppID(m.Scope, m.Name),
			Path: path,
			Spec: m.Spec,
			Ref:  string(m.Kind) + "/" + m.Scope + "/" + m.Name,
		})
	}

	return out
}

// extractRepoArchive unpacks a GitHub tarball into a temporary directory,
// dropping the `owner-repo-<sha>/` wrapper it puts around everything.
//
// Returns the directory and a cleanup. The caller must call cleanup even on
// error paths: a build that failed still leaves a repository on disk, and a
// box that accumulates one per failed deploy fills up quietly.
//
// `root` IS WHERE, and passing it beats defaulting to /tmp.
//
// A controller running under a hardened unit — `ProtectSystem=strict`,
// `PrivateTmp=`, a read-only rootfs — has no writable /tmp, and the failure
// surfaces four layers up as "temp dir: read-only file system" on a deploy
// that had nothing wrong with it. The plugin installer already extracts under
// a directory the controller owns; this is the same rule.
//
// Empty root keeps the old behaviour (os.MkdirTemp's own default, which honours
// TMPDIR), so a controller nobody has reconfigured still works.
func extractRepoArchive(root string, src io.Reader) (string, func(), error) {
	if root != "" {
		// The parent has to exist before MkdirTemp will use it, and it is ours
		// to create — the operator configured a path, not a tree.
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", func() {}, fmt.Errorf("build root %s: %w", root, err)
		}
	}

	dir, err := os.MkdirTemp(root, "voodu-deploy-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("temp dir under %q: %w", tempRootLabel(root), err)
	}

	cleanup := func() { _ = os.RemoveAll(dir) }

	gz, err := gzip.NewReader(src)
	if err != nil {
		cleanup()

		return "", func() {}, fmt.Errorf("repository archive is not gzip: %w", err)
	}

	defer gz.Close()

	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()

		if err == io.EOF {
			return dir, cleanup, nil
		}

		if err != nil {
			cleanup()

			return "", func() {}, fmt.Errorf("repository archive: %w", err)
		}

		if err := extractEntry(dir, hdr, tr); err != nil {
			cleanup()

			return "", func() {}, err
		}
	}
}

func extractEntry(dir string, hdr *tar.Header, tr *tar.Reader) error {
	name, keep := stripArchiveWrapper(hdr.Name)
	if !keep {
		return nil
	}

	target, err := safeArchiveJoin(dir, name)
	if err != nil {
		return fmt.Errorf("entry %q: %w", hdr.Name, err)
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o755)

	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm()|0o200)
		if err != nil {
			return err
		}

		defer f.Close()

		_, err = io.Copy(f, tr)

		return err

	default:
		// Symlinks, devices, hard links: skipped rather than recreated.
		//
		// A build context does not need them, and a symlink is how an archive
		// reaches outside the directory it was extracted into — the class of
		// bug that has cost several build systems a CVE. Skipping loses
		// nothing real and removes the whole category.
		return nil
	}
}

// stripArchiveWrapper drops GitHub's `owner-repo-<sha>/` prefix.
//
// Refuses anything that still climbs after cleaning, rather than stripping it:
// `wrapper/../../etc/passwd` cleans to `../etc/passwd`, and removing one
// component from THAT yields `etc/passwd` — a path that no longer escapes, so
// the join would accept it, and the file would land somewhere inside the
// extraction that nothing in the archive named.
func stripArchiveWrapper(name string) (string, bool) {
	cleaned := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(strings.TrimPrefix(name, "./"))), "/")

	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}

	parts := strings.Split(cleaned, "/")
	if len(parts) <= 1 {
		return "", false
	}

	return strings.Join(parts[1:], "/"), true
}

func safeArchiveJoin(base, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path not allowed")
	}

	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}

	cleaned := filepath.Clean(filepath.Join(baseAbs, rel))

	if cleaned != baseAbs && !strings.HasPrefix(cleaned, baseAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("escapes the destination")
	}

	return cleaned, nil
}

// buildOne re-tars a workload's build context and runs the pipeline on it.
//
// Streamed through a pipe rather than buffered: the context can be hundreds of
// megabytes, and the builder consumes it as the tar is produced.
func (a *API) buildOne(root string, target buildTarget, force bool) error {
	contextDir, err := safeArchiveJoin(root, target.Path)
	if err != nil {
		return fmt.Errorf("%s: build path %q is not inside the repository", target.Ref, target.Path)
	}

	if info, err := os.Stat(contextDir); err != nil || !info.IsDir() {
		return fmt.Errorf("%s: build path %q is not a directory in this commit", target.Ref, target.Path)
	}

	pr, pw := io.Pipe()

	// tarball.Stream gzips internally, so nothing wraps it here. Adding a
	// second gzip writer produces a doubly-compressed stream that the builder
	// decodes one layer of and then reads as a truncated tar — which is how it
	// reports itself, several layers away from the cause.
	go func() {
		_, err := tarball.Stream(pw, contextDir, tarball.Options{})

		_ = pw.CloseWithError(err)
	}()

	defer pr.Close()

	if err := a.BuildFromSource(target.App, pr, target.Spec, force); err != nil {
		return fmt.Errorf("%s: %w", target.Ref, err)
	}

	return nil
}
