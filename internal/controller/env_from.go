package controller

import (
	"fmt"
	"os"
	"strings"

	"go.voodu.clowk.in/internal/paths"
)

// resolveEnvFromRef translates a single `env_from` entry into a
// host filesystem path the docker --env-file flag can consume.
//
// Accepted shapes (all resolve to AppID for paths.AppEnvFile):
//
//	"web"            same scope as the calling resource → AppID(callScope, "web")
//	"prod-1/web"     cross-scope → AppID("prod-1", "web")
//	"clowk-lp-web"   AppID literal — used verbatim
//
// The single-segment shape is the most natural for "I want my
// pair deployment's env" — operator types `web` and voodu fills
// in the scope. Cross-scope is the safety hatch for shared
// secret stores (e.g. `secrets/aws`). The AppID-literal shape is
// for cases where the operator already thinks in voodu's
// internal naming (rare; mostly tests).
//
// Returns the absolute env file path. Does NOT verify the file
// exists — callers handle the missing-file case at run time
// because the referenced resource may be applied AFTER the
// referencing resource (apply order is not enforced).
func resolveEnvFromRef(ref, callerScope string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("env_from ref is empty")
	}

	// Cross-scope shape: "scope/name"
	if i := strings.Index(ref, "/"); i >= 0 {
		scope := strings.TrimSpace(ref[:i])
		name := strings.TrimSpace(ref[i+1:])

		if scope == "" || name == "" {
			return "", fmt.Errorf("env_from ref %q: scope/name expected, got empty side", ref)
		}

		return paths.AppEnvFile(AppID(scope, name)), nil
	}

	// Same-scope shape: bare name. Resolves against the caller's
	// scope. AppID handles the (scope, name) → "<scope>-<name>"
	// formatting consistently with how the deployment / statefulset
	// handlers persist their env files.
	return paths.AppEnvFile(AppID(callerScope, ref)), nil
}

// resolveEnvFromList walks a list of refs and returns the
// corresponding env file paths in declared order. Files that
// don't exist on disk are skipped silently with a warning via
// the supplied logger — better than failing the whole run when
// only one optional source is missing (e.g., the operator
// declared `env_from = ["secrets/aws", "clowk-lp/web"]` but
// secrets/aws hasn't been applied yet on a fresh environment;
// the deployment-paired env still flows through).
//
// At least one MUST exist when env_from is non-empty — if every
// entry is missing the caller errors out with a clear "no env
// sources resolved" message.
func resolveEnvFromList(refs []string, callerScope string, logf func(format string, args ...any)) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	if logf == nil {
		logf = func(string, ...any) {}
	}

	resolved := make([]string, 0, len(refs))

	for _, ref := range refs {
		path, err := resolveEnvFromRef(ref, callerScope)
		if err != nil {
			return nil, err
		}

		if _, err := os.Stat(path); err != nil {
			// File may not exist yet (referenced resource not
			// applied, or deleted). Skip with warning rather
			// than fail — typical in dev where one operator
			// applies pieces of a stack incrementally.
			logf("env_from %q resolves to %s but file does not exist; skipping", ref, path)

			continue
		}

		resolved = append(resolved, path)
	}

	if len(resolved) == 0 {
		return nil, fmt.Errorf("env_from %v: no env files resolved (referenced resources may not be applied yet)", refs)
	}

	return resolved, nil
}
