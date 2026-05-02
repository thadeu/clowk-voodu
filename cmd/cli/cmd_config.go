package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	var (
		noRestart bool
		output    string
	)

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

Output formats (-o / --output, applies to list / get / set):

  text (default)         KEY=VALUE one per line, copy-paste-able into a .env
  json                   {"KEY": "VALUE"} object — pipe into jq / scripts
  hcl                    env = { KEY = "VALUE" } — paste into a manifest's
                         spec.env block

By default, set / unset trigger an automatic reconcile of every
container the change affects so the new env reaches running pods
without manual intervention. Pass --no-restart to batch edits and
defer the restart.

Examples:
  vd config clowk-lp/web                       # list (default verb)
  vd config clowk-lp/web -o json               # list as JSON
  vd config clowk-lp/web get LOG -o hcl        # filtered, HCL-formatted
  vd config clowk-lp/web set LOG_LEVEL=debug
  vd config clowk-lp set DATABASE_URL=...      # scope-level
  vd config clowk-lp/web unset LOG_LEVEL`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := parseConfigRef(args[0])
			if err != nil {
				return err
			}

			format, err := parseConfigOutputFormat(output)
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
				return runConfigList(cmd, target, format)
			case "set":
				if len(verbArgs) == 0 {
					return fmt.Errorf("set: at least one KEY=VALUE is required")
				}

				return runConfigSet(cmd, target, verbArgs, !noRestart, format)
			case "get":
				pattern := ""
				if len(verbArgs) > 0 {
					pattern = verbArgs[0]
				}

				return runConfigGet(cmd, target, pattern, format)
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
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text | json | hcl")

	return cmd
}

// configOutputFormat is the closed enum of -o values vd config
// accepts. Three formats cover the realistic operator workflows:
//
//   text  — KEY=VALUE per line, paste into a .env file or copy
//           a single value from the terminal
//   json  — pipe through jq / consume from scripts
//   hcl   — paste straight into a manifest's `env = { ... }` block
//
// YAML was considered and dropped — the output `KEY: VALUE` per
// line is functionally redundant with text's `KEY=VALUE`. Operators
// who want YAML for k8s/compose docs can `sed 's/=/: /'` the text
// output, or use json + yq.
type configOutputFormat string

const (
	configOutputText configOutputFormat = "text"
	configOutputJSON configOutputFormat = "json"
	configOutputHCL  configOutputFormat = "hcl"
)

// parseConfigOutputFormat normalises and validates the operator-
// supplied -o value. Empty string defaults to text — Cobra's
// default value handles that case but the helper still accepts
// it explicitly so test harnesses can call with "".
func parseConfigOutputFormat(s string) (configOutputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "text":
		return configOutputText, nil
	case "json":
		return configOutputJSON, nil
	case "hcl":
		return configOutputHCL, nil
	default:
		return "", fmt.Errorf("invalid output format %q (want text, json, or hcl)", s)
	}
}

// renderConfigVars writes the (key,value) map to stdout in the
// requested format. Sorted by key so output is deterministic
// across runs — important for diff-friendly piping into git or
// scripts that snapshot operator output.
//
// Empty input prints the format-appropriate empty marker (empty
// JSON object, empty HCL block, empty YAML map) — text mode
// stays silent because an empty list of `KEY=VALUE` lines is
// still valid.
func renderConfigVars(w io.Writer, vars map[string]string, format configOutputFormat) error {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	switch format {
	case configOutputJSON:
		// MarshalIndent for human-readability when piping to
		// terminal; jq / scripts handle whitespace fine.
		raw, err := json.MarshalIndent(vars, "", "  ")
		if err != nil {
			return fmt.Errorf("encode json: %w", err)
		}

		fmt.Fprintln(w, string(raw))

		return nil

	case configOutputHCL:
		fmt.Fprintln(w, "env = {")

		for _, k := range keys {
			// HCL string values need backslashes + double-quotes
			// escaped. strconv.Quote covers both plus produces
			// a valid HCL string literal as a side effect of
			// being a valid Go string literal (HCL adopted Go's
			// string syntax for quoted strings).
			fmt.Fprintf(w, "  %s = %s\n", k, hclQuote(vars[k]))
		}

		fmt.Fprintln(w, "}")

		return nil

	default: // text
		for _, k := range keys {
			fmt.Fprintf(w, "%s=%s\n", k, vars[k])
		}

		return nil
	}
}

// hclQuote wraps a string in HCL-compatible double quotes,
// escaping internal `"` and `\` per HCL2's string-literal grammar
// (which mirrors JSON's). Used by the `-o hcl` formatter to make
// the output safely paste-able into a `spec.env = {...}` block.
func hclQuote(s string) string {
	// Reuse Go's strconv to escape — HCL2 accepts the same
	// double-quoted escape sequences (\n, \t, \", \\, \uXXXX).
	// Operator-supplied env values rarely have weird chars, but
	// we shouldn't break when they do.
	var b strings.Builder

	b.Grow(len(s) + 2)
	b.WriteByte('"')

	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}

	b.WriteByte('"')

	return b.String()
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
// the bucket directly. Output format follows the operator's -o
// flag (text by default).
func runConfigList(cmd *cobra.Command, target configTarget, format configOutputFormat) error {
	vars, err := configFetch(cmd, target.scope, target.name, "")
	if err != nil {
		return err
	}

	if len(vars) == 0 && format == configOutputText {
		// Empty + text → friendly message. Other formats emit
		// the format-appropriate empty marker (JSON `{}`, HCL
		// `env = {}`, YAML `{}`) so script consumers always get
		// valid syntax.
		fmt.Println("No environment variables set")
		return nil
	}

	return renderConfigVars(os.Stdout, vars, format)
}

// runConfigSet posts the parsed KEY=VALUE pairs to /config. Empty
// VALUE is allowed (set-to-empty intent); a missing `=` errors so
// the operator doesn't accidentally pass a bare key. Echoes the
// just-set keys in the operator's chosen output format — useful
// for piping confirmation through jq / direct paste-back into HCL.
func runConfigSet(cmd *cobra.Command, target configTarget, args []string, restart bool, format configOutputFormat) error {
	payload, err := parseKeyValuePairs(args)
	if err != nil {
		return err
	}

	if err := configPatch(cmd, target.scope, target.name, payload, restart); err != nil {
		return err
	}

	return renderConfigVars(os.Stdout, payload, format)
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
func runConfigGet(cmd *cobra.Command, target configTarget, pattern string, format configOutputFormat) error {
	vars, err := configFetch(cmd, target.scope, target.name, "")
	if err != nil {
		return err
	}

	// Filter by substring pattern (case-insensitive). Empty
	// pattern is the no-op identity filter.
	filtered := vars

	if pattern != "" {
		needle := strings.ToUpper(pattern)
		filtered = make(map[string]string, len(vars))

		for k, v := range vars {
			if strings.Contains(strings.ToUpper(k), needle) {
				filtered[k] = v
			}
		}
	}

	if len(filtered) == 0 {
		if pattern == "" {
			if format == configOutputText {
				fmt.Println("No environment variables set")
				return nil
			}
			// Other formats emit the empty-map marker — keep
			// consumers (jq, scripts) happy with valid syntax.
		} else {
			return fmt.Errorf("no keys match %q", pattern)
		}
	}

	return renderConfigVars(os.Stdout, filtered, format)
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
