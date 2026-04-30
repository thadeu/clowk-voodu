package manifest

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// varPattern matches ${NAME} or ${NAME:-default}. Only env-var style is
// supported in M4; the full cty-based HCL expression language is
// overkill until we actually need it.
var varPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// serverSideRefPattern matches `${name.path...}` shapes that the CLI
// must NOT resolve at parse time — they're reserved for server-side
// expansion (currently `${ref.<kind>.<name>.<field>}` and
// `${asset.<name>.<key>}`). Distinguished from plain env vars by the
// presence of at least one dot inside the braces.
var serverSideRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*\.[^}]+)\}`)


// Interpolate substitutes ${VAR} references from the supplied map (env
// takes precedence; os.Environ() fills the rest). Unknown vars with no
// default return an error naming the missing variable, so a typo doesn't
// silently produce empty strings in manifests.
func Interpolate(src string, env map[string]string) (string, error) {
	var missing []string

	out := varPattern.ReplaceAllStringFunc(src, func(match string) string {
		parts := varPattern.FindStringSubmatch(match)
		name, def := parts[1], parts[2]

		if v, ok := env[name]; ok {
			return v
		}

		if v, ok := os.LookupEnv(name); ok {
			return v
		}

		if def != "" || strings.Contains(match, ":-") {
			return def
		}

		missing = append(missing, name)

		return match
	})

	if len(missing) > 0 {
		return "", fmt.Errorf("undefined variable(s): %s", strings.Join(missing, ", "))
	}

	return out, nil
}

// escapeServerSideRefsForHCL prefixes every `${name.path…}` pattern
// (server-side refs — `${ref.…}` and `${asset.…}`) with an extra `$`
// so HCL2's template engine treats the whole token as a literal
// `${…}` in the resulting string. The escape only matters for HCL
// because that's the only format whose parser actively evaluates
// `${expr}` template tokens; YAML and JSON pass strings through
// verbatim and don't need the dance.
//
// HCL's escape syntax: `$${foo.bar}` in source produces the literal
// `${foo.bar}` in the resulting string at parse time. Server-side
// interpolation (handlers.go's resolveAppEnv, asset_refs.go's
// InterpolateAssetRefs) then expands those tokens at reconcile time.
//
// Plain `${VAR}` env interpolation already happened in Interpolate
// — what's left after that pass is the server-side refs only, so a
// blanket escape here is safe.
func escapeServerSideRefsForHCL(src string) string {
	return serverSideRefPattern.ReplaceAllStringFunc(src, func(match string) string {
		return "$" + match
	})
}

