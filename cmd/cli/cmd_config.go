package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// newConfigCmd manages env vars stored in etcd via the controller's
// /config endpoint. Shape mirrors `vd release` and `vd rollback` —
// ref-first positional, optional verb, verb-specific args after:
//
//	vd config <ref>                   list (default verb)
//	vd config <ref> list              same, explicit
//	vd config <ref> set KEY=VAL...    upsert one or more keys
//	vd config <ref> get [PATTERN]     read all (or substring-match
//	                                  PATTERN against key names)
//	vd config <ref> unset KEY...      delete one or more keys
//
// Ref shape (same disambiguation rule the rest of the CLI uses):
//
//   - "<scope>"           scope-level config (shared by every
//                         resource in the scope)
//   - "<scope>/<name>"    app-level config (overrides scope-level
//                         keys on conflict)
//
// On set/unset the controller fires reconcile events for affected
// resources so the new env reaches running pods without a manual
// `vd restart`. Pass --no-restart to batch edits before triggering.
//
// Examples:
//
//	vd config clowk-lp/web                       list every var visible to web
//	vd config clowk-lp/web set LOG_LEVEL=debug   set on the app level
//	vd config clowk-lp set LOG_LEVEL=info        set on the scope level
//	vd config clowk-lp/web get LOG               match-all keys containing LOG
//	vd config clowk-lp/web unset LOG_LEVEL       remove + restart
func newConfigCmd() *cobra.Command {
	var noRestart bool

	cmd := &cobra.Command{
		Use:   "config <ref> [verb] [args...]",
		Short: "Manage env vars for scopes and resources via the controller",
		Long: `Read and write environment variables stored in etcd. The ref
positional follows the same shape as vd release / vd rollback:

  <scope>             scope-level (shared across the scope)
  <scope>/<name>      app-level   (overrides scope-level keys)

Verbs:

  list (default)         list all keys visible to <ref>
  set KEY=VALUE [...]    upsert one or more keys
  get [PATTERN]          list keys; if PATTERN is given, only keys
                         whose name contains PATTERN (case-insensitive)
  unset KEY [...]        delete one or more keys

By default, set / unset trigger an automatic reconcile of every
container the change affects so the new env reaches running pods
without manual intervention. Pass --no-restart to batch edits and
defer the restart.

Examples:
  vd config clowk-lp/web                       # list (default verb)
  vd config clowk-lp/web set LOG_LEVEL=debug
  vd config clowk-lp set DATABASE_URL=...      # scope-level
  vd config clowk-lp/web get LOG               # substring match
  vd config clowk-lp/web unset LOG_LEVEL`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := parseConfigRef(args[0])
			if err != nil {
				return err
			}

			verb := "list"
			verbArgs := []string{}

			if len(args) > 1 {
				verb = strings.ToLower(strings.TrimSpace(args[1]))
				verbArgs = args[2:]
			}

			switch verb {
			case "list", "":
				return runConfigList(cmd, target)
			case "set":
				if len(verbArgs) == 0 {
					return fmt.Errorf("set: at least one KEY=VALUE is required")
				}

				return runConfigSet(cmd, target, verbArgs, !noRestart)
			case "get":
				pattern := ""
				if len(verbArgs) > 0 {
					pattern = verbArgs[0]
				}

				return runConfigGet(cmd, target, pattern)
			case "unset":
				if len(verbArgs) == 0 {
					return fmt.Errorf("unset: at least one KEY is required")
				}

				return runConfigUnset(cmd, target, verbArgs, !noRestart)
			default:
				return fmt.Errorf("unknown config verb %q (want list, set, get, unset)", verb)
			}
		},
	}

	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "do not auto-restart affected containers (set/unset only)")

	return cmd
}

// configTarget is the (scope, name) tuple the /config endpoint
// addresses by. name="" means scope-level config.
type configTarget struct {
	scope string
	name  string
}

// parseConfigRef splits "<scope>" or "<scope>/<name>" into the
// addressable target. Bare token is a scope; first slash is the
// boundary. Errors on empty input so a stray `vd config` doesn't
// silently target the empty scope.
//
// Inverse default vs splitJobRef: a bare token here means SCOPE,
// not name. Operators reach for `vd config clowk-lp` when they
// want "scope-level config", so the bare form populates scope and
// leaves name empty. The slash is the explicit "narrow to one
// resource" signal.
func parseConfigRef(ref string) (configTarget, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return configTarget{}, fmt.Errorf("ref is required (use <scope> or <scope>/<name>)")
	}

	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
		return configTarget{}, fmt.Errorf("invalid ref %q (leading/trailing slash)", ref)
	}

	if i := strings.Index(ref, "/"); i >= 0 {
		return configTarget{scope: ref[:i], name: ref[i+1:]}, nil
	}

	return configTarget{scope: ref}, nil
}

// runConfigList prints every var visible to the target. App-level
// targets get the merged scope+app view; scope-level targets get
// the bucket directly.
func runConfigList(cmd *cobra.Command, target configTarget) error {
	vars, err := configFetch(cmd, target.scope, target.name, "")
	if err != nil {
		return err
	}

	if len(vars) == 0 {
		fmt.Println("No environment variables set")
		return nil
	}

	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, vars[k])
	}

	return nil
}

// runConfigSet posts the parsed KEY=VALUE pairs to /config. Empty
// VALUE is allowed (set-to-empty intent); a missing `=` errors so
// the operator doesn't accidentally pass a bare key.
func runConfigSet(cmd *cobra.Command, target configTarget, args []string, restart bool) error {
	payload, err := parseKeyValuePairs(args)
	if err != nil {
		return err
	}

	if err := configPatch(cmd, target.scope, target.name, payload, restart); err != nil {
		return err
	}

	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, payload[k])
	}

	return nil
}

