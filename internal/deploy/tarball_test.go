package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/paths"
)

// makeTar builds a gzipped tar in memory from a slice of entry specs.
// Using a helper (rather than fixtures on disk) keeps each test's
// scenario visible right next to the assertion.
func makeTar(t *testing.T, entries []tarEntry) io.Reader {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Typeflag: e.typeflag,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
		}

		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}

		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}

		if e.typeflag == tar.TypeReg || e.typeflag == 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	return &buf
}

type tarEntry struct {
	name     string
	mode     int64
	body     []byte
	typeflag byte
	linkname string
}

func TestBufferTarballHashesStream(t *testing.T) {
	src := makeTar(t, []tarEntry{
		{name: "Dockerfile", mode: 0644, body: []byte("FROM alpine\n")},
		{name: "main.go", mode: 0644, body: []byte("package main\n")},
	})

	id1, path1, err := bufferTarball("", src)
	if err != nil {
		t.Fatalf("bufferTarball: %v", err)
	}

	defer os.Remove(path1)

	if len(id1) != BuildIDLen {
		t.Fatalf("build-id length = %d, want %d", len(id1), BuildIDLen)
	}

	// Stream-twice-same-bytes → same id. This is the dedup invariant
	// that RunFromTarball relies on to skip redundant rebuilds.
	src2 := makeTar(t, []tarEntry{
		{name: "Dockerfile", mode: 0644, body: []byte("FROM alpine\n")},
		{name: "main.go", mode: 0644, body: []byte("package main\n")},
	})

	id2, path2, err := bufferTarball("", src2)
	if err != nil {
		t.Fatalf("bufferTarball 2: %v", err)
	}

	defer os.Remove(path2)

	if id1 != id2 {
		t.Errorf("identical content produced different build-ids: %q vs %q", id1, id2)
	}
}

func TestBufferTarballDistinctForDifferentContent(t *testing.T) {
	a := makeTar(t, []tarEntry{{name: "a", mode: 0644, body: []byte("one")}})
	b := makeTar(t, []tarEntry{{name: "a", mode: 0644, body: []byte("two")}})

	idA, pA, err := bufferTarball("", a)
	if err != nil {
		t.Fatalf("a: %v", err)
	}

	defer os.Remove(pA)

	idB, pB, err := bufferTarball("", b)
	if err != nil {
		t.Fatalf("b: %v", err)
	}

	defer os.Remove(pB)

	if idA == idB {
		t.Errorf("different content yielded same build-id %q", idA)
	}
}

