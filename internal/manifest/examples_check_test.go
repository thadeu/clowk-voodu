package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPluginExampleManifestsParse guards the manifests shipped in the
// traffik plugin's examples/ directory.
//
// An example that does not parse is worse than no example: it is copied
// before it is run, and the operator debugs their own typo instead of
// the one they inherited. Skipped when the plugin repo is not checked
// out beside this one.
func TestPluginExampleManifestsParse(t *testing.T) {
	home, err := os.UserHomeDir()

	if err != nil {
		t.Skip("no home dir")
	}

	dir := filepath.Join(home, "code", "voodu-traffik", "examples")

	files, err := filepath.Glob(filepath.Join(dir, "*.voodu"))

	if err != nil || len(files) == 0 {
		t.Skip("voodu-traffik/examples not checked out beside this repo")
	}

	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			mans, err := ParseFile(f, map[string]string{})

			if err != nil {
				t.Fatalf("%s does not parse: %v", filepath.Base(f), err)
			}

			if len(mans) == 0 {
				t.Fatalf("%s parsed to nothing", filepath.Base(f))
			}

			for _, m := range mans {
				t.Logf("  %s/%s/%s", m.Kind, m.Scope, m.Name)
			}
		})
	}
}
