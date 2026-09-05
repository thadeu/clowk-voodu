// Package triggerspec parses the deploy trigger files a repository declares in
// `.voodu/**/*.yml`.
//
// WHY THE CONFIG LIVES IN THE REPOSITORY. A control plane that stored this
// would be a control plane that could change it — pointing a deploy at another
// branch, widening the paths that fire it. In the repository it travels with
// the code, gets reviewed in a pull request, and changing it means landing a
// commit on the branch the operator pinned. A stolen deploy token cannot do
// that. Same shape as GitHub Actions, and not by coincidence: the placement is
// what carries the property, not the syntax.
//
// WHAT IS DELIBERATELY ABSENT is as load-bearing as what is here:
//
//   - No `jobs`, `steps` or `runs-on`. This is not a runner. The file says
//     WHEN to deploy and WHICH manifest to apply; `vd apply` does the rest,
//     which is why it is a fraction the size of a workflow.
//   - No `scope`. In voodu the scope is the manifest's own first label
//     (`statefulset "runa" "pg"`), so a scope here would be a second source
//     for one fact — and validating THAT against what the box allows would be
//     checking a declaration beside the manifest instead of the manifest. The
//     box parses what it is about to apply and validates those scopes.
package triggerspec

import (
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Dir is the directory a repository declares its triggers in.
const Dir = ".voodu"

// Spec is one parsed trigger file.
//
// EVERY FIELD CARRIES BOTH TAGS. The yaml one reads the customer's file; the
// json one is what `GET /deploy/manifests` puts on the wire, and the console
// renders it back as YAML beside the verdict. Without the json tags Go would
// emit its own field names — `On`, `Push`, `Branches` — and the screen would
// show a customer their own file spelled in a vocabulary that does not exist
// in it.
type Spec struct {
	// Name labels the deploy on screen and in the activity trail. Defaults to
	// the file's base name when absent, so a file is never nameless.
	Name string `yaml:"name" json:"name"`

	On    On    `yaml:"on" json:"on"`
	Apply Apply `yaml:"apply" json:"apply"`
}

// On is when a deploy fires. `push` is the only event today; the nesting
// exists so a second one does not have to break every file already committed.
type On struct {
	Push Push `yaml:"push" json:"push"`
}

// Push mirrors the GitHub Actions field names exactly. Somebody who has
// written a workflow already knows this, and a near-miss vocabulary is worse
// than an unfamiliar one — it invites muscle memory to be wrong.
type Push struct {
	Branches []string `yaml:"branches" json:"branches,omitempty"`
	Tags     []string `yaml:"tags" json:"tags,omitempty"`
	Paths    []string `yaml:"paths" json:"paths,omitempty"`
}

// Apply is what to hand `vd apply`.
//
// A mapping with one field on purpose: `prune` and `force` belong here the day
// they are needed, and starting with a bare string would mean turning
// `apply: file.hcl` into a mapping later — breaking every file already
// committed.
type Apply struct {
	File string `yaml:"file" json:"file"`
}

// ErrInvalid marks a file the box refuses, so the caller can report it against
// that one file and keep the others.
type ErrInvalid struct {
	Path   string
	Reason string
}

func (e ErrInvalid) Error() string { return fmt.Sprintf("%s: %s", e.Path, e.Reason) }

// Parse reads one trigger file.
//
// `filePath` is the repository-relative path, used for the default name and
// for error messages — a reader fixing three broken files needs to know which
// one each complaint is about.
func Parse(filePath string, raw []byte) (Spec, error) {
	var spec Spec

	// KnownFields so a typo is an error rather than silence. `branch:` instead
	// of `branches:` would otherwise parse into nothing and produce a trigger
	// that never fires, which is the worst way to learn about a typo.
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)

	if err := dec.Decode(&spec); err != nil {
		return Spec{}, ErrInvalid{filePath, "not valid YAML: " + err.Error()}
	}

	if spec.Name == "" {
		spec.Name = defaultName(filePath)
	}

	if err := spec.validate(filePath); err != nil {
		return Spec{}, err
	}

	return spec, nil
}