func TestExtractTarballBasicLayout(t *testing.T) {
	dest := t.TempDir()

	src := makeTar(t, []tarEntry{
		{name: "Dockerfile", mode: 0644, body: []byte("FROM alpine\n")},
		{name: "cmd/", mode: 0755, typeflag: tar.TypeDir},
		{name: "cmd/api/main.go", mode: 0644, body: []byte("package main\n")},
		{name: "script.sh", mode: 0755, body: []byte("#!/bin/sh\n")},
	})

	_, tmpPath, err := bufferTarball("", src)
	if err != nil {
		t.Fatalf("buffer: %v", err)
	}

	defer os.Remove(tmpPath)

	if err := extractTarball(tmpPath, dest, 0); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// Regular file content preserved.
	got, err := os.ReadFile(filepath.Join(dest, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	if string(got) != "FROM alpine\n" {
		t.Errorf("Dockerfile content = %q", got)
	}

	// Nested dir created, content readable.
	if _, err := os.ReadFile(filepath.Join(dest, "cmd/api/main.go")); err != nil {
		t.Errorf("cmd/api/main.go: %v", err)
	}

	// Executable bit preserved — critical for post-deploy scripts.
	info, err := os.Stat(filepath.Join(dest, "script.sh"))
	if err != nil {
		t.Fatalf("stat script.sh: %v", err)
	}

	if info.Mode().Perm()&0111 == 0 {
		t.Errorf("script.sh lost exec bit: %v", info.Mode().Perm())
	}
}

func TestExtractTarballRejectsPathTraversal(t *testing.T) {
	dest := t.TempDir()

	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{
			name: "parent escape",
			entries: []tarEntry{
				{name: "../evil", mode: 0644, body: []byte("pwn")},
			},
		},
		{
			name: "deep parent escape",
			entries: []tarEntry{
				{name: "sub/../../evil", mode: 0644, body: []byte("pwn")},
			},
		},
		{
			name: "absolute path",
			entries: []tarEntry{
				{name: "/etc/passwd", mode: 0644, body: []byte("pwn")},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := makeTar(t, tc.entries)

			_, tmpPath, err := bufferTarball("", src)
			if err != nil {
				t.Fatalf("buffer: %v", err)
			}

			defer os.Remove(tmpPath)

			err = extractTarball(tmpPath, dest, 0)
			if err == nil {
				t.Fatalf("expected extract to reject %s, got nil", tc.name)
			}

			if !strings.Contains(err.Error(), "escape") && !strings.Contains(err.Error(), "not allowed") {
				t.Errorf("unexpected error %q (wanted traversal refusal)", err)
			}
		})
	}
}

func TestExtractTarballRejectsEscapingSymlink(t *testing.T) {
	dest := t.TempDir()

	src := makeTar(t, []tarEntry{
		{name: "bad-link", typeflag: tar.TypeSymlink, linkname: "../../../etc/passwd"},
	})

	_, tmpPath, err := bufferTarball("", src)
	if err != nil {
		t.Fatalf("buffer: %v", err)
	}

	defer os.Remove(tmpPath)

	err = extractTarball(tmpPath, dest, 0)
	if err == nil {
		t.Fatal("expected extract to reject escaping symlink, got nil")
	}

	if !strings.Contains(err.Error(), "escaping symlink") {
		t.Errorf("unexpected error %q", err)
	}
}

// seedRelease creates a release dir with the given mtime so gc tests
// can deterministically control ordering without wall-clock sleeps.
func seedRelease(t *testing.T, app, id string, ageSeconds int) string {
	t.Helper()

	dir := filepath.Join(paths.AppReleasesDir(app), id)

	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	ts := time.Now().Add(-time.Duration(ageSeconds) * time.Second)

	if err := os.Chtimes(dir, ts, ts); err != nil {
		t.Fatalf("chtimes %s: %v", dir, err)
	}

	return dir
}

func countReleases(t *testing.T, app string) int {
	t.Helper()

	entries, err := os.ReadDir(paths.AppReleasesDir(app))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	n := 0

	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}

	return n
}

func TestGcReleasesKeepsNewestN(t *testing.T) {
	// Isolate paths.AppReleasesDir under a temp VOODU_ROOT so the test
	// cannot touch /opt/voodu even if run on a server.
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)

	app := "gc-test"

	// 8 releases with ascending ages: r0 is newest (age 0), r7 is oldest.
	for i := 0; i < 8; i++ {
		seedRelease(t, app, fmt.Sprintf("build%02d", i), i*10)
	}

	pruned, err := gcReleases(app, 5)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}

	if pruned != 3 {
		t.Errorf("pruned = %d, want 3", pruned)
	}

	remaining := countReleases(t, app)
	if remaining != 5 {
		t.Errorf("remaining = %d, want 5", remaining)
	}

	// The 5 newest (build00..build04) must still exist.
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("build%02d", i)
		if _, err := os.Stat(filepath.Join(paths.AppReleasesDir(app), name)); err != nil {
			t.Errorf("expected %s to be kept, got %v", name, err)
		}
	}

	// The oldest 3 (build05..build07) must be gone.
	for i := 5; i < 8; i++ {
		name := fmt.Sprintf("build%02d", i)
		if _, err := os.Stat(filepath.Join(paths.AppReleasesDir(app), name)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be pruned, stat err = %v", name, err)
		}
	}
}

func TestGcReleasesNeverDeletesCurrent(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)

	app := "gc-current"

	// 6 releases where build05 (the oldest) is the active `current`.
	// Without the guard, gc would prune it — breaking the container.
	for i := 0; i < 6; i++ {
		seedRelease(t, app, fmt.Sprintf("build%02d", i), i*10)
	}

	currentTarget := filepath.Join(paths.AppReleasesDir(app), "build05")

	if err := os.MkdirAll(paths.AppDir(app), 0755); err != nil {
		t.Fatalf("mkdir app: %v", err)
	}

	if err := os.Symlink(currentTarget, paths.AppCurrentLink(app)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := gcReleases(app, 3)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}

	// build05 is beyond the keep=3 cutoff by age, but it's `current` —
	// must survive.
	if _, err := os.Stat(currentTarget); err != nil {
		t.Errorf("current release was pruned: %v", err)
	}
}

