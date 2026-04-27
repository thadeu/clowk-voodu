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

// newConfigCmd is the M-4 successor to the filesystem-backed
// `vd config`. Every operation now goes through the controller's
// /config endpoint so an operator on their dev Mac can manipulate
// env vars without SSHing into the server.
//
// Two namespace levels:
//
//   - scope-level config (`-s clowk-lp`): shared across every
//     resource in the scope.
//   - app-level config (`-s clowk-lp -n web`): per-resource,
//     overrides scope-level keys on conflict.
//
// `-a clowk-lp/web` is a one-flag shortcut that splits scope/name
// in the same shape `vd describe` and `vd logs` use. Mix-and-match
// with `-s`/`-n` is permissive (explicit -s/-n wins) so muscle
// memory transfers cleanly between commands.
//
// On set/unset the server fires reconcile events for affected
// resources so the changes land in running containers without an
// explicit `vd config reload`. Pass --no-restart to batch multiple
// edits before triggering the reconcile.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage env vars for scopes and resources via the controller",
		Long: `Read and write environment variables stored in etcd. Two
addressing levels:

  vd config set FOO=bar -s clowk-lp                  scope-level (shared)
  vd config set FOO=bar -s clowk-lp -n web           app-level (overrides scope)
  vd config set FOO=bar -a clowk-lp/web              same as above (one flag)
  vd config set FOO=bar -a clowk-lp                  scope-level via -a

App-level keys override scope-level on conflict — same precedence
shells use for /etc/environment vs ~/.profile.

By default, set / unset trigger an automatic reconcile of every
container the change affects so the new env reaches running pods
without manual intervention. Pass --no-restart to batch edits and
defer the restart.`,
	}

	cmd.AddCommand(
		configSetCmd(),
		configGetCmd(),
		configListCmd(),
		configUnsetCmd(),
	)

	return cmd
}

// configTarget bundles the (scope, name) pair every config command
// needs to compute. Three input shapes folded into one place:
//
//   -s SCOPE -n NAME         classic, both flags
//   -s SCOPE                 scope-level (no name)
//   -a SCOPE/NAME            shorthand, splits on first slash
//   -a SCOPE                 shorthand, scope-level
//
// When both -a and -s/-n are passed, explicit -s/-n wins on each
// field so muscle-memory invocations like
// `vd config set X=y -a clowk-lp -n web` still work intuitively
// (the operator wrote -n explicitly, that wins).
type configTarget struct {
	scope string
	name  string
}

func resolveConfigTarget(cmd *cobra.Command, scope, name, app string) (configTarget, error) {
	target := configTarget{scope: scope, name: name}

	if app != "" {
		// `-a` is config-specific and uses the inverse default of
		// splitJobRef: a bare token (no slash) is the SCOPE, not a
		// name. Operators reach for `-a clowk-lp` when they mean
		// "scope-level config" — so the bare form must populate
		// scope, leaving name empty.
		var appScope, appName string

		if i := strings.Index(app, "/"); i >= 0 {
			appScope = app[:i]
			appName = app[i+1:]
		} else {
			appScope = app
		}

		if !cmd.Flags().Changed("scope") {
			target.scope = appScope
		}

		if !cmd.Flags().Changed("name") {
			target.name = appName
		}
	}

	if target.scope == "" {
		return configTarget{}, fmt.Errorf("--scope/-s (or --app/-a) is required")
	}

	return target, nil
}

func configSetCmd() *cobra.Command {
	var (
		scope     string
		name      string
		app       string
		noRestart bool
	)

	cmd := &cobra.Command{
		Use:   "set KEY=VALUE [KEY=VALUE ...]",
		Short: "Set one or more env vars on the controller",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveConfigTarget(cmd, scope, name, app)
			if err != nil {
				return err
			}

			payload, err := parseKeyValuePairs(args)
			if err != nil {
				return err
			}

			if err := configPatch(cmd, target.scope, target.name, payload, !noRestart); err != nil {
				return err
			}

			for k, v := range payload {
				fmt.Printf("%s=%s\n", k, v)
			}

			return nil
		},
	}

	addConfigTargetFlags(cmd, &scope, &name, &app)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "do not auto-restart affected containers")

	return cmd
}

func configGetCmd() *cobra.Command {
	var (
		scope string
		name  string
		app   string
	)

	cmd := &cobra.Command{
		Use:   "get KEY",
		Short: "Read one env var",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveConfigTarget(cmd, scope, name, app)
			if err != nil {
				return err
			}

			vars, err := configFetch(cmd, target.scope, target.name, args[0])
			if err != nil {
				return err
			}

			v, ok := vars[args[0]]
			if !ok {
				return fmt.Errorf("key %q not set", args[0])
			}

			fmt.Printf("%s=%s\n", args[0], v)

			return nil
		},
	}

	addConfigTargetFlags(cmd, &scope, &name, &app)

	return cmd
}

func configListCmd() *cobra.Command {
	var (
		scope string
		name  string
		app   string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List env vars (merged scope+app when --name is set)",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveConfigTarget(cmd, scope, name, app)
			if err != nil {
				return err
			}

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
		},
	}

	addConfigTargetFlags(cmd, &scope, &name, &app)

	return cmd
}

func configUnsetCmd() *cobra.Command {
	var (
		scope     string
		name      string
		app       string
		noRestart bool
	)

	cmd := &cobra.Command{
		Use:   "unset KEY [KEY ...]",
		Short: "Delete one or more env vars",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveConfigTarget(cmd, scope, name, app)
			if err != nil {
				return err
			}

			for _, key := range args {
				if err := configDelete(cmd, target.scope, target.name, key, !noRestart); err != nil {
					return err
				}

				fmt.Printf("Unset %s\n", key)
			}

			return nil
		},
	}

	addConfigTargetFlags(cmd, &scope, &name, &app)
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "do not auto-restart affected containers")

	return cmd
}

// addConfigTargetFlags wires the standard scope/name/app trio onto
// a config subcommand. Centralised so help text and flag shorthands
// stay identical across set / get / list / unset — divergence here
// would be the kind of bug that surfaces only as "why does -a work
// on set but not on list".
func addConfigTargetFlags(cmd *cobra.Command, scope, name, app *string) {
	cmd.Flags().StringVarP(scope, "scope", "s", "", "scope (required, or use -a)")
	cmd.Flags().StringVarP(name, "name", "n", "", "resource name (omit for scope-level)")
	cmd.Flags().StringVarP(app, "app", "a", "",
		"shorthand for --scope/--name (e.g. clowk-lp/web), or just <scope> for scope-level")
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

	return fmt.Errorf("controller returned %d: %s", code, strings.TrimSpace(string(raw)))
}
