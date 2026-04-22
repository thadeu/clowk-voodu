package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFile(t *testing.T) {
	vars, err := Load(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(vars) != 0 {
		t.Errorf("expected empty map, got %d entries", len(vars))
	}
}

func TestLoadIgnoresCommentsAndBlanks(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")

	err := os.WriteFile(p, []byte(`
# a comment
KEY=value

OTHER=1=2
`), 0600)
	if err != nil {
		t.Fatal(err)
	}

	vars, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}

	if vars["KEY"] != "value" {
		t.Errorf("KEY mismatch: %q", vars["KEY"])
	}

	if vars["OTHER"] != "1=2" {
		t.Errorf("OTHER mismatch (should keep first = split): %q", vars["OTHER"])
	}
}

func TestSaveSortsKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")

	err := Save(p, map[string]string{"B": "2", "A": "1"})
	if err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(p)
	want := "A=1\nB=2\n"

	if string(got) != want {
		t.Errorf("save mismatch:\ngot:  %q\nwant: %q", string(got), want)
	}
}
