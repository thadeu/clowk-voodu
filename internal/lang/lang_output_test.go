package lang

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The build transcript is only as complete as what reaches spec.Output.
// A handler printing to os.Stdout again would silently drop out of the
// deploy plane's log, so pin the two seams: the writer resolution and
// the detection lines NewLang prints before any handler runs.
func TestBuildSpecOutFallsBackToStdout(t *testing.T) {
	var nilSpec *BuildSpec

	if nilSpec.out() != os.Stdout {
		t.Fatal("nil spec should print to stdout")
	}

	if (&BuildSpec{}).out() != os.Stdout {
		t.Fatal("spec without Output should print to stdout")
	}

	var buf bytes.Buffer

	if (&BuildSpec{Output: &buf}).out() != &buf {
		t.Fatal("spec with Output should print there")
	}
}

func TestDetectLanguageWritesToTheGivenWriter(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer

	got, err := detectLanguage(dir, &buf)
	if err != nil || got != "docker" {
		t.Fatalf("detectLanguage: %q, %v", got, err)
	}

	if !strings.Contains(buf.String(), "using 'docker' strategy") {
		t.Fatalf("detection line did not reach the writer: %q", buf.String())
	}
}
