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
func Set(app string, pairs []string) (map[string]string, error) {
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
