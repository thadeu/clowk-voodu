package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "go.voodu.clowk.in/internal/github"
)

// repoArchive builds a GitHub-shaped tarball: every path wrapped in
// `owner-repo-<sha>/`, which is the detail this code exists to handle.
func repoArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	buf := new(bytes.Buffer)
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)

	for name, content := range files {
		hdr := &tar.Header{
			Name: "acme-web-abc1234/" + name,
			Mode: 0o644,
			Size: int64(len(content)),
		}

		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// The wrapper is the classic mistake with this endpoint: keep it and every
// path gains a prefix, so an `apply.file: voodu.hcl` written by somebody
// looking at their own repository resolves to a file that is not there.
func TestExtractRepoArchiveDropsTheWrapper(t *testing.T) {
	raw := repoArchive(t, map[string]string{
		"voodu.hcl":           "manifest",
		"apps/pwa/main.go":    "package main",
		"apps/pwa/Dockerfile": "FROM scratch",
	})

	dir, cleanup, err := extractRepoArchive(t.TempDir(), bytes.NewReader(raw))

	defer cleanup()

	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"voodu.hcl", "apps/pwa/main.go"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s missing after extraction: %v", want, err)
		}
	}

	// The wrapper must not survive as a directory of its own.
	if _, err := os.Stat(filepath.Join(dir, "acme-web-abc1234")); err == nil {
		t.Error("the wrapper directory was extracted")
	}
}

// A symlink is how an archive reaches outside the directory it was extracted
// into — the class of bug that has cost several build systems a CVE. A build
// context does not need them, so skipping loses nothing and removes the whole
// category.
func TestExtractRepoArchiveSkipsSymlinks(t *testing.T) {
	buf := new(bytes.Buffer)
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)

	_ = tw.WriteHeader(&tar.Header{
		Name:     "acme-web-abc/escape",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
		Mode:     0o777,
	})

	_ = tw.WriteHeader(&tar.Header{Name: "acme-web-abc/ok.txt", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))

	_ = tw.Close()
	_ = gz.Close()

	dir, cleanup, err := extractRepoArchive(t.TempDir(), bytes.NewReader(buf.Bytes()))

	defer cleanup()

	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(filepath.Join(dir, "escape")); err == nil {
		t.Fatal("a symlink was recreated from the archive")
	}

	if _, err := os.Stat(filepath.Join(dir, "ok.txt")); err != nil {
		t.Errorf("a regular file beside the symlink was lost: %v", err)
	}
}

// `wrapper/../../etc/passwd` cleans to `../etc/passwd`, and removing one
// component from THAT yields `etc/passwd` — no longer escaping, so the join
// would accept it, and the file would land somewhere nothing in the archive
// named.
func TestStripArchiveWrapperRefusesTraversal(t *testing.T) {
	for _, name := range []string{"wrapper/../../etc/passwd", "../escape", ".."} {
		if got, keep := stripArchiveWrapper(name); keep {
			t.Errorf("%q was rewritten to %q instead of being refused", name, got)
		}
	}
}

// A workload naming an image is registry mode even with a build block — the
// image is what runs, so nothing needs building.
func TestBuildTargetsOnlyPicksWorkloadsWithoutAnImage(t *testing.T) {
	manifests := []Manifest{
		{Kind: KindDeployment, Scope: "runa", Name: "built", Spec: json.RawMessage(`{"build":{"path":"apps/pwa"}}`)},
		{Kind: KindDeployment, Scope: "runa", Name: "pulled", Spec: json.RawMessage(`{"image":"acme/x:1","build":{}}`)},
		{Kind: KindIngress, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"host":"x"}`)},
	}

	targets := buildTargets(manifests)

	if len(targets) != 1 {
		t.Fatalf("targets = %+v", targets)
	}

	if targets[0].App != "runa-built" {
		t.Errorf("app = %q, want the scope-name form the release tree is keyed by", targets[0].App)
	}

	if targets[0].Path != "apps/pwa" {
		t.Errorf("path = %q", targets[0].Path)
	}
}

// A build block naming no path means the repository root, matching what
// `vd apply` means by the same thing.
func TestBuildTargetsDefaultsThePathToRoot(t *testing.T) {
	targets := buildTargets([]Manifest{
		{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"build":{}}`)},
	})

	if len(targets) != 1 || targets[0].Path != "." {
		t.Fatalf("targets = %+v", targets)
	}
}