func TestGcReleasesNoopBelowThreshold(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.EnvRoot, root)

	app := "gc-small"

	for i := 0; i < 3; i++ {
		seedRelease(t, app, fmt.Sprintf("build%02d", i), i*10)
	}

	pruned, err := gcReleases(app, 5)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}

	if pruned != 0 {
		t.Errorf("pruned = %d, want 0 (below threshold)", pruned)
	}

	if countReleases(t, app) != 3 {
		t.Errorf("releases touched below threshold")
	}
}

func TestExtractTarballAllowsInternalSymlink(t *testing.T) {
	dest := t.TempDir()

	src := makeTar(t, []tarEntry{
		{name: "target", mode: 0644, body: []byte("x")},
		{name: "link", typeflag: tar.TypeSymlink, linkname: "target"},
	})

	_, tmpPath, err := bufferTarball("", src)
	if err != nil {
		t.Fatalf("buffer: %v", err)
	}

	defer os.Remove(tmpPath)

	if err := extractTarball(tmpPath, dest, 0); err != nil {
		t.Fatalf("extract: %v", err)
	}

	// Symlink should exist and resolve to the sibling file.
	info, err := os.Lstat(filepath.Join(dest, "link"))
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}

	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link is not a symlink: %v", info.Mode())
	}
}

// GitHub's /tarball wraps the whole tree in `owner-repo-<sha>/`. Without
// stripping it, an `apply.file: voodu.hcl` written by somebody looking at
// their own repository resolves to a file that is not there — and it fails at
// the last step of a deploy, after the download and the build.
func TestStripComponents(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		strip int
		want  string
		keep  bool
	}{
		{"no stripping leaves the name alone", "./Dockerfile", 0, "./Dockerfile", true},
		{"strips the github wrapper", "acme-web-abc123/voodu.hcl", 1, "voodu.hcl", true},
		{"strips from a nested path", "acme-web-abc123/apps/pwa/main.go", 1, "apps/pwa/main.go", true},
		{"leading ./ is not a component", "./acme-web-abc123/voodu.hcl", 1, "voodu.hcl", true},

		// The wrapper directory itself has no destination — skipped, not an
		// error, because it is a legitimate entry.
		{"the wrapper directory is dropped", "acme-web-abc123/", 1, "", false},
		{"shallower than the strip depth is dropped", "README.md", 1, "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, keep := stripComponents(c.in, c.strip)

			if keep != c.keep {
				t.Fatalf("keep = %v, want %v", keep, c.keep)
			}

			if keep && got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Stripping must not quietly rewrite a traversal into a plausible file.
//
// path.Clean collapses `wrapper/../../etc/passwd` into `../etc/passwd`, and
// dropping a component from THAT yields `etc/passwd` — which no longer
// escapes, so safeJoin would accept it, and the entry would land somewhere
// inside the release that nothing in the tarball actually named.
func TestStripComponentsRefusesTraversal(t *testing.T) {
	for _, name := range []string{
		"acme-web-abc/../../etc/passwd",
		"../escape",
		"..",
	} {
		if got, keep := stripComponents(name, 1); keep {
			t.Errorf("%q was rewritten to %q instead of being refused", name, got)
		}
	}
}

// A box whose /tmp is read-only — a hardened systemd unit
// (`ProtectSystem=strict`, `PrivateTmp=`) or a read-only rootfs — failed here
// with "buffer tarball: read-only file system" on a deploy that had nothing
// wrong with it. Twice: once extracting, once buffering. This is the second.
func TestBufferTarballUsesTheScratchDirItIsGiven(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "builds")

	_, path, err := bufferTarball(dir, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}

	defer os.Remove(path)

	if !strings.HasPrefix(path, dir) {
		t.Errorf("buffered to %q, want it under %q", path, dir)
	}
}

// Empty keeps the old behaviour, so a caller nobody reconfigured still works.
func TestBufferTarballFallsBackToTheSystemTempDir(t *testing.T) {
	_, path, err := bufferTarball("", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}

	defer os.Remove(path)

	if !strings.HasPrefix(path, os.TempDir()) {
		t.Errorf("buffered to %q, want it under %q", path, os.TempDir())
	}
}

// The error has to name the directory. Without it, "read-only file system"
// leaves the operator guessing which one — and an empty dir means /tmp, which
// is exactly the knowledge they lack at that moment.
func TestBufferTarballNamesTheDirectoryItCouldNotUse(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-dir")

	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := bufferTarball(filepath.Join(blocked, "builds"), strings.NewReader("x"))
	if err == nil {
		t.Fatal("expected an error")
	}

	if !strings.Contains(err.Error(), blocked) {
		t.Errorf("error %q does not name the path", err)
	}
}
