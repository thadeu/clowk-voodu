package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/manifest"
)

const applyTimeout = 30 * time.Second

type applyFlags struct {
	files  []string
	dryRun bool
	format string // stdin only: "hcl" | "yaml"
}

func newApplyCmd() *cobra.Command {
	var f applyFlags

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply manifests (HCL or YAML) to the controller",
		Long: `Reads one or more manifests and POSTs them to the controller.

Accepted inputs:
  -f api               short form — tries .hcl/.voodu/.vdu/.vd/.yml/.yaml
  -f file.hcl          apply a single file (.hcl/.voodu/.vdu/.vd are all HCL)
  -f ./dir             walk dir recursively for manifest files
  -f a.voodu -f b.yml  mix files of either format
  -f -                 read from stdin (requires --format hcl|yaml)

Use -a <remote> to forward the apply to a configured voodu remote.
The file is parsed locally so ${VAR} expands on your dev machine,
then streamed to the server over SSH — the controller never needs
a public port.

${VAR} in the file body is interpolated from the current process
environment before parsing. Use ${VAR:-default} to fall back.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApply(cmd, f)
		},
	}

	cmd.Flags().StringArrayVarP(&f.files, "file", "f", nil, "manifest file (extension optional), directory, or - for stdin (repeatable)")
	cmd.Flags().BoolVarP(&f.dryRun, "dry-run", "n", false, "print the manifests that would be applied and exit")
	cmd.Flags().StringVar(&f.format, "format", "", "stdin format: hcl, yaml, or json (required for -f -)")

	return cmd
}

func newDiffCmd() *cobra.Command {
	var f applyFlags

	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Show changes between local manifests and the controller",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd, f)
		},
	}

	cmd.Flags().StringArrayVarP(&f.files, "file", "f", nil, "manifest file (extension optional), directory, or - for stdin (repeatable)")
	cmd.Flags().StringVar(&f.format, "format", "", "stdin format: hcl, yaml, or json (required for -f -)")

	return cmd
}

func newDeleteCmd() *cobra.Command {
	var f applyFlags

	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete resources declared in the given manifests",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(cmd, f)
		},
	}

	cmd.Flags().StringArrayVarP(&f.files, "file", "f", nil, "manifest file (extension optional), directory, or - for stdin (repeatable)")
	cmd.Flags().StringVar(&f.format, "format", "", "stdin format: hcl, yaml, or json (required for -f -)")

	return cmd
}

func runApply(cmd *cobra.Command, f applyFlags) error {
	mans, err := loadManifests(f)
	if err != nil {
		return err
	}

	if len(mans) == 0 {
		return fmt.Errorf("no manifests found")
	}

	if f.dryRun {
		return printManifests(mans)
	}

	root := cmd.Root()

	body, err := json.Marshal(mans)
	if err != nil {
		return err
	}

	resp, err := controllerDo(root, http.MethodPost, "/apply", "", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("controller returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	for _, m := range mans {
		if m.Scope != "" {
			fmt.Printf("%s/%s/%s applied\n", m.Kind, m.Scope, m.Name)
		} else {
			fmt.Printf("%s/%s applied\n", m.Kind, m.Name)
		}
	}

	// The controller returns {"data": {"applied": [...], "pruned": [...]}}.
	// Surface prune results so operators can see what a re-apply removed.
	var env struct {
		Data struct {
			Pruned []string `json:"pruned"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err == nil {
		for _, p := range env.Data.Pruned {
			fmt.Printf("%s pruned (removed from manifests)\n", p)
		}
	}

	return nil
}

func runDiff(cmd *cobra.Command, f applyFlags) error {
	local, err := loadManifests(f)
	if err != nil {
		return err
	}

	root := cmd.Root()

	for _, m := range local {
		remote, err := fetchRemote(root, m.Kind, m.Scope, m.Name)
		if err != nil {
			return err
		}

		label := formatRef(m.Kind, m.Scope, m.Name)

		if remote == nil {
			fmt.Printf("+ %s (new)\n", label)
			continue
		}

		if bytes.Equal(canonicalJSON(m.Spec), canonicalJSON(remote.Spec)) {
			fmt.Printf("= %s (unchanged)\n", label)
			continue
		}

		fmt.Printf("~ %s\n  local : %s\n  remote: %s\n",
			label,
			string(canonicalJSON(m.Spec)),
			string(canonicalJSON(remote.Spec)),
		)
	}

	return nil
}

// formatRef prints "kind/scope/name" when scoped, "kind/name" otherwise.
// Kept in one place so diff / apply / prune output stays consistent.
func formatRef(kind controller.Kind, scope, name string) string {
	if scope != "" {
		return fmt.Sprintf("%s/%s/%s", kind, scope, name)
	}

	return fmt.Sprintf("%s/%s", kind, name)
}