// THE property that makes per-workload re-tarring worth its cost: two
// workloads in one repository must get DIFFERENT build contexts. Handing both
// the whole archive would give them the same buildID with different content,
// and the dedup would then skip builds that had to happen.
func TestEachWorkloadIsBuiltFromItsOwnContext(t *testing.T) {
	raw := repoArchive(t, map[string]string{
		"apps/pwa/main.go": "PWA SOURCE",
		"apps/api/main.go": "API SOURCE",
	})

	root, cleanup, err := extractRepoArchive(t.TempDir(), bytes.NewReader(raw))

	defer cleanup()

	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]string{}

	api, _ := newTestAPI(t)
	api.BuildFromSource = func(app string, src io.Reader, _ json.RawMessage, _ bool) error {
		seen[app] = tarContents(t, src)

		return nil
	}

	for _, target := range []buildTarget{
		{App: "runa-pwa", Path: "apps/pwa", Ref: "deployment/runa/pwa"},
		{App: "runa-api", Path: "apps/api", Ref: "deployment/runa/api"},
	} {
		if err := api.buildOne(root, target, false); err != nil {
			t.Fatal(err)
		}
	}

	if !bytes.Contains([]byte(seen["runa-pwa"]), []byte("PWA SOURCE")) {
		t.Errorf("pwa context: %q", seen["runa-pwa"])
	}

	if bytes.Contains([]byte(seen["runa-pwa"]), []byte("API SOURCE")) {
		t.Error("the pwa build context leaked the api's source — the contexts are not separate")
	}

	if !bytes.Contains([]byte(seen["runa-api"]), []byte("API SOURCE")) {
		t.Errorf("api context: %q", seen["runa-api"])
	}
}

// A build path that is not in the commit is a configuration error in the
// repository, and the message has to name which workload and which path.
func TestBuildOneReportsAMissingContext(t *testing.T) {
	raw := repoArchive(t, map[string]string{"README.md": "hi"})

	root, cleanup, err := extractRepoArchive(t.TempDir(), bytes.NewReader(raw))

	defer cleanup()

	if err != nil {
		t.Fatal(err)
	}

	api, _ := newTestAPI(t)
	api.BuildFromSource = func(string, io.Reader, json.RawMessage, bool) error { return nil }

	err = api.buildOne(root, buildTarget{App: "runa-pwa", Path: "apps/pwa", Ref: "deployment/runa/pwa"}, false)

	if err == nil {
		t.Fatal("a missing build path must be refused")
	}

	if !bytes.Contains([]byte(err.Error()), []byte("apps/pwa")) {
		t.Errorf("the error should name the path: %v", err)
	}
}

// A build path escaping the repository would read from the box's filesystem.
func TestBuildOneRefusesAnEscapingPath(t *testing.T) {
	api, _ := newTestAPI(t)
	api.BuildFromSource = func(string, io.Reader, json.RawMessage, bool) error { return nil }

	dir := t.TempDir()

	if err := api.buildOne(dir, buildTarget{App: "x", Path: "../../etc", Ref: "deployment/x/y"}, false); err == nil {
		t.Fatal("a path outside the repository must be refused")
	}
}

// tarContents flattens a gzipped tar into one string, for asserting what a
// build context did and did not carry.
func tarContents(t *testing.T, src io.Reader) string {
	t.Helper()

	gz, err := gzip.NewReader(src)
	if err != nil {
		t.Fatal(err)
	}

	defer gz.Close()

	out := new(bytes.Buffer)
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()

		if err == io.EOF {
			return out.String()
		}

		if err != nil {
			t.Fatal(err)
		}

		out.WriteString(hdr.Name + "\n")

		if _, err := io.Copy(out, tr); err != nil {
			t.Fatal(err)
		}
	}
}

// THE optimisation, and the shape of it: a directory's git SHA changes only
// when something inside it changed, so the download is decided BEFORE it
// happens. The buildID dedup still exists and still works — it just runs after
// the transfer, so on its own it saves the build and not the 20MB.
func TestStaleTargetsSkipsUnchangedContexts(t *testing.T) {
	tree := treeWith(map[string]string{"apps/pwa": "t9", "apps/api": "ta"})
	snapshot := repoSnapshot{Tree: tree}
	snapshot.Commit.SHA = "commit1"
	snapshot.Commit.Commit.Tree.SHA = "root1"

	targets := []buildTarget{
		{App: "runa-pwa", Path: "apps/pwa"},
		{App: "runa-api", Path: "apps/api"},
	}

	// Nothing built before: both are stale.
	fresh := &Trigger{}

	stale, current := staleTargets(fresh, snapshot, targets)

	if len(stale) != 2 {
		t.Fatalf("a first deploy must build everything: %+v", stale)
	}

	if current["apps/pwa"] != "t9" || current["apps/api"] != "ta" {
		t.Fatalf("current = %v", current)
	}

	// Both recorded: nothing is stale, so nothing downloads.
	seen := &Trigger{LastBuilt: map[string]string{"apps/pwa": "t9", "apps/api": "ta"}}

	if stale, _ := staleTargets(seen, snapshot, targets); len(stale) != 0 {
		t.Fatalf("an unchanged tree must build nothing: %+v", stale)
	}

	// One moved: only that one is stale. A monorepo deploying three services
	// where one changed must not rebuild the other two.
	moved := &Trigger{LastBuilt: map[string]string{"apps/pwa": "OLD", "apps/api": "ta"}}

	stale, _ = staleTargets(moved, snapshot, targets)

	if len(stale) != 1 || stale[0].Path != "apps/pwa" {
		t.Fatalf("only the changed context should rebuild: %+v", stale)
	}
}

