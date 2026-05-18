// Package main: client-side env_file loader.
//
// docker-compose-shaped `env_file = [...]` on deployment/statefulset/
// job/cronjob blocks names local .env files whose KEY=value lines get
// merged into the spec's env map at apply time. Everything happens on
// the operator's machine — the controller never sees env_file as a
// field; only the merged env map crosses the wire.
//
// Why CLIENT-side:
//
//   - The controller has no filesystem access to the operator's repo.
//     `env_file = "./apps/foo/.env"` is meaningful only relative to
//     where the operator ran `vd apply`.
//
//   - Secrets in `.env` stay on the operator's machine in their
//     authoring form (a file the team can gitignore) AND get baked
//     into the manifest spec so the server can write them to the
//     pod's env file. Same single source of truth, no double-spec.
//
//   - Matches docker-compose semantics exactly: env_file values
//     come BEFORE inline `environment:` values, with inline winning
//     on key collision.
//
// Precedence (last write wins, then operator-inline wins):
//
//   1. env_file values, in declared order (later files override earlier)
//   2. inline `env = {...}` block values
//
// So if `.env` declares FOO=from-file and HCL declares FOO=inline,
// FOO=inline wins. Operators thinking in terms of "env_file is a
// default I sometimes override inline" get exactly that.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.voodu.clowk.in/internal/controller"
)

// envFileKinds is the set of manifest kinds that carry an `env_file`
// field. Ingress / asset / etc. don't have one and are skipped.
var envFileKinds = map[controller.Kind]bool{
	controller.KindDeployment:  true,
	controller.KindStatefulset: true,
	controller.KindJob:         true,
	controller.KindCronJob:     true,
}

// mergeEnvFilesInManifests walks every manifest, materialises any
// declared `env_file` entries into the spec's `env` map (client-side),
// and strips the `env_file` field from the serialised spec so the
// controller wire stays free of paths that are only meaningful on the
// operator's machine.
//
// baseDir is the directory env_file paths resolve against. For files
// loaded by path, this is the manifest file's directory (matches
// docker-compose's "paths are relative to the compose file" rule). For
// stdin/json input, the caller passes the current working directory.
//
// Empty/missing env_file is a no-op — the manifest passes through
// unchanged. Read errors are surfaced verbatim so the operator sees
// "open ./apps/foo/.env: no such file or directory" instead of a
// generic "apply failed".
func mergeEnvFilesInManifests(mans []controller.Manifest, baseDir string) ([]controller.Manifest, error) {
	for i, m := range mans {
		if !envFileKinds[m.Kind] {
			continue
		}

		updated, err := mergeEnvFilesInManifest(m, baseDir)
		if err != nil {
			return nil, fmt.Errorf("%s/%s/%s: %w", m.Kind, m.Scope, m.Name, err)
		}

		mans[i] = updated
	}

	return mans, nil
}

// mergeEnvFilesInManifest handles one manifest. Spec is opaque JSON
// (the parser produces it via encode → json.Marshal of the typed
// struct); we round-trip it through a generic map so we can mutate
// env_file/env without re-binding the per-kind type.
//
// CronJob is the one case where env_file lives inside a `job` sub-
// object on the wire — the rest are flat. The helper handles both
// shapes.
func mergeEnvFilesInManifest(m controller.Manifest, baseDir string) (controller.Manifest, error) {
	var spec map[string]any

	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return m, fmt.Errorf("decode spec: %w", err)
	}

	target := spec

	if m.Kind == controller.KindCronJob {
		jobAny, ok := spec["job"]
		if !ok {
			return m, nil
		}

		jobMap, ok := jobAny.(map[string]any)
		if !ok {
			return m, nil
		}

		target = jobMap
	}

	files, ok := stringSliceFromAny(target["env_file"])
	if !ok || len(files) == 0 {
		return m, nil
	}

	merged, err := loadEnvFiles(files, baseDir)
	if err != nil {
		return m, err
	}

	if len(merged) > 0 {
		inline := stringMapFromAny(target["env"])
		for k, v := range inline {
			// inline-wins on key collision. Operators who declared both
			// `env_file = "..."` and `env = { FOO = "explicit" }` get
			// FOO=explicit regardless of what the file says.
			merged[k] = v
		}

		target["env"] = merged
	}

	// Strip env_file from the wire form — it's a CLI concern, not a
	// controller concern. The controller never reads this field; leaving
	// it in would just bloat the manifest JSON.
	delete(target, "env_file")

	if m.Kind == controller.KindCronJob {
		spec["job"] = target
	}

	out, err := json.Marshal(spec)
	if err != nil {
		return m, fmt.Errorf("re-encode spec: %w", err)
	}

	m.Spec = out

	return m, nil
}

