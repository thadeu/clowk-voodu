// Package secrets manages per-app environment variables stored in the
// app's shared/.env file. It provides pure logic — CLI presentation
// (stdout vs JSON) lives in the cmd layer.
package secrets

import (
	"fmt"
	"sort"
	"strings"

	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/envfile"
	"go.voodu.clowk.in/internal/paths"
)

// Set merges the given KEY=VALUE pairs into the app's .env file.
// Returns the parsed map that was written.
//
// Calls paths.EnsureAppLayout first so every code path that writes
// env (deployment apply, job run, cronjob tick, vd config set) gets
// the per-app filesystem tree (apps/<app>/{releases,shared} and
// volumes/<app>/) materialised before the env file lands. Without
// this, image-mode deployments that declare `volumes = [...]` in
// HCL would let docker create the host volume path at container-
// start time with daemon-default ownership (root:root), and apps
// inside the container would trip "permission denied" on writes.
func Set(app string, pairs []string) (map[string]string, error) {
	if err := paths.EnsureAppLayout(app); err != nil {
		return nil, fmt.Errorf("ensure app layout: %w", err)
	}

	envFile := paths.AppEnvFile(app)

	vars, err := envfile.Load(envFile)
	if err != nil {
		return nil, err
	}

	for _, pair := range pairs {
		parts := strings.SplitN(pair, "=", 2)

		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid format %q, expected KEY=VALUE", pair)
		}

		vars[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}

	if err := envfile.Save(envFile, vars); err != nil {
		return nil, err
	}

	return vars, nil
}

// Get returns the value of a single key. Returns an error when the key
// is not set.
func Get(app, key string) (string, error) {
	vars, err := envfile.Load(paths.AppEnvFile(app))
	if err != nil {
		return "", err
	}

	v, ok := vars[key]
	if !ok {
		return "", fmt.Errorf("variable %q not found", key)
	}

	return v, nil
}

// List returns all env vars for the app, sorted by key.
// The returned slice preserves insertion (sorted) order for stable output.
func List(app string) ([]string, map[string]string, error) {
	vars, err := envfile.Load(paths.AppEnvFile(app))
	if err != nil {
		return nil, nil, err
	}

	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys, vars, nil
}

// Replace overwrites the app's .env file with the given pairs.
// Unlike Set (which OVERLAYS — preserves existing keys not in
// the pairs slice), Replace treats the pairs argument as the
// COMPLETE desired state: keys present in the file but absent
// from pairs get removed.
//
// Used by reconciler paths (DeploymentHandler.linkEnv and the
// statefulset/job/cronjob equivalents) where the pairs come
// from a fresh merge of (config bucket + spec.env). The merge
// IS the source of truth — the file should mirror it exactly,
// not accumulate forever.
//
// Set vs Replace cheat sheet:
//
//   - `vd config set FOO=bar` → Set semantics: keep existing
//     keys, add/update FOO. Operator's mental model is "I'm
//     adding one var".
//
//   - reconciler watch event after `vd config unset` →
//     Replace semantics: rewrite .env from the (now-shorter)
//     merged state. Without this, removed keys persist forever
//     in the .env file even though the config bucket says
//     they're gone.
func Replace(app string, pairs []string) (map[string]string, error) {
	if err := paths.EnsureAppLayout(app); err != nil {
		return nil, fmt.Errorf("ensure app layout: %w", err)
	}

	envFile := paths.AppEnvFile(app)

	vars := make(map[string]string, len(pairs))

	for _, pair := range pairs {
		parts := strings.SplitN(pair, "=", 2)

		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid format %q, expected KEY=VALUE", pair)
		}

		vars[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}

	if err := envfile.Save(envFile, vars); err != nil {
		return nil, err
	}

	return vars, nil
}

// Unset removes the given keys from the app's .env file. Missing keys
// are silently ignored.
func Unset(app string, keys []string) error {
	envFile := paths.AppEnvFile(app)

	vars, err := envfile.Load(envFile)
	if err != nil {
		return err
	}

	for _, k := range keys {
		delete(vars, k)
	}

	return envfile.Save(envFile, vars)
}

// Reload recreates the currently-active container with the updated env
// file. Kept for compatibility; prefer a full redeploy in production.
func Reload(app string) error {
	return docker.RecreateActiveContainer(app, paths.AppEnvFile(app), paths.AppCurrentLink(app))
}