// runConfigGet reads vars and filters by PATTERN. Empty PATTERN is
// equivalent to list (mirrors `git remote` / `git remote -v`).
//
// Match is case-insensitive substring against the key name —
// `get LOG` returns LOG_LEVEL, RAILS_LOG, anything containing LOG.
// Operators rarely want exact-match for env reads (they're
// remembering "the var about logging" not the exact spelling), and
// substring is a strict superset: an exact key is a substring of
// itself.
func runConfigGet(cmd *cobra.Command, target configTarget, pattern string) error {
	vars, err := configFetch(cmd, target.scope, target.name, "")
	if err != nil {
		return err
	}

	keys := make([]string, 0, len(vars))

	if pattern == "" {
		for k := range vars {
			keys = append(keys, k)
		}
	} else {
		needle := strings.ToUpper(pattern)
		for k := range vars {
			if strings.Contains(strings.ToUpper(k), needle) {
				keys = append(keys, k)
			}
		}
	}

	if len(keys) == 0 {
		if pattern == "" {
			fmt.Println("No environment variables set")
			return nil
		}

		return fmt.Errorf("no keys match %q", pattern)
	}

	sort.Strings(keys)

	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, vars[k])
	}

	return nil
}

// runConfigUnset deletes one or more keys, one per /config DELETE.
// The server fires reconcile events for affected resources unless
// --no-restart is set.
func runConfigUnset(cmd *cobra.Command, target configTarget, keys []string, restart bool) error {
	for _, key := range keys {
		if err := configDelete(cmd, target.scope, target.name, key, restart); err != nil {
			return err
		}

		fmt.Printf("Unset %s\n", key)
	}

	return nil
}

// parseKeyValuePairs splits "KEY=VALUE" tokens into a map. Empty
// VALUE is allowed (a real "set to empty" intent); a missing `=`
// errors so the operator doesn't accidentally pass a bare key.
func parseKeyValuePairs(args []string) (map[string]string, error) {
	out := make(map[string]string, len(args))

	for _, a := range args {
		idx := strings.IndexByte(a, '=')
		if idx <= 0 {
			return nil, fmt.Errorf("expected KEY=VALUE, got %q", a)
		}

		key := strings.TrimSpace(a[:idx])
		val := a[idx+1:]

		if key == "" {
			return nil, fmt.Errorf("empty key in %q", a)
		}

		out[key] = val
	}

	return out, nil
}

// configPatch POSTs to /config?scope=&name=[&restart=false] with
// the given vars. Returns nil on 200, the server error verbatim
// otherwise.
func configPatch(cmd *cobra.Command, scope, name string, vars map[string]string, restart bool) error {
	q := url.Values{}
	q.Set("scope", scope)

	if name != "" {
		q.Set("name", name)
	}

	if !restart {
		q.Set("restart", "false")
	}

	body, err := json.Marshal(vars)
	if err != nil {
		return err
	}

	resp, err := controllerDo(cmd.Root(), http.MethodPost, "/config", q.Encode(), bytes.NewReader(body))
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return surfaceConfigError(resp.StatusCode, raw)
	}

	return nil
}

// configFetch GETs /config?scope=&name=[&key=]. When key is empty
// the response data carries a `vars` map; when set, the data is a
// single-key map.
func configFetch(cmd *cobra.Command, scope, name, key string) (map[string]string, error) {
	q := url.Values{}
	q.Set("scope", scope)

	if name != "" {
		q.Set("name", name)
	}

	if key != "" {
		q.Set("key", key)
	}

	resp, err := controllerDo(cmd.Root(), http.MethodGet, "/config", q.Encode(), nil)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		// 404 only happens for ?key= with a missing key; an empty
		// list is 200. Surface verbatim.
		return nil, surfaceConfigError(resp.StatusCode, raw)
	}

	if resp.StatusCode >= 400 {
		return nil, surfaceConfigError(resp.StatusCode, raw)
	}

	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// When ?key= is supplied, server returns a flat map. Otherwise
	// it nests under "vars".
	if key != "" {
		var direct map[string]string
		if err := json.Unmarshal(env.Data, &direct); err == nil {
			return direct, nil
		}
	}

	var nested struct {
		Vars map[string]string `json:"vars"`
	}

	if err := json.Unmarshal(env.Data, &nested); err != nil {
		return nil, fmt.Errorf("decode vars: %w", err)
	}

	if nested.Vars == nil {
		nested.Vars = map[string]string{}
	}

	return nested.Vars, nil
}

// configDelete DELETEs /config?scope=&name=&key=.
func configDelete(cmd *cobra.Command, scope, name, key string, restart bool) error {
	q := url.Values{}
	q.Set("scope", scope)

	if name != "" {
		q.Set("name", name)
	}

	q.Set("key", key)

	if !restart {
		q.Set("restart", "false")
	}

	resp, err := controllerDo(cmd.Root(), http.MethodDelete, "/config", q.Encode(), nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return surfaceConfigError(resp.StatusCode, raw)
	}

	return nil
}

// surfaceConfigError decodes the controller's JSON-envelope error
// shape and returns the server-side message verbatim. Plain-text
// bodies (rare; only when the controller crashes mid-response)
// fall through unchanged.
func surfaceConfigError(code int, raw []byte) error {
	var env struct {
		Error string `json:"error"`
	}

	if err := json.Unmarshal(raw, &env); err == nil && env.Error != "" {
		return fmt.Errorf("%s", env.Error)
	}

	return formatControllerError(code, raw)
}
