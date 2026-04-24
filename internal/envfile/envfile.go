// Package envfile reads and writes dotenv-style KEY=VALUE files.
package envfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Load reads KEY=VALUE pairs from a file into a map. Missing files return an empty map.
// Blank lines and '#' comment lines are ignored.
func Load(path string) (map[string]string, error) {
	vars := make(map[string]string)

	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return vars, nil
		}

		return nil, fmt.Errorf("read env file %s: %w", path, err)
	}

	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)

		if len(parts) == 2 {
			vars[parts[0]] = parts[1]
		}
	}

	return vars, nil
}

// Save writes the map to the file atomically-ish (write then rename would be safer,
// but we match Gokku's prior behavior here for simplicity).
// File mode is 0600 (env files frequently contain secrets).
func Save(path string, vars map[string]string) error {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	var b strings.Builder

	for _, k := range keys {
		b.WriteString(fmt.Sprintf("%s=%s\n", k, vars[k]))
	}

	// `voodu apply` can fire before any deploy, so /opt/voodu/apps/<app>/shared
	// may not exist yet. MkdirAll instead of expecting the deploy flow
	// to have prepared the tree.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	return os.WriteFile(path, []byte(b.String()), 0600)
}
