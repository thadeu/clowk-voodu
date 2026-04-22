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

