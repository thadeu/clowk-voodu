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
	"gopkg.in/yaml.v3"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/manifest"
)

const applyTimeout = 30 * time.Second

type applyFlags struct {
	files            []string
	format           string // stdin only: "hcl" | "yaml"
	noPrune          bool   // apply + diff: upsert without deleting siblings in the same (scope, kind)
	detailedExitcode bool   // diff only: exit 2 when there are changes, mirrors `terraform plan`
	autoApprove      bool   // apply only: skip the interactive confirmation in forwarded mode
	force            bool   // apply only: force rebuild even when the tarball hash already has a release
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
environment before parsing. Use ${VAR:-default} to fall back.

By default, apply is source-of-truth: anything in the same
(scope, kind) that isn't in this apply gets pruned. Pass --no-prune
when several independent applies (different repos, different CI
pipelines) share a scope and each declares only a slice of it.

When forwarded to a remote, apply runs diff first, shows the plan,
and prompts for y/N on your local terminal. Pass --auto-approve
(alias -y) or set VOODU_AUTO_APPROVE=1 to skip the prompt in CI.
Non-interactive invocations without either will refuse to apply.

Build-mode deployments ship their source as a content-addressed
tarball. Identical trees skip rebuild and just repoint the 'current'
symlink — fast path for "same code, redeploy". Pass --force to
rebuild the image anyway (useful for non-deterministic build caches
or when validating CI image changes). VOODU_FORCE_REBUILD=1 has the
same effect.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApply(cmd, f)
		},
	}

	cmd.Flags().StringArrayVarP(&f.files, "file", "f", nil, "manifest file (extension optional), directory, or - for stdin (repeatable)")
	cmd.Flags().StringVar(&f.format, "format", "", "stdin format: hcl, yaml, or json (required for -f -)")
	cmd.Flags().BoolVar(&f.noPrune, "no-prune", false, "upsert only; do not delete other resources in the same (scope, kind)")
	cmd.Flags().BoolVarP(&f.autoApprove, "auto-approve", "y", false, "skip the interactive y/N confirmation (also VOODU_AUTO_APPROVE=1)")
	cmd.Flags().BoolVar(&f.force, "force", false, "rebuild build-mode deployments even when the tarball hash matches an existing release (also VOODU_FORCE_REBUILD=1)")

	return cmd
}

func newDiffCmd() *cobra.Command {
	var f applyFlags

	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Show changes between local manifests and the controller",
		Long: `Renders a plan-style diff of what 'voodu apply' would do:

  ~ kind/scope/name      — resource exists and its spec changed
  + kind/scope/name      — resource would be created
  = kind/scope/name      — spec matches the controller (no change)
  --- Would prune ---    — resources that would be deleted by prune

The diff calls the controller with ?dry_run=true, so nothing is
persisted and the output matches byte-for-byte what the next
'voodu apply' would do with the same flags.

By default, the pruned section reflects the source-of-truth apply
behavior. Pass --no-prune to simulate an upsert-only apply (for
shared-scope cross-repo workflows).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd, f)
		},
	}

	cmd.Flags().StringArrayVarP(&f.files, "file", "f", nil, "manifest file (extension optional), directory, or - for stdin (repeatable)")
	cmd.Flags().StringVar(&f.format, "format", "", "stdin format: hcl, yaml, or json (required for -f -)")
	cmd.Flags().BoolVar(&f.noPrune, "no-prune", false, "simulate an apply that wouldn't prune siblings in the same (scope, kind)")
	cmd.Flags().BoolVar(&f.detailedExitcode, "detailed-exitcode", false, "exit 0 when no changes, 2 when changes, 1 on error (CI-friendly)")

	// --detailed-exitcode returns errExitWithChanges to signal code 2.
	// Silence cobra's auto-printed "Error:" + usage blurb so the
	// diff output stays clean — main() takes over exit-code mapping.
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

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

	// Local apply (no remote, no SSH) applies directly. The diff+prompt
	// dance belongs to the forwarded path — on the dev box or the
	// server itself, `runApply` is used by tests, server-init, and
	// one-off operator commands where a prompt would get in the way.
	// See runApplyForwarded for the two-phase orchestrated flow.
	//
	// `force` only has meaning when we push a tarball to receive-pack;
	// the local path reconciles the controller directly and never
	// touches the build cache. The flag is silently ignored here.
	_ = f.autoApprove
	_ = f.force

	root := cmd.Root()

	body, err := json.Marshal(mans)
	if err != nil {
		return err
	}

	query := ""
	if f.noPrune {
		query = "prune=false"
	}

	resp, err := controllerDo(root, http.MethodPost, "/apply", query, bytes.NewReader(body))
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

	if len(local) == 0 {
		return fmt.Errorf("no manifests found")
	}

	body, err := json.Marshal(local)
	if err != nil {
		return err
	}

	// Diff piggybacks on /apply?dry_run=true so the controller is the
	// one source of truth about what would happen — same prune logic,
	// same validation, same ordering. Whatever the server would do on
	// a real apply shows up here, nothing more.
	query := "dry_run=true"
	if f.noPrune {
		query += "&prune=false"
	}

	resp, err := controllerDo(cmd.Root(), http.MethodPost, "/apply", query, bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("controller returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var plan diffResponse
	if err := json.Unmarshal(raw, &plan); err != nil {
		return fmt.Errorf("decode dry-run: %w", err)
	}

	out := cmd.OutOrStdout()

	// Machine-readable formats bypass the renderer entirely. The plan
	// struct is emitted verbatim so callers (CI pipelines, the
	// apply-forwarded orchestrator on the client) can parse it. We
	// still honour --detailed-exitcode so `voodu diff -o json
	// --detailed-exitcode | jq` scripts get the same signal as text
	// mode. Counts are derived locally from plan.Data — mirrors what
	// the text renderer would compute.
	switch outputFormat(cmd.Root()) {
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")

		if err := enc.Encode(plan); err != nil {
			return err
		}

		return detailedExitcodeFromPlan(f.detailedExitcode, plan)

	case "yaml":
		if err := yaml.NewEncoder(out).Encode(plan); err != nil {
			return err
		}

		return detailedExitcodeFromPlan(f.detailedExitcode, plan)
	}

	palette := newDiffPalette(out)

	added, modified := renderApplyPlan(out, plan, palette)
	renderPrunePlan(out, plan.Data.Pruned, palette)

	fmt.Fprintf(out, "\n%s\n", diffSummary(added, modified, len(plan.Data.Pruned)))

	// `--detailed-exitcode` mirrors `terraform plan`: 0 = clean,
	// 2 = changes pending, 1 = error. Without the flag, we return nil
	// so existing scripts that ignore exit code keep working.
	if f.detailedExitcode && (added+modified+len(plan.Data.Pruned)) > 0 {
		return errExitWithChanges
	}

	return nil
}