// loadEnvFiles walks the declared paths in order, reads each file, and
// returns a single merged map. Later files override earlier ones on key
// collision (matches docker-compose's "list order = layer order, last
// wins"). Missing files return a hard error — the alternative
// (silently skip) would mask typos and the resulting empty env value
// would only surface at runtime, much further from the operator's
// edit.
func loadEnvFiles(files []string, baseDir string) (map[string]string, error) {
	out := map[string]string{}

	for _, raw := range files {
		path := raw
		if !filepath.IsAbs(path) {
			path = filepath.Join(baseDir, path)
		}

		kv, err := parseEnvFile(path)
		if err != nil {
			return nil, err
		}

		for k, v := range kv {
			out[k] = v
		}
	}

	return out, nil
}

// parseEnvFile is a minimal .env parser — KEY=value lines, # comments,
// blank lines ignored. Values may be wrapped in matching single or
// double quotes (outer quotes stripped, content kept verbatim). No
// shell interpolation, no $VAR expansion, no `export` prefix
// handling — keeps the contract small and the surprise surface
// shallow. If operators need any of that they can write the values
// inline.
//
// Whitespace handling matches the docker-compose convention:
// surrounding whitespace on keys and unquoted values is trimmed;
// whitespace INSIDE quoted values is preserved.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer f.Close()

	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	lineNo := 0

	for scanner.Scan() {
		lineNo++

		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Tolerate `export KEY=val` for shell-shaped .env files. The
		// `export` is shell syntax; the underlying KEY=val carries the
		// data either way.
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimPrefix(line, "export ")
			line = strings.TrimSpace(line)
		}

		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("%s:%d: missing '=' (line: %q)", path, lineNo, line)
		}

		key := strings.TrimSpace(line[:eq])
		val := line[eq+1:]

		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, lineNo)
		}

		out[key] = unquoteEnvValue(val)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return out, nil
}

// unquoteEnvValue strips matching outer quotes when present, otherwise
// trims surrounding whitespace. Mirrors docker-compose's behaviour: a
// quoted value preserves whitespace inside the quotes, an unquoted one
// has its surrounding whitespace stripped.
//
// Order of operations matters: trim the whole value first (because the
// quotes may be surrounded by whitespace like `KEY = "..."`), then
// detect matching outer quotes on the trimmed view, then either strip
// the quotes (preserving interior whitespace) or hand back the trimmed
// unquoted value.
func unquoteEnvValue(v string) string {
	v = strings.TrimSpace(v)

	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}

	return v
}

// stringSliceFromAny extracts a []string from the loosely-typed JSON
// shape (json.Unmarshal of `[]string` lands as `[]any` of strings).
// Returns ok=false for anything else so the caller treats it as
// "no env_file declared" rather than erroring on a malformed value
// — the parser already validated the typed shape; this is just
// defensive plumbing.
func stringSliceFromAny(v any) ([]string, bool) {
	if v == nil {
		return nil, false
	}

	raw, ok := v.([]any)
	if !ok {
		return nil, false
	}

	out := make([]string, 0, len(raw))

	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}

		out = append(out, s)
	}

	return out, true
}

// stringMapFromAny extracts a map[string]string from the loosely-typed
// JSON shape. Returns an empty map (not nil) so callers can range over
// it without nil-checking — the only consumer here merges keys, and
// "no inline env" is functionally identical to "empty inline env".
func stringMapFromAny(v any) map[string]string {
	out := map[string]string{}

	if v == nil {
		return out
	}

	raw, ok := v.(map[string]any)
	if !ok {
		return out
	}

	for k, item := range raw {
		s, ok := item.(string)
		if !ok {
			continue
		}

		out[k] = s
	}

	return out
}
