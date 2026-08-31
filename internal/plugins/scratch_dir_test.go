package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAllIgnoresTheInstallScratchDir(t *testing.T) {
	root := t.TempDir()

	real := filepath.Join(root, "traffik")
	os.MkdirAll(real, 0o755)
	os.WriteFile(filepath.Join(real, "plugin.yml"), []byte("name: traffik\n"), 0o644)

	// What Installer.Install leaves in Root while it works.
	scratch, _ := os.MkdirTemp(root, ".install-*")
	os.WriteFile(filepath.Join(scratch, "plugin.yml"), []byte("name: whatever\n"), 0o644)

	loaded, _ := LoadAll(root)
	if len(loaded) != 1 || loaded[0].Manifest.Name != "traffik" {
		names := []string{}
		for _, p := range loaded {
			names = append(names, p.Manifest.Name)
		}

		t.Fatalf("scratch dir leaked into the list: %v", names)
	}
}
