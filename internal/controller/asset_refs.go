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

// assetRefPattern matches BOTH 3-segment and 4-segment forms in
// a single regex via an optional scope group:
//
//   ${asset.NAME.KEY}              → unscoped lookup (scope="")
//                                    Matches a 1-label
//                                    `asset "name" { … }`.
//   ${asset.SCOPE.NAME.KEY}        → scoped lookup at (SCOPE,
//                                    NAME). Matches a 2-label
//                                    `asset "scope" "name" { … }`.
//
// The first capture group is the OPTIONAL scope; group 2 is
// always name; group 3 is always key. When the scope group is
// empty, the regex matched the 3-segment shape.
//
// Identifier rules match validAssetKey: alphanumeric +
// underscore + hyphen. NAME and SCOPE also take hyphens.
var assetRefPattern = regexp.MustCompile(
	`\$\{asset\.(?:([A-Za-z0-9][A-Za-z0-9_-]*)\.)?([A-Za-z0-9][A-Za-z0-9_-]*)\.([A-Za-z0-9][A-Za-z0-9_-]*)\}`,
)

// AssetPathLookup resolves one ${asset.…} reference into the
// on-disk path the asset handler materialised. Scope is
// passed explicitly per call (never implicit) — the value is
// "" for unscoped (3-segment) refs and the operator-supplied
// label for scoped (4-segment) refs.
type AssetPathLookup func(scope, name, key string) (string, bool)

// makeAssetPathLookup returns a closure that resolves
// ${asset.…} against a known asset manifest registry. We
// check that the asset manifest exists in /desired (catches
// typos) and return the convention-derived path; we don't
// read /status here because the materialisation contract is
// "if the asset is declared, the handler will eventually put
// it on disk" — the resource's spec hash folds in the asset
// digest, so drift triggers a re-roll later.
//
// The lookup returns false only when the asset doesn't
// exist in the store — that's a deliberate authoring error
// the caller surfaces as "unresolved reference".
func makeAssetPathLookup(ctx context.Context, store Store) AssetPathLookup {
	return func(scope, name, key string) (string, bool) {
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

// InterpolateAssetRefs expands every ${asset.…} pattern in
// src. The regex captures scope (optional), name, key in one
// pass — both 3-segment (unscoped) and 4-segment (scoped)
// forms route through the same lookup with scope="" or the
// matched scope respectively.
//
// Unknown references become errors, not empty strings —
// matches the InterpolateRefs posture for ${ref.…}.
func InterpolateAssetRefs(src string, lookup AssetPathLookup) (string, error) {
	var missing []string

	out := assetRefPattern.ReplaceAllStringFunc(src, func(match string) string {
		parts := assetRefPattern.FindStringSubmatch(match)
		scope, name, key := parts[1], parts[2], parts[3]

		if v, ok := lookup(scope, name, key); ok {
			return v
		}

		missing = append(missing, formatAssetRef(scope, name, key))

		return match
	})

	if len(missing) > 0 {
		return "", fmt.Errorf("unresolved asset reference(s): %s", strings.Join(missing, ", "))
	}

	return out, nil
}

// formatAssetRef renders an (scope, name, key) triple in the
// shape that produced it — 3-segment when scope is empty,
// 4-segment otherwise. Used in error messages so the
// operator sees the typo in the same shape they typed.
func formatAssetRef(scope, name, key string) string {
	if scope == "" {
		return name + "." + key
	}

	return scope + "." + name + "." + key
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

// assetRef is one (scope, name, key) triple extracted from a
// string by walking the asset reference pattern. Scope is
// empty for unscoped (3-segment) refs and the operator-typed
// scope for scoped (4-segment) refs. Used by the hash
// machinery to discover which assets a resource depends on
// without resolving the references — the spec is hashed
// BEFORE resolution so the literal `${asset.…}` text persists
// in the hash, and the digest sidecar (asset bundle digest)
// fold-in handles content drift.
type assetRef struct {
	Scope string
	Name  string
	Key   string
}

// collectAssetRefs scans every string field of the given
// values for `${asset.…}` patterns and returns the flat
// list of (scope, name, key) triples. Both 3-segment and
// 4-segment forms are matched by the same regex; scope is
// empty for the 3-segment form. Duplicates are kept — the
// digest lookup dedupes downstream.
func collectAssetRefs(values ...string) []assetRef {
	var out []assetRef

	for _, s := range values {
		for _, m := range assetRefPattern.FindAllStringSubmatch(s, -1) {
			out = append(out, assetRef{Scope: m[1], Name: m[2], Key: m[3]})
		}
	}

	return out
}

// resolveStampedOrLookup returns the asset digest map to fold
// into a consumer's spec hash. Prefers the apply-time-stamped
// digests (set by StampAssetDigests on the consumer's spec
// before /desired persist); falls back to a /status lookup for
// legacy manifests applied before stamping was wired up.
//
// Both paths produce the same map shape (formatted ref →
// sha256), so the hash function downstream is agnostic to the
// source. The fallback exists so a controller upgrade doesn't
// invalidate every running consumer's hash on first reconcile —
// pre-stamping manifests keep their /status-based hash until
// re-applied through the new pipeline.
//
// `fallback` is a thunk so we don't pay the /status lookup when
// stamped digests are present (the fast path).
func resolveStampedOrLookup(stamped map[string]string, fallback func() map[string]string) map[string]string {
	if len(stamped) > 0 {
		return stamped
	}

	return fallback()
}

// LookupAssetDigests resolves a slice of (scope, name, key)
// refs against /status/assets/<scope>-<name> and returns a
// flat map keyed by the formatted ref (`<name>.<key>` for
// unscoped, `<scope>.<name>.<key>` for scoped) → sha256.
// Missing assets or missing keys map to empty strings
// (caller folds that into the spec hash anyway — operator
// gets a deterministic "asset not yet materialised" hash
// that flips once the asset reconciles).
//
// Reads /status once per unique (scope, name) pair so the
// cost stays flat regardless of how many keys the resource
// references.
func LookupAssetDigests(ctx context.Context, store Store, refs []assetRef) map[string]string {
	if len(refs) == 0 || store == nil {
		return nil
	}

	type scopeName struct {
		scope string
		name  string
	}

	byPair := make(map[scopeName][]string, len(refs))

	for _, r := range refs {
		key := scopeName{r.Scope, r.Name}
		byPair[key] = append(byPair[key], r.Key)
	}

	out := make(map[string]string, len(refs))

	for sn, keys := range byPair {
		raw, err := store.GetStatus(ctx, KindAsset, AppID(sn.scope, sn.name))
		if err != nil || raw == nil {
			// Asset not yet reconciled — record empty
			// digests for every requested key so the spec
			// hash carries the "not ready" state, and the
			// next reconcile (after asset materialises)
			// produces a different hash → rolling restart
			// picks up the real content.
			for _, k := range keys {
				out[formatAssetRef(sn.scope, sn.name, k)] = ""
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
			out[formatAssetRef(sn.scope, sn.name, k)] = st.Files[k]
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