func (s Spec) validate(filePath string) error {
	if strings.TrimSpace(s.Apply.File) == "" {
		return ErrInvalid{filePath, "apply.file is required — it names the manifest to apply"}
	}

	if err := validateRepoPath(filePath, s.Apply.File); err != nil {
		return err
	}

	if len(s.On.Push.Branches) == 0 && len(s.On.Push.Tags) == 0 {
		return ErrInvalid{filePath,
			"on.push needs branches or tags — a trigger that matches nothing never fires, " +
				"and a file that never fires is more confusing than a missing one"}
	}

	return nil
}

// validateRepoPath refuses anything that escapes the repository.
//
// `apply.file` is joined onto an extracted tarball and opened. A `../` or an
// absolute path would read a file outside the release directory — on a box
// that also holds every other app's releases and the config buckets.
func validateRepoPath(filePath, target string) error {
	target = strings.TrimSpace(target)

	if strings.HasPrefix(target, "/") {
		return ErrInvalid{filePath, fmt.Sprintf("apply.file %q must be relative to the repository root", target)}
	}

	if path.Clean(target) != target && path.Clean(target) != strings.TrimPrefix(target, "./") {
		return ErrInvalid{filePath, fmt.Sprintf("apply.file %q must be a plain path", target)}
	}

	cleaned := path.Clean(target)

	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ErrInvalid{filePath, fmt.Sprintf("apply.file %q escapes the repository", target)}
	}

	return nil
}

// defaultName is the file's base name without extension: `.voodu/deploy/pwa.yml`
// becomes `pwa`. A file always has a name, so the screen never has a blank row.
func defaultName(filePath string) string {
	base := path.Base(filePath)

	return strings.TrimSuffix(base, path.Ext(base))
}

// IsTriggerFile reports whether a repository path is one of ours.
//
// Under `.voodu/`, at any depth, ending in .yml or .yaml. The manifests
// themselves live there too (`.voodu/pwa.hcl`) and must NOT be mistaken for
// triggers — the extension is what separates them.
func IsTriggerFile(repoPath string) bool {
	cleaned := path.Clean(repoPath)

	if !strings.HasPrefix(cleaned, Dir+"/") {
		return false
	}

	ext := strings.ToLower(path.Ext(cleaned))

	return ext == ".yml" || ext == ".yaml"
}

// MatchesRef reports whether this spec fires for a pushed ref.
//
// `ref` is the full git ref — "refs/heads/main" or "refs/tags/v1.2.3". Taking
// the full ref rather than a bare name is what keeps a branch named `v1.0`
// from matching a tag pattern.
func (s Spec) MatchesRef(ref string) bool {
	if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return matchAny(s.On.Push.Branches, name)
	}

	if name, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		return matchAny(s.On.Push.Tags, name)
	}

	return false
}

// MatchesPaths reports whether a push touching these files fires this spec.
//
// No `paths` means every push of a matching ref fires — the same default
// Actions uses, and the one somebody writing their first file expects.
func (s Spec) MatchesPaths(changed []string) bool {
	if len(s.On.Push.Paths) == 0 {
		return true
	}

	for _, file := range changed {
		for _, pattern := range s.On.Push.Paths {
			if matchPath(pattern, file) {
				return true
			}
		}
	}

	return false
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}

	return false
}

// matchPath supports the `**` that Actions users expect and path.Match does
// not: `apps/pwa/**` has to match `apps/pwa/src/index.ts`, which a single
// star will not do because it never crosses a separator.
func matchPath(pattern, file string) bool {
	if idx := strings.Index(pattern, "**"); idx >= 0 {
		prefix := pattern[:idx]
		suffix := strings.TrimPrefix(pattern[idx+2:], "/")

		if !strings.HasPrefix(file, prefix) {
			return false
		}

		if suffix == "" {
			return true
		}

		rest := strings.TrimPrefix(file, prefix)

		return matchAny([]string{suffix}, path.Base(rest)) || strings.HasSuffix(rest, suffix)
	}

	ok, err := path.Match(pattern, file)

	return err == nil && ok
}
