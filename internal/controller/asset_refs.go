package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.voodu.clowk.in/internal/paths"
)

// assetRefPattern matches ${asset.NAME.KEY}. NAME is the
// asset manifest's `name` label; KEY is one entry from its
// body. Scope is implicit — same scope as the resource doing
// the interpolation. Cross-scope references could land later
// as `${asset.SCOPE.NAME.KEY}` (4 segments) but the typical
// "config lives next to the workload" pattern is satisfied
// by 3-segment scoped resolution alone.
//
// Identifier rules match validAssetKey: alphanumeric +
// underscore + hyphen. NAME also takes hyphens (matches the
// rest of the manifest naming).
var assetRefPattern = regexp.MustCompile(`\$\{asset\.([A-Za-z0-9][A-Za-z0-9_-]*)\.([A-Za-z0-9][A-Za-z0-9_-]*)\}`)

// AssetPathLookup resolves one ${asset.NAME.KEY} reference
// into the on-disk path the asset handler materialised. The
// scope is bound at construction time (closure over the
// resource's own scope), so the lookup signature stays
// minimal.
type AssetPathLookup func(name, key string) (string, bool)

// makeAssetPathLookup returns a closure that resolves
// ${asset.NAME.KEY} against a known asset manifest registry.
// The simplest correct implementation just checks that the
// asset manifest exists in /desired (catches typos) and
// returns the convention-derived path; we don't read /status
// here because the materialisation contract is "if the asset
// is declared, the handler will eventually put it on disk" —
// the resource's spec hash folds in the asset digest, so
// drift triggers a re-roll later.
//
// The lookup returns false only when the asset doesn't
// exist in the store — that's a deliberate authoring error
// the caller surfaces as "unresolved reference".
func makeAssetPathLookup(ctx context.Context, store Store, scope string) AssetPathLookup {
	return func(name, key string) (string, bool) {
		if store == nil {
			return "", false
		}

		manifest, err := store.Get(ctx, KindAsset, scope, name)
		if err != nil || manifest == nil {
			return "", false
		}

		// We don't validate the key against the asset's
		// declared file map here — that's the job of M-C4's
		// hash plumbing. Returning the path even for a
		// not-yet-materialised key keeps the reconciler
		// resilient: docker creates the bind mount as a
		// directory if the source doesn't exist, and a
		// retry once the asset reconciles fixes everything.
		return paths.AssetFile(scope, name, key), true
	}
}

// InterpolateAssetRefs expands every ${asset.NAME.KEY} in src.
// Mirror of InterpolateRefs but for the asset namespace.
// Unknown references become errors, not empty strings.
func InterpolateAssetRefs(src string, lookup AssetPathLookup) (string, error) {
	var missing []string

	out := assetRefPattern.ReplaceAllStringFunc(src, func(match string) string {
		parts := assetRefPattern.FindStringSubmatch(match)
		name, key := parts[1], parts[2]

		if v, ok := lookup(name, key); ok {
			return v
		}

		missing = append(missing, fmt.Sprintf("%s.%s", name, key))

		return match
	})

	if len(missing) > 0 {
		return "", fmt.Errorf("unresolved asset reference(s): %s", strings.Join(missing, ", "))
	}

	return out, nil
}

// resolveAssetRefsInSlice rewrites every string element of in
// through the asset lookup. Used for ContainerSpec fields
// like Volumes, Command, Ports — slices of strings where any
// element may carry an `${asset.X.Y}` placeholder. Non-string
// values are passed through untouched (today there aren't
// any, but defensive).
func resolveAssetRefsInSlice(in []string, lookup AssetPathLookup) ([]string, error) {
	if len(in) == 0 {
		return in, nil
	}

	out := make([]string, len(in))

	for i, v := range in {
		resolved, err := InterpolateAssetRefs(v, lookup)
		if err != nil {
			return nil, err
		}

		out[i] = resolved
	}

	return out, nil
}

// resolveAssetRefsInMap rewrites every value of an env map.
// Same posture as InterpolateRefsMap (the ref counterpart):
// returns a new map so etcd-owned data isn't mutated in
// place.
func resolveAssetRefsInMap(in map[string]string, lookup AssetPathLookup) (map[string]string, error) {
	if len(in) == 0 {
		return in, nil
	}

	out := make(map[string]string, len(in))

	for k, v := range in {
		resolved, err := InterpolateAssetRefs(v, lookup)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}

		out[k] = resolved
	}

	return out, nil
}

// assetRef is one (name, key) pair extracted from a string by
// walking the asset reference pattern. Used by the hash
// machinery to discover which assets a resource depends on
// without resolving the references — the spec is hashed BEFORE
// resolution so the literal `${asset.X.Y}` text persists in
// the hash, and the digest sidecar (asset bundle digest)
// fold-in handles content drift.
type assetRef struct {
	Name string
	Key  string
}

// collectAssetRefs scans every string field of the given
// values for `${asset.NAME.KEY}` patterns and returns the
// flat list. Duplicates are kept — the digest lookup
// dedupes downstream.
func collectAssetRefs(values ...string) []assetRef {
	var out []assetRef

	for _, s := range values {
		for _, m := range assetRefPattern.FindAllStringSubmatch(s, -1) {
			out = append(out, assetRef{Name: m[1], Key: m[2]})
		}
	}

	return out
}

// LookupAssetDigests resolves a slice of (name, key) refs
// against /status/assets/<scope>-<name> and returns a flat
// map `<name>.<key> → <sha256-of-content>`. Missing assets
// or missing keys map to empty strings (caller folds that
// into the spec hash anyway — operator gets a deterministic
// "asset not yet materialised" hash that flips once the
// asset reconciles).
//
// Reads /status once per unique asset name (typically just
// one or two per resource) so the cost stays flat regardless
// of how many keys the resource references.
func LookupAssetDigests(ctx context.Context, store Store, scope string, refs []assetRef) map[string]string {
	if len(refs) == 0 || store == nil {
		return nil
	}

	byName := make(map[string][]string, len(refs))

	for _, r := range refs {
		byName[r.Name] = append(byName[r.Name], r.Key)
	}

	out := make(map[string]string, len(refs))

	for name, keys := range byName {
		raw, err := store.GetStatus(ctx, KindAsset, AppID(scope, name))
		if err != nil || raw == nil {
			// Asset not yet reconciled — record empty
			// digests for every requested key so the spec
			// hash carries the "not ready" state, and the
			// next reconcile (after asset materialises)
			// produces a different hash → rolling restart
			// picks up the real content.
			for _, k := range keys {
				out[name+"."+k] = ""
			}

			continue
		}

		var st struct {
			Files map[string]string `json:"files,omitempty"`
		}

		if err := json.Unmarshal(raw, &st); err != nil {
			continue
		}

		for _, k := range keys {
			out[name+"."+k] = st.Files[k]
		}
	}

	return out
}

// flattenAssetDigests sorts the digest map deterministically
// for inclusion in a spec hash. The hash function gets a
// stable []string regardless of map iteration order, so two
// reconciles with the same asset content produce the same
// hash even when Go's map randomisation kicks in.
func flattenAssetDigests(digests map[string]string) []string {
	if len(digests) == 0 {
		return nil
	}

	keys := make([]string, 0, len(digests))
	for k := range digests {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	out := make([]string, 0, len(keys))

	for _, k := range keys {
		out = append(out, k+"="+digests[k])
	}

	return out
}