// detailedExitcodeFromPlan replays the `--detailed-exitcode` decision
// without running the text renderer, so JSON / YAML callers get the
// same exit-code contract as humans. A resource with no matching
// `current` entry (or a spec mismatch) counts as a change — same
// rule the text renderer uses via renderApplyPlan.
func detailedExitcodeFromPlan(enabled bool, plan diffResponse) error {
	if !enabled {
		return nil
	}

	changes := len(plan.Data.Pruned)

	for i, desired := range plan.Data.Applied {
		if desired == nil {
			continue
		}

		var current *controller.Manifest

		if i < len(plan.Data.Current) {
			current = plan.Data.Current[i]
		}

		if current == nil || len(diffSpec(desired.Spec, current.Spec)) > 0 {
			changes++
		}
	}

	if changes > 0 {
		return errExitWithChanges
	}

	return nil
}

// errExitWithChanges is a sentinel returned by runDiff to signal the
// main() exit-code handler that a non-zero code is warranted even
// though no actual error occurred.
var errExitWithChanges = fmt.Errorf("voodu-diff-has-changes")

// renderApplyPlan walks each (applied, current) pair from the dry-run
// response and prints the resource header plus — for modified and
// created resources — the field-by-field diff underneath. A blank
// line separates each resource so two back-to-back kinds
// (deployment + ingress, say) don't smash into one visual block.
// Returns counts so the caller can produce the final summary line.
func renderApplyPlan(w io.Writer, plan diffResponse, p diffPalette) (added, modified int) {
	first := true

	for i, desired := range plan.Data.Applied {
		if desired == nil {
			continue
		}

		var current *controller.Manifest

		if i < len(plan.Data.Current) {
			current = plan.Data.Current[i]
		}

		label := formatRef(desired.Kind, desired.Scope, desired.Name)

		// Blank line between resources. Skipped before the first
		// printed row so a single-resource diff stays compact.
		if !first {
			fmt.Fprintln(w)
		}

		first = false

		if current == nil {
			fmt.Fprintf(w, "%s %s (new)\n", p.Add("+"), label)

			renderResourceDiff(w, diffSpec(desired.Spec, nil), p)

			added++

			continue
		}

		changes := diffSpec(desired.Spec, current.Spec)

		if len(changes) == 0 {
			fmt.Fprintf(w, "= %s (unchanged)\n", label)
			continue
		}

		fmt.Fprintf(w, "%s %s\n", p.Mod("~"), label)

		renderResourceDiff(w, changes, p)

		modified++
	}

	return added, modified
}

// renderPrunePlan prints the footer block listing resources that would
// be removed. Skipped when empty so clean diffs stay terse; the hint
// about --no-prune reminds operators about the shared-scope escape
// hatch without forcing them to recall the flag name.
func renderPrunePlan(w io.Writer, pruned []string, p diffPalette) {
	if len(pruned) == 0 {
		return
	}

	fmt.Fprintln(w, "\n--- Would prune (pass --no-prune to keep) ---")

	for _, ref := range pruned {
		fmt.Fprintf(w, "%s %s\n", p.Del("-"), ref)
	}
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