// A path that is not in the commit is treated as stale rather than skipped:
// the build then fails with "that directory is not in this commit", which
// names the problem. Skipping would report a successful deploy that built
// nothing.
func TestStaleTargetsTreatsAMissingPathAsStale(t *testing.T) {
	snapshot := repoSnapshot{Tree: treeWith(map[string]string{"apps/api": "ta"})}
	snapshot.Commit.Commit.Tree.SHA = "root1"

	trigger := &Trigger{LastBuilt: map[string]string{"apps/pwa": "t9"}}

	stale, current := staleTargets(trigger, snapshot, []buildTarget{{Path: "apps/pwa"}})

	if len(stale) != 1 {
		t.Fatal("a path missing from the commit must not be silently skipped")
	}

	if _, ok := current["apps/pwa"]; ok {
		t.Error("a path that is not in the tree has no SHA to record")
	}
}

// A build block with no path means the repository root, whose SHA is the
// commit's own tree.
func TestSubtreeSHAOfTheRootIsTheCommitTree(t *testing.T) {
	snapshot := repoSnapshot{Tree: treeWith(nil)}
	snapshot.Commit.Commit.Tree.SHA = "root1"

	for _, p := range []string{".", "", "./"} {
		sha, ok := snapshot.subtreeSHA(p)

		if !ok || sha != "root1" {
			t.Errorf("subtreeSHA(%q) = %q, %v; want the commit tree", p, sha, ok)
		}
	}
}

// The snapshot exists so the commit and the tree are fetched ONCE. Reading a
// blob out of it must not reach the network again.
func TestBlobSHAReadsTheSnapshot(t *testing.T) {
	snapshot := repoSnapshot{Tree: gh.Tree{Entries: []gh.TreeEntry{
		{Path: "voodu.hcl", Type: "blob", SHA: "b1"},
		{Path: "apps", Type: "tree", SHA: "t1"},
	}}}

	if sha, ok := snapshot.blobSHA("voodu.hcl"); !ok || sha != "b1" {
		t.Errorf("blobSHA = %q, %v", sha, ok)
	}

	if sha, ok := snapshot.blobSHA("./voodu.hcl"); !ok || sha != "b1" {
		t.Errorf("a leading ./ should resolve the same file: %q, %v", sha, ok)
	}

	// A directory is not a file.
	if _, ok := snapshot.blobSHA("apps"); ok {
		t.Error("a tree entry was returned as a blob")
	}
}

func treeWith(dirs map[string]string) gh.Tree {
	tree := gh.Tree{}

	for path, sha := range dirs {
		tree.Entries = append(tree.Entries, gh.TreeEntry{Path: path, Type: "tree", SHA: sha})
	}

	return tree
}

// A controller under a hardened unit — `ProtectSystem=strict`, `PrivateTmp=`,
// a read-only rootfs — has no writable /tmp, and the failure surfaced four
// layers up as "temp dir: read-only file system" on a deploy that had nothing
// wrong with it.
//
// The root is passed in for that reason. The plugin installer already extracts
// under a directory the controller owns; this is the same rule arriving late.
func TestExtractUsesTheRootItIsGiven(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "builds")

	raw := repoArchive(t, map[string]string{"main.go": "package main"})

	dir, cleanup, err := extractRepoArchive(root, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	defer cleanup()

	if !strings.HasPrefix(dir, root) {
		t.Errorf("extracted to %q, want it under %q", dir, root)
	}

	// The parent is created rather than required: the operator configured a
	// path, not a tree.
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the build root should have been created: %v", err)
	}
}

// The error has to name the directory. Without it the operator has to know
// that an empty root means /tmp, which is exactly the knowledge they lack at
// that moment.
func TestExtractNamesTheDirectoryItCouldNotUse(t *testing.T) {
	// A file where a directory has to be: MkdirAll refuses, deterministically.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")

	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := extractRepoArchive(filepath.Join(blocked, "builds"), bytes.NewReader(nil))
	if err == nil {
		t.Fatal("expected an error")
	}

	if !strings.Contains(err.Error(), blocked) {
		t.Errorf("error %q does not name the path", err)
	}
}

// Empty root keeps the old behaviour, so a controller nobody reconfigured
// still deploys.
func TestExtractFallsBackToTheSystemTempDir(t *testing.T) {
	raw := repoArchive(t, map[string]string{"main.go": "package main"})

	dir, cleanup, err := extractRepoArchive("", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	defer cleanup()

	if !strings.HasPrefix(dir, os.TempDir()) {
		t.Errorf("extracted to %q, want it under %q", dir, os.TempDir())
	}
}
