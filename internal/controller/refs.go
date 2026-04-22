package controller

import (
	"fmt"
	"regexp"
	"strings"
)

// refPattern matches ${ref.KIND.NAME.FIELD} — the reconcile-time lookup
// into a resource's runtime status. Depth is fixed at one field; plugin
// status blobs are flat maps (url, host, port, …), so nested traversal
// would just be complexity nobody needs today.
//
// Kept in this package (not internal/manifest) to avoid an import cycle:
// manifest already imports controller for the wire Manifest type, and
// refs need the status store that only controller owns.
var refPattern = regexp.MustCompile(`\$\{ref\.([a-z]+)\.([A-Za-z0-9][A-Za-z0-9_-]*)\.([A-Za-z][A-Za-z0-9_]*)\}`)

// RefLookup resolves a single ${ref.KIND.NAME.FIELD} reference. Returns
// (value, true) on success or ("", false) when the reference doesn't
// point at anything — the caller decides whether that's an error.
type RefLookup func(kind, name, field string) (string, bool)

// InterpolateRefs expands every ${ref.*} in src. Unknown references
// become errors, not empty strings: a missing DATABASE_URL is the kind
// of bug you want loud.
func InterpolateRefs(src string, lookup RefLookup) (string, error) {
	var missing []string

	out := refPattern.ReplaceAllStringFunc(src, func(match string) string {
		parts := refPattern.FindStringSubmatch(match)
		kind, name, field := parts[1], parts[2], parts[3]

		if v, ok := lookup(kind, name, field); ok {
			return v
		}

		missing = append(missing, fmt.Sprintf("%s.%s.%s", kind, name, field))

		return match
	})

	if len(missing) > 0 {
		return "", fmt.Errorf("unresolved reference(s): %s", strings.Join(missing, ", "))
	}

	return out, nil
}

// InterpolateRefsMap is a convenience for the common case: an env map
// whose values may contain refs. Returns a new map so the caller's
// input isn't mutated (etcd-owned data shouldn't be rewritten
// in-place).
func InterpolateRefsMap(env map[string]string, lookup RefLookup) (map[string]string, error) {
	out := make(map[string]string, len(env))

	for k, v := range env {
		resolved, err := InterpolateRefs(v, lookup)
		if err != nil {
			return nil, fmt.Errorf("env[%s]: %w", k, err)
		}

		out[k] = resolved
	}

	return out, nil
}
