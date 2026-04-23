package tarball

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// listTar decompresses a gzipped tar in memory and returns the entry
// names sorted — sort order lets tests assert "file X is present" and
// "file Y is absent" without caring about walk order.
func listTar(t *testing.T, r io.Reader) []string {
	t.Helper()

	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}

	defer gz.Close()

	tr := tar.NewReader(gz)

	var names []string

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatalf("tar next: %v", err)
		}

		names = append(names, hdr.Name)
	}

	sort.Strings(names)

	return names
}

func writeFile(t *testing.T, dir, rel string, data []byte, mode os.FileMode) {
	t.Helper()

	full := filepath.Join(dir, rel)

	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}

	if err := os.WriteFile(full, data, mode); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

func TestStreamIncludesRegularFilesAndDirs(t *testing.T) {
	src := t.TempDir()

	writeFile(t, src, "Dockerfile", []byte("FROM alpine\n"), 0644)
	writeFile(t, src, "cmd/api/main.go", []byte("package main\n"), 0644)
	writeFile(t, src, "go.mod", []byte("module x\n"), 0644)

	var buf bytes.Buffer

	n, err := Stream(&buf, src, Options{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if n == 0 {
		t.Errorf("Stream wrote 0 bytes, expected payload")
	}

	names := listTar(t, &buf)

	want := []string{"Dockerfile", "cmd/", "cmd/api/", "cmd/api/main.go", "go.mod"}

	for _, w := range want {
		found := false

		for _, got := range names {
			if got == w {
				found = true
				break
			}
		}

		if !found {
			t.Errorf("expected entry %q, got %v", w, names)
		}
	}
}

func TestStreamRespectsBuiltinIgnores(t *testing.T) {
	src := t.TempDir()

	writeFile(t, src, "Dockerfile", []byte("FROM alpine\n"), 0644)
	writeFile(t, src, ".git/config", []byte("[core]\n"), 0644)
	writeFile(t, src, "node_modules/react/index.js", []byte("x\n"), 0644)
	writeFile(t, src, ".DS_Store", []byte("bin\n"), 0644)

	var buf bytes.Buffer

	if _, err := Stream(&buf, src, Options{}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	names := listTar(t, &buf)

	for _, n := range names {
		if n == ".git/" || n == ".git/config" {
			t.Errorf(".git leaked into tarball: %v", names)
		}

		if n == "node_modules/" || n == "node_modules/react/index.js" {
			t.Errorf("node_modules leaked: %v", names)
		}

		if n == ".DS_Store" {
			t.Errorf(".DS_Store leaked: %v", names)
		}
	}
}

func TestStreamRespectsDockerignore(t *testing.T) {
	src := t.TempDir()

	writeFile(t, src, "Dockerfile", []byte("FROM alpine\n"), 0644)
	writeFile(t, src, "keep.go", []byte("// keep\n"), 0644)
	writeFile(t, src, "tmp/junk.log", []byte("junk\n"), 0644)
	writeFile(t, src, "secrets.env", []byte("k=v\n"), 0644)
	writeFile(t, src, ".dockerignore", []byte("tmp\nsecrets.env\n"), 0644)

	var buf bytes.Buffer

	if _, err := Stream(&buf, src, Options{}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	names := listTar(t, &buf)

	contains := func(x string) bool {
		for _, n := range names {
			if n == x {
				return true
			}
		}

		return false
	}

	if !contains("Dockerfile") || !contains("keep.go") {
		t.Errorf("lost expected files: %v", names)
	}

	if contains("tmp/") || contains("tmp/junk.log") {
		t.Errorf("tmp/ leaked despite .dockerignore: %v", names)
	}

	if contains("secrets.env") {
		t.Errorf("secrets.env leaked despite .dockerignore: %v", names)
	}
}

func TestStreamDockerignoreNegation(t *testing.T) {
	src := t.TempDir()

	writeFile(t, src, "Dockerfile", []byte("FROM alpine\n"), 0644)
	writeFile(t, src, "build/important.bin", []byte("x\n"), 0644)
	writeFile(t, src, "build/junk.tmp", []byte("y\n"), 0644)
	// Ignore the whole build/ dir, but negate important.bin so it's
	// shipped anyway. Common pattern when most of a directory is
	// generated but one file is required by the build.
	writeFile(t, src, ".dockerignore", []byte("build\n!build/important.bin\n"), 0644)

	var buf bytes.Buffer

	if _, err := Stream(&buf, src, Options{}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	names := listTar(t, &buf)

	hasImportant := false
	hasJunk := false

	for _, n := range names {
		if n == "build/important.bin" {
			hasImportant = true
		}

		if n == "build/junk.tmp" {
			hasJunk = true
		}
	}

	if !hasImportant {
		t.Errorf("build/important.bin dropped despite negation: %v", names)
	}

	if hasJunk {
		t.Errorf("build/junk.tmp leaked: %v", names)
	}
}

func TestStreamDeterministicAcrossRuns(t *testing.T) {
	src := t.TempDir()

	writeFile(t, src, "Dockerfile", []byte("FROM alpine\n"), 0644)
	writeFile(t, src, "cmd/api/main.go", []byte("package main\n"), 0644)
	writeFile(t, src, "go.mod", []byte("module x\n"), 0644)

	var a, b bytes.Buffer

	if _, err := Stream(&a, src, Options{}); err != nil {
		t.Fatalf("stream a: %v", err)
	}

	if _, err := Stream(&b, src, Options{}); err != nil {
		t.Fatalf("stream b: %v", err)
	}

	// Entry order must match byte-for-byte — server-side build-id is a
	// hash of the stream, so non-determinism breaks dedup.
	if !bytes.Equal(listEntryNames(t, &a), listEntryNames(t, &b)) {
		t.Errorf("tarball entries non-deterministic across runs")
	}
}

// listEntryNames returns the tar entry names as a newline-joined blob
// so bytes.Equal can compare them. The tar *payload* bytes would differ
// run-to-run (timestamps inside tar headers) — we only care that the
// contents and order are stable, which is what matters for the hash.
func listEntryNames(t *testing.T, r io.Reader) []byte {
	names := listTar(t, r)
	var out []byte
	for _, n := range names {
		out = append(out, n...)
		out = append(out, '\n')
	}
	return out
}

func TestStreamMaxSizeEnforced(t *testing.T) {
	src := t.TempDir()

	writeFile(t, src, "big.bin", bytes.Repeat([]byte("x"), 10*1024), 0644)

	var buf bytes.Buffer

	_, err := Stream(&buf, src, Options{MaxSize: 512})
	if err == nil {
		t.Fatal("expected MaxSize to error, got nil")
	}
}

func TestStreamExtraIgnores(t *testing.T) {
	src := t.TempDir()

	writeFile(t, src, "Dockerfile", []byte("FROM alpine\n"), 0644)
	writeFile(t, src, "dist/app.js", []byte("x"), 0644)

	var buf bytes.Buffer

	if _, err := Stream(&buf, src, Options{ExtraIgnores: []string{"dist"}}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	names := listTar(t, &buf)

	for _, n := range names {
		if n == "dist/" || n == "dist/app.js" {
			t.Errorf("extra ignore not honored: %v", names)
		}
	}
}
