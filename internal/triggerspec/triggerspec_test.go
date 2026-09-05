package triggerspec

import (
	"encoding/json"
	"strings"
	"testing"
)

const minimal = `
name: PWA
on:
  push:
    branches: [main]
apply:
  file: .voodu/pwa.hcl
`

func TestParseMinimal(t *testing.T) {
	spec, err := Parse(".voodu/deploy/pwa.yml", []byte(minimal))
	if err != nil {
		t.Fatal(err)
	}

	if spec.Name != "PWA" {
		t.Errorf("name = %q", spec.Name)
	}

	if spec.Apply.File != ".voodu/pwa.hcl" {
		t.Errorf("apply.file = %q", spec.Apply.File)
	}

	if len(spec.On.Push.Branches) != 1 || spec.On.Push.Branches[0] != "main" {
		t.Errorf("branches = %v", spec.On.Push.Branches)
	}
}

// A file always has a name, so the screen never renders a blank row.
func TestNameDefaultsToTheFileName(t *testing.T) {
	spec, err := Parse(".voodu/deploy/api.yml", []byte(`
on:
  push:
    branches: [main]
apply:
  file: voodu.hcl
`))
	if err != nil {
		t.Fatal(err)
	}

	if spec.Name != "api" {
		t.Errorf("name = %q, want the file's base name", spec.Name)
	}
}

// `branch:` instead of `branches:` would otherwise parse into nothing and
// produce a trigger that never fires — the worst way to learn about a typo.
func TestUnknownFieldIsRefused(t *testing.T) {
	_, err := Parse(".voodu/x.yml", []byte(`
on:
  push:
    branch: [main]
apply:
  file: voodu.hcl
`))

	if err == nil {
		t.Fatal("a misspelled field must be refused, not ignored")
	}

	if !strings.Contains(err.Error(), ".voodu/x.yml") {
		t.Errorf("the error must name the file: %v", err)
	}
}

func TestValidationRefusals(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"no apply.file", "on:\n  push:\n    branches: [main]\n"},
		{"blank apply.file", "on:\n  push:\n    branches: [main]\napply:\n  file: '   '\n"},
		// A trigger matching nothing never fires, which is more confusing
		// than a missing file.
		{"no branches or tags", "on:\n  push:\n    paths: ['**']\napply:\n  file: voodu.hcl\n"},

		// apply.file is joined onto an extracted tarball and opened, on a box
		// that also holds every other app's releases.
		{"absolute path", "on:\n  push:\n    branches: [main]\napply:\n  file: /etc/passwd\n"},
		{"escapes the repo", "on:\n  push:\n    branches: [main]\napply:\n  file: ../../etc/passwd\n"},

		{"not yaml", "{{{"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse(".voodu/x.yml", []byte(c.yaml)); err == nil {
				t.Fatal("must be refused")
			}
		})
	}
}

// The manifests live in `.voodu/` too. The extension is what separates a
// trigger from the thing it applies.
func TestIsTriggerFile(t *testing.T) {
	yes := []string{".voodu/pwa.yml", ".voodu/deploy/api.yaml", ".voodu/a/b/c.yml"}
	no := []string{
		".voodu/pwa.hcl",   // the manifest, not a trigger
		".voodu",           // the directory itself
		"voodu.yml",        // outside the directory
		".github/ci.yml",   // somebody else's
		"src/.voodu/x.yml", // nested elsewhere, not the repo root
	}

	for _, p := range yes {
		if !IsTriggerFile(p) {
			t.Errorf("%q should be a trigger file", p)
		}
	}

	for _, p := range no {
		if IsTriggerFile(p) {
			t.Errorf("%q should NOT be a trigger file", p)
		}
	}
}

// Taking the full ref is what keeps a branch named `v1.0` from matching a tag
// pattern.
func TestMatchesRef(t *testing.T) {
	spec := Spec{On: On{Push: Push{Branches: []string{"main", "release-*"}, Tags: []string{"v*"}}}}

	match := []string{"refs/heads/main", "refs/heads/release-2", "refs/tags/v1.2.3"}
	miss := []string{"refs/heads/feature", "refs/tags/nightly", "refs/heads/v1.0", "refs/pull/3/head"}

	for _, ref := range match {
		if !spec.MatchesRef(ref) {
			t.Errorf("%q should fire", ref)
		}
	}

	for _, ref := range miss {
		if spec.MatchesRef(ref) {
			t.Errorf("%q should NOT fire", ref)
		}
	}
}

// No `paths` means every push of a matching ref fires — the default Actions
// uses, and the one somebody writing their first file expects.
func TestMatchesPathsDefaultsToEverything(t *testing.T) {
	spec := Spec{}

	if !spec.MatchesPaths([]string{"anything.txt"}) {
		t.Error("an absent paths list must match everything")
	}

	if !spec.MatchesPaths(nil) {
		t.Error("an absent paths list must match even an empty change set")
	}
}

// `apps/pwa/**` has to reach `apps/pwa/src/index.ts`; a single star never
// crosses a separator, which is why path.Match alone is not enough.
func TestMatchesPathsWithDoubleStar(t *testing.T) {
	spec := Spec{On: On{Push: Push{Paths: []string{"apps/pwa/**"}}}}

	if !spec.MatchesPaths([]string{"apps/pwa/src/index.ts"}) {
		t.Error("** must cross separators")
	}

	if !spec.MatchesPaths([]string{"README.md", "apps/pwa/main.go"}) {
		t.Error("one matching file in the change set is enough")
	}

	if spec.MatchesPaths([]string{"apps/api/main.go"}) {
		t.Error("a different directory must not match")
	}
}

// The console reads this shape off `GET /deploy/manifests` and renders it back
// to the customer as YAML beside the verdict. Go's default marshalling would
// emit `On`, `Push`, `Branches` — the customer's own file spelled in a
// vocabulary that appears nowhere in it. Pinned here rather than left to the
// screen to notice.
func TestSpecMarshalsWithTheFieldNamesTheFileUses(t *testing.T) {
	spec := Spec{
		Name:  "PWA",
		On:    On{Push: Push{Branches: []string{"main"}, Paths: []string{"app/**"}}},
		Apply: Apply{File: "voodu.hcl"},
	}

	out, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := string(out)

	for _, key := range []string{`"name"`, `"on"`, `"push"`, `"branches"`, `"paths"`, `"apply"`, `"file"`} {
		if !strings.Contains(got, key) {
			t.Errorf("missing %s in %s", key, got)
		}
	}

	// The Go-cased spellings must NOT appear: a screen that reads them would
	// keep working here and break the day somebody adds a tag.
	for _, key := range []string{`"On"`, `"Push"`, `"Branches"`, `"Apply"`} {
		if strings.Contains(got, key) {
			t.Errorf("unexpected Go-cased %s in %s", key, got)
		}
	}

	// Empty lists are omitted, not rendered as null: the screen shows a
	// customer their file, and `tags: null` is not in it.
	if strings.Contains(got, `"tags"`) {
		t.Errorf("empty tags should be omitted: %s", got)
	}
}