func runDelete(cmd *cobra.Command, f applyFlags) error {
	mans, err := loadManifests(f)
	if err != nil {
		return err
	}

	if len(mans) == 0 {
		return fmt.Errorf("no manifests found")
	}

	root := cmd.Root()

	for _, m := range mans {
		q := url.Values{}
		q.Set("kind", string(m.Kind))
		q.Set("name", m.Name)

		if m.Scope != "" {
			q.Set("scope", m.Scope)
		}

		resp, err := controllerDo(root, http.MethodDelete, "/apply", q.Encode(), nil)
		if err != nil {
			return err
		}

		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusNotFound:
			fmt.Printf("? %s/%s (not found)\n", m.Kind, m.Name)
		case resp.StatusCode >= 400:
			return fmt.Errorf("delete %s/%s: %d: %s", m.Kind, m.Name, resp.StatusCode, strings.TrimSpace(string(raw)))
		default:
			fmt.Printf("- %s/%s deleted\n", m.Kind, m.Name)
		}
	}

	return nil
}

// loadManifests expands every -f argument (file, dir, stdin) into a flat
// list, applying ${VAR} interpolation from the current environment.
func loadManifests(f applyFlags) ([]controller.Manifest, error) {
	if len(f.files) == 0 {
		return nil, fmt.Errorf("at least one -f is required")
	}

	env := envAsMap()

	var out []controller.Manifest

	for _, path := range f.files {
		mans, err := loadOne(path, f.format, env)
		if err != nil {
			return nil, err
		}

		out = append(out, mans...)
	}

	return out, nil
}

func loadOne(path, stdinFormat string, env map[string]string) ([]controller.Manifest, error) {
	if path == "-" {
		if stdinFormat == "" {
			return nil, fmt.Errorf("-f -: --format hcl|yaml is required for stdin")
		}

		return manifest.ParseReader(os.Stdin, manifest.Format(stdinFormat), env)
	}

	resolved, info, err := resolveManifestPath(path)
	if err != nil {
		return nil, err
	}

	if info.IsDir() {
		return manifest.ParseDir(resolved, env)
	}

	return manifest.ParseFile(resolved, env)
}

// resolveManifestPath adds a manifest extension when the user omitted
// one. `voodu apply -f api` should just work when api.voodu (or .hcl,
// .vdu, .vd, .yml, .yaml) is sitting next to it. HCL variants are
// probed before YAML so a project with both wins toward the typed
// format.
func resolveManifestPath(path string) (string, os.FileInfo, error) {
	if info, err := os.Stat(path); err == nil {
		return path, info, nil
	}

	for _, ext := range []string{".hcl", ".voodu", ".vdu", ".vd", ".yml", ".yaml"} {
		candidate := path + ext

		if info, err := os.Stat(candidate); err == nil {
			return candidate, info, nil
		}
	}

	// Fall back to the original path so the error message names what the
	// user typed, not an extension the user didn't write.
	_, err := os.Stat(path)

	return path, nil, err
}

func envAsMap() map[string]string {
	out := map[string]string{}

	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}

	return out
}

func printManifests(mans []controller.Manifest) error {
	b, err := json.MarshalIndent(mans, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(b))

	return nil
}

// controllerDo is the one-stop HTTP helper for apply/diff/delete. It
// honours --controller-url and VOODU_CONTROLLER_URL and sets a sane
// timeout so the CLI never hangs on an unreachable controller.
func controllerDo(root *cobra.Command, method, path, rawQuery string, body io.Reader) (*http.Response, error) {
	base := strings.TrimRight(controllerURL(root), "/")
	full := base + path

	if rawQuery != "" {
		full += "?" + rawQuery
	}

	req, err := http.NewRequest(method, full, body)
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	req.Header.Set("User-Agent", fmt.Sprintf("voodu-cli/%s", version))

	client := &http.Client{Timeout: applyTimeout}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("controller %s %s: %w", method, path, err)
	}

	return resp, nil
}

// fetchRemote GETs /apply?kind=&name= and returns the manifest or nil
// if the controller does not know about it. Scoped kinds are narrowed to
// a single scope; an empty scope for a scoped kind matches across scopes
// (used by helpers like `voodu scale` that auto-resolve).
func fetchRemote(root *cobra.Command, kind controller.Kind, scope, name string) (*controller.Manifest, error) {
	q := url.Values{}
	q.Set("kind", string(kind))

	resp, err := controllerDo(root, http.MethodGet, "/apply", q.Encode(), nil)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch %s/%s: %d", kind, name, resp.StatusCode)
	}

	var env struct {
		Status string                `json:"status"`
		Data   []controller.Manifest `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}

	for i := range env.Data {
		m := env.Data[i]

		if m.Kind != kind || m.Name != name {
			continue
		}

		if scope != "" && m.Scope != scope {
			continue
		}

		return &m, nil
	}

	return nil, nil
}

// canonicalJSON re-marshals a JSON blob with sorted keys so diff output
// isn't thrown off by key ordering differences.
func canonicalJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}

	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}

	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}

	return b
}
