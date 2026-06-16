package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/manifest"
	"go.voodu.clowk.in/internal/progress"
)

const applyTimeout = 30 * time.Second

// controllerInstallRef mirrors controller.PluginInstallation —
// what the server reports back when a plugin was JIT-installed
// during apply. Decoded inline in runApply so the CLI can
// surface "installed plugin X v0.2.0 from <repo>" to the
// operator.
type controllerInstallRef struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
}

// controllerExpansionRef mirrors controller.PluginExpansion.
// Surfaces "expanded postgres/data/main → statefulset/data/main"
// in the apply output so the operator sees the macro layer's
// work without spelunking into describe.
type controllerExpansionRef struct {
	From string   `json:"from"`
	To   []string `json:"to"`
}

type applyFlags struct {
	files            []string
	format           string // stdin only: "hcl"
	applyPrune       bool   // apply + diff: opt-in to delete siblings in the same (scope, kind) not declared in this apply
	detailedExitcode bool   // diff only: exit 2 when there are changes, mirrors `terraform plan`
	autoApprove      bool   // apply + delete: skip the interactive confirmation
	force            bool   // apply only: force rebuild even when the tarball hash already has a release
	verbose          bool   // apply only: passthrough raw build output instead of collapsing to a spinner
	dryRun           bool   // delete only: print the plan and exit without removing anything
	prune            bool   // delete only: also wipe config + on-disk app/volume dirs
	app              string // apply only: Procfile mode scope (default: random 3-char id)
	eject            bool   // apply only: scaffold an HCL .voodu from a Procfile instead of applying
}

func newApplyCmd() *cobra.Command {
	var f applyFlags

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply manifests (HCL) to the controller",
		Long: `Reads one or more manifests and POSTs them to the controller.

Accepted inputs:
  -f api               short form — tries .hcl/.voodu/.vdu/.vd
  -f file.hcl          apply a single file (.hcl/.voodu/.vdu/.vd are all HCL)
  -f ./dir             walk dir recursively for manifest files
  -f a.voodu -f b.hcl  mix files of any HCL-compatible extension
  -f -                 read from stdin (requires --format hcl)

Use -a <remote> to forward the apply to a configured voodu remote.
The file is parsed locally so ${VAR} expands on your dev machine,
then streamed to the server over SSH — the controller never needs
a public port.

${VAR} in the file body is interpolated from the current process
environment before parsing. Use ${VAR:-default} to fall back.

By default, apply is upsert-only: existing resources not declared
in this apply are LEFT ALONE. To clean up siblings in the same
(scope, kind) that aren't in this apply, pass --prune. Default-off
matches the operator workflow of refactoring HCL across multiple
files — splitting a stack into smaller files shouldn't accidentally
delete the resources you split out.

When forwarded to a remote, apply runs diff first, shows the plan,
and prompts for y/N on your local terminal. Pass --auto-approve
(alias -y) or set VOODU_AUTO_APPROVE=1 to skip the prompt in CI.
Non-interactive invocations without either will refuse to apply.

Build-mode deployments ship their source as a content-addressed
tarball. Identical trees skip rebuild and just repoint the 'current'
symlink — fast path for "same code, redeploy". Pass --force to
rebuild the image anyway (useful for non-deterministic build caches
or when validating CI image changes). VOODU_FORCE_REBUILD=1 has the
same effect.

Build output is collapsed into a spinner by default so docker buildx
chatter stays out of the way. Pass --verbose (alias -v) to see the
raw stream when debugging a failed build.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApply(cmd, f)
		},
	}

	cmd.Flags().StringArrayVarP(&f.files, "file", "f", nil, "manifest file (extension optional), directory, or - for stdin (repeatable)")
	cmd.Flags().StringVar(&f.format, "format", "", "stdin format: hcl or json (required for -f -)")
	cmd.Flags().BoolVar(&f.applyPrune, "prune", false, "delete sibling resources in the same (scope, kind) that aren't declared in this apply (default: upsert-only, leave existing siblings alone)")
	cmd.Flags().BoolVarP(&f.autoApprove, "auto-approve", "y", false, "skip the interactive y/N confirmation (also VOODU_AUTO_APPROVE=1)")
	cmd.Flags().BoolVar(&f.force, "force", false, "rebuild build-mode deployments even when the tarball hash matches an existing release (also VOODU_FORCE_REBUILD=1)")
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, "show raw docker build output (default: collapse into a spinner)")
	cmd.Flags().StringVar(&f.app, "app", "", "Procfile mode: scope for the generated resources (default: a random 3-char id)")
	cmd.Flags().BoolVar(&f.eject, "eject", false, "Procfile mode: write an equivalent .voodu file instead of applying")

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

By default, diff matches the upsert-only apply behavior — the
pruned section is empty. Pass --prune to preview which sibling
resources in the same (scope, kind) would be deleted IF you
ran 'vd apply --prune'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd, f)
		},
	}

	cmd.Flags().StringArrayVarP(&f.files, "file", "f", nil, "manifest file (extension optional), directory, or - for stdin (repeatable)")
	cmd.Flags().StringVar(&f.format, "format", "", "stdin format: hcl or json (required for -f -)")
	cmd.Flags().BoolVar(&f.applyPrune, "prune", false, "preview which siblings would be deleted by 'apply --prune' (default: upsert-only)")
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
		Use:   "delete [-f file.hcl | <scope> | <scope>/<name> | <scope>/<name>.<ordinal>]",
		Short: "Delete resources declared in manifests, an entire scope, a single resource, or one pod",
		Long: `delete removes resources from the controller's store. Five shapes:

  vd delete -f file.hcl            soft-delete every manifest in the file
                                   (containers + manifest + status; env vars
                                   and on-disk state are kept)
  vd delete -f file.hcl --prune    hard-delete: above + config bucket +
                                   /opt/voodu/apps/<app> + volumes

  vd delete <scope>                soft-wipe the entire scope: every manifest
                                   of every kind, scope-level config. Volumes
                                   and per-app config preserved (re-apply later
                                   to reattach to data).
  vd delete <scope> --prune        hard-wipe the scope: also nuke per-app
                                   config + volumes + on-disk state.

  vd delete <scope>/<name>         soft-delete the resource at (scope, name)
                                   across whatever kind owns it (auto-detect).
                                   ` + "`app`" + ` blocks delete both deployment + ingress
                                   in one shot.
  vd delete <scope>/<name> --prune hard-delete (also nukes config + volumes).

  vd delete <scope>/<name>.<N>     statefulset only — wipe pod ordinal N's
                                   container and let the reconciler recreate
                                   it. Useful for DR ("rebootstrap one
                                   replica without touching the cluster").
  vd delete <scope>/<name>.<N> --prune
                                   also wipes that pod's data volume so the
                                   wrapper bootstraps from scratch.

By default delete prints a plan listing every resource that will be
removed and asks y/N before issuing any DELETE. Pass --auto-approve
(alias -y) or set VOODU_AUTO_APPROVE=1 to skip the prompt in CI.
Non-interactive invocations without either will refuse to proceed.

Pass --dry-run to render the plan and exit without contacting the
controller — useful for confirming "is this the right manifest set?"
before committing to the destructive operation.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Dispatch by positional shape. Mixing -f with a positional
			// would be ambiguous (which wins?), so we reject up front.
			if len(args) == 1 {
				if len(f.files) > 0 {
					return fmt.Errorf("delete: pass either -f or a positional ref, not both")
				}

				ref := args[0]

				// `<scope>/<name>.<ordinal>` — per-pod delete
				if scope, name, ordinal, ok := parsePodRef(ref); ok {
					return runPodDelete(cmd, scope, name, ordinal, f)
				}

				// `<scope>/<name>` — kind-agnostic single resource
				if scope, name, ok := parseResourceRef(ref); ok {
					return runResourceDelete(cmd, scope, name, f)
				}

				// Bare `<scope>` — scope-wide wipe
				return runScopeWipe(cmd, ref, f)
			}

			return runDelete(cmd, f)
		},
	}

	cmd.Flags().StringArrayVarP(&f.files, "file", "f", nil, "manifest file (extension optional), directory, or - for stdin (repeatable)")
	cmd.Flags().StringVar(&f.format, "format", "", "stdin format: hcl or json (required for -f -)")
	cmd.Flags().BoolVarP(&f.autoApprove, "auto-approve", "y", false, "skip the interactive y/N confirmation (also VOODU_AUTO_APPROVE=1)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "render the plan and exit without deleting anything")
	cmd.Flags().BoolVar(&f.prune, "prune", false, "also wipe app config + on-disk state (env file, releases dir, volumes); REQUIRED for scope-wipe shape")

	return cmd
}

func runApply(cmd *cobra.Command, f applyFlags) error {
	// `vd apply` with no -f → discover an implicit target: Procfile
	// first, else the .voodu/ dir. Keeps the zero-arg "deploy this
	// project" ergonomics symmetric with the forwarded (remote) path.
	if len(f.files) == 0 {
		if discovered := discoverApplyFiles(); len(discovered) > 0 {
			f.files = discovered
		}
	}

	// Procfile mode: a `-f Procfile` (or --format procfile) input is not
	// HCL. This local path is reached only when NO remote is configured
	// (the remote path is intercepted in maybeForwardRemote before HCL
	// parsing). --eject works fully offline; the runtime transform needs
	// the server-side receive-pack fan-out, so a bare local apply with a
	// Procfile points the operator at the remote flow.
	if path, ok := procfilePathFromFiles(f.files, f.format); ok {
		scope, err := resolveProcfileScope(f.app, path)
		if err != nil {
			return err
		}

		pa := procfileApply{path: path, scope: scope, eject: f.eject, force: f.force, verbose: f.verbose}

		if f.eject {
			return runProcfileEject(pa, scope)
		}

		return fmt.Errorf("Procfile apply requires a configured remote: run `vd remote add <user@host>` then `vd apply -f %s`, or pass --eject to scaffold an HCL file locally", path)
	}

	mans, err := loadManifests(cmdBucketFetcher(cmd), f)
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
	//
	// `verbose` is a presentation knob for the forwarded path's
	// spinner — local apply has no build output to collapse.
	_ = f.autoApprove
	_ = f.force
	_ = f.verbose

	root := cmd.Root()

	body, err := json.Marshal(mans)
	if err != nil {
		return err
	}

	query := ""
	if f.applyPrune {
		query = "prune=true"
	}

	resp, err := controllerDo(root, http.MethodPost, "/apply", query, bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, raw)
	}

	// Route per-resource verdicts through progress.Reporter so NDJSON
	// clients see typed result events (kind/scope/name/action as
	// distinct fields) while legacy / local callers keep the plain
	// "deployment/softphone/web applied" wire format. The reporter
	// kind is picked from VOODU_PROTOCOL env — set by forwarded apply
	// clients speaking NDJSON, unset in local / legacy flows.
	reporter := progress.NewReporterFromEnv(os.Stdout)
	reporter.Hello()

	defer reporter.Close()

	for _, m := range mans {
		reporter.Result(string(m.Kind), m.Scope, m.Name, "applied")
	}

	// The controller returns {"data": {"applied": [...], "pruned":
	// [...], "plugin_installs": [...], "plugin_expansions": [...]}}.
	// Surface prune + plugin lifecycle so operators see what
	// happened beyond the per-manifest verdicts.
	var env struct {
		Data struct {
			Pruned           []string                `json:"pruned"`
			PluginInstalls   []controllerInstallRef  `json:"plugin_installs,omitempty"`
			PluginExpansions []controllerExpansionRef `json:"plugin_expansions,omitempty"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err == nil {
		// Plugin installs first — operators want to know "I got
		// voodu-postgres v0.2.0" before the per-resource lines
		// scroll past.
		for _, ins := range env.Data.PluginInstalls {
			suffix := ins.Plugin
			if ins.Version != "" {
				suffix += " " + ins.Version
			}

			if ins.Source != "" {
				suffix += " from " + ins.Source
			}

			reporter.Result("plugin", "", ins.Plugin, "installed "+suffix)
		}

		// Macro expansions: `expanded postgres/data/main →
		// statefulset/data/main`. Pretty-print so the operator
		// sees the lineage at a glance.
		for _, exp := range env.Data.PluginExpansions {
			reporter.Result("expand", "", exp.From, "expanded → "+strings.Join(exp.To, ", "))
		}

		for _, p := range env.Data.Pruned {
			// p is a pre-formatted "kind/scope/name" string from the
			// controller. Split back into parts so the typed Result
			// event carries structured fields — the text action is
			// the full "pruned (removed from manifests)" phrase to
			// match what the legacy client rendered.
			kind, scope, name := splitManifestRef(p)
			reporter.Result(kind, scope, name, "pruned (removed from manifests)")
		}
	}

	// Release-phase auto-trigger: after the apply succeeds, fire
	// `vd release run` for every deployment whose manifest carries
	// a `release { ... }` block. The server's reconciler skips
	// rolling restart for these deployments precisely so this CLI
	// orchestration can take over with streaming logs — operator
	// (and CI) sees the migration output flow live.
	//
	// Idempotency on the server side prevents double-execution if
	// the apply also triggers the release via some other path:
	// the second invocation finds a Succeeded record for the spec
	// hash and skips the run.
	for _, m := range mans {
		if !manifestHasReleaseBlock(m) {
			continue
		}

		ref := m.Name
		if m.Scope != "" {
			ref = m.Scope + "/" + m.Name
		}

		// releaseRunStreaming opens its own `-----> Releasing ...`
		// banner so the section sits in the spinner-friendly visual
		// vocabulary the build steps use. No header here would emit
		// a duplicate; let the streaming function own it.
		if err := releaseRunStreaming(cmd, ref); err != nil {
			return fmt.Errorf("release for %s: %w", ref, err)
		}
	}

	return nil
}

// manifestHasReleaseBlock returns true when the deployment's spec
// JSON contains a non-empty release block. Cheap parse — only the
// release.command field is inspected, which is enough to know that
// auto-trigger should fire (other fields don't change the
// trigger decision).
func manifestHasReleaseBlock(m controller.Manifest) bool {
	if m.Kind != controller.KindDeployment {
		return false
	}

	if len(m.Spec) == 0 {
		return false
	}

	var probe struct {
		Release *struct {
			Command     []string `json:"command,omitempty"`
			PreCommand  []string `json:"pre_command,omitempty"`
			PostCommand []string `json:"post_command,omitempty"`
		} `json:"release,omitempty"`
	}

	if err := json.Unmarshal(m.Spec, &probe); err != nil {
		return false
	}

	if probe.Release == nil {
		return false
	}

	// Empty release block (operator declared but didn't fill any
	// commands) is treated as "no release" — running an empty
	// command would be a confusing no-op.
	return len(probe.Release.Command) > 0 || len(probe.Release.PreCommand) > 0 || len(probe.Release.PostCommand) > 0
}

// splitManifestRef decomposes "kind/name" or "kind/scope/name" into
// parts. Mirrors formatRef's output shape so Result events carry the
// same identifiers the text path was concatenating.
func splitManifestRef(ref string) (kind, scope, name string) {
	parts := strings.SplitN(ref, "/", 3)

	switch len(parts) {
	case 2:
		return parts[0], "", parts[1]
	case 3:
		return parts[0], parts[1], parts[2]
	}

	return "", "", ref
}

func runDiff(cmd *cobra.Command, f applyFlags) error {
	local, err := loadManifests(cmdBucketFetcher(cmd), f)
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
	if f.applyPrune {
		query += "&prune=true"
	}

	resp, err := controllerDo(cmd.Root(), http.MethodPost, "/apply", query, bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, raw)
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

	// `voodu diff` is the dedicated detail-level inspector — operators
	// reach for it specifically to see field-by-field deltas. Compact
	// mode would defeat the purpose, so the diff command pins
	// verbose=true regardless of the apply spinner's --verbose flag.
	added, modified := renderApplyPlan(out, plan, palette, true)
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
// response and prints the resource header. Two rendering modes,
// chosen by the verbose flag:
//
//   verbose=true (also used by `voodu diff`):
//     ~ kind/scope/name              ← header
//         ~ field.path  "old"  →  "new"
//         + new.field   "v"
//     ...
//
//   verbose=false (default for `voodu apply` preview):
//     ~ kind/scope/name   field.path=new, another=v
//     + kind/scope/name   replicas=3 image=...
//     - kind/scope/name
//
// The compact mode is what the landing page mockup shows — one line
// per resource, eye scans down a column. Operators reach for verbose
// when they need exact old/new transitions during code review.
//
// Returns counts so the caller can produce the final summary line.
func renderApplyPlan(w io.Writer, plan diffResponse, p diffPalette, verbose bool) (added, modified int) {
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

		// Blank line between resources — verbose mode only, where each
		// resource emits a header + multiple field lines and the gap
		// helps the eye reset between blocks. Compact mode keeps lines
		// flush so a stack of one-liners reads as a single table.
		if verbose && !first {
			fmt.Fprintln(w)
		}

		first = false

		// Asset specs carry file bytes (`content` field as base64
		// for file() sources, raw string for inline). Two reasons
		// to redact them in the diff:
		//
		//   1. Security — assets can include ACL files with
		//      hashed passwords, TLS certs with fingerprints,
		//      configs with embedded tokens. Diff output
		//      lands in terminal scrollback, CI logs, screen
		//      shares — surfaces operators may not control.
		//   2. UX — base64-encoded files can be megabytes;
		//      diffing them inline buries the rest of the
		//      output. A `<sha256 N bytes>` summary tells
		//      the operator that a file changed without
		//      flooding the terminal.
		//
		// URLs and filenames pass through verbatim — those are
		// metadata operators want to see in the diff (URL
		// rotated? filename renamed?). Only `content` is
		// redacted.
		desiredSpec, currentSpec := desired.Spec, json.RawMessage(nil)
		if current != nil {
			currentSpec = current.Spec
		}

		if desired.Kind == controller.KindAsset {
			desiredSpec = redactAssetContent(desiredSpec)
			currentSpec = redactAssetContent(currentSpec)
		}

		if current == nil {
			if verbose {
				fmt.Fprintf(w, "%s %s (new)\n", p.Add("+"), label)
				renderResourceDiff(w, diffSpec(desiredSpec, nil), p)
			} else {
				renderResourceDiffCompact(w, label, diffSpec(desiredSpec, nil), p, '+')
			}

			added++

			continue
		}

		changes := diffSpec(desiredSpec, currentSpec)

		if len(changes) == 0 {
			fmt.Fprintf(w, "= %s (unchanged)\n", label)
			continue
		}

		if verbose {
			fmt.Fprintf(w, "%s %s\n", p.Mod("~"), label)
			renderResourceDiff(w, changes, p)
		} else {
			renderResourceDiffCompact(w, label, changes, p, '~')
		}

		modified++
	}

	return added, modified
}

// renderPrunePlan prints the footer block listing resources that
// WOULD be removed if --prune were active. Diff only invokes this
// when the operator passed --prune (otherwise the prune set is
// always empty by request), so the section header reminds them
// what they're seeing.
func renderPrunePlan(w io.Writer, pruned []string, p diffPalette) {
	if len(pruned) == 0 {
		return
	}

	fmt.Fprintln(w, "\n--- Would prune (--prune passed; remove flag to keep) ---")

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

// hasPluginMacro reports whether any manifest in the batch carries a
// non-core kind — i.e. one of the macro forms a plugin (`redis`,
// `postgres`, …) installed via `vd plugin install` expands at apply
// time. The probe is just `controller.ParseKind`: it accepts the six
// core kinds and rejects everything else, including plugin kinds.
//
// Used by runDelete to decide whether the per-manifest DELETE loop
// can run directly or whether the batch needs a server-side expand
// pass first.
func hasPluginMacro(mans []controller.Manifest) bool {
	for _, m := range mans {
		if _, err := controller.ParseKind(string(m.Kind)); err != nil {
			return true
		}
	}

	return false
}

// expandManifestsViaDryRun walks the batch through POST /apply?dry_run=true
// and returns the post-expand manifest list (the server's `applied` field).
// Plugin macros (`redis "..." "..." {}`) become their constituent core
// manifests (e.g. asset + statefulset for voodu-redis); core manifests
// pass through unchanged.
//
// Why the apply endpoint specifically:
//
//   - The expand pipeline is already wired there (expandPluginBlocks).
//     A dedicated /expand endpoint would duplicate the surface area; the
//     dry-run flag is the documented "compute the plan, don't write"
//     shape, and `applied` is the canonical post-expand list.
//   - Asset stamping runs too, which is harmless for delete (we ignore
//     the digest field — only kind/scope/name matters at the DELETE
//     query-param layer).
//
// One round-trip per `vd delete -f file.hcl` invocation when the file
// has any plugin macros. Files containing only core kinds skip this
// path entirely (hasPluginMacro returns false).
func expandManifestsViaDryRun(cmd *cobra.Command, mans []controller.Manifest) ([]controller.Manifest, error) {
	body, err := json.Marshal(mans)
	if err != nil {
		return nil, fmt.Errorf("marshal manifests: %w", err)
	}

	resp, err := controllerDo(cmd.Root(), http.MethodPost, "/apply", "dry_run=true&prune=false", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, formatControllerError(resp.StatusCode, raw)
	}

	var env struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
		Data   struct {
			Applied []controller.Manifest `json:"applied"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode dry-run response: %w", err)
	}

	if env.Status == "error" {
		return nil, fmt.Errorf("%s", env.Error)
	}

	if len(env.Data.Applied) == 0 {
		// Defensive: a successful response with an empty `applied`
		// slice would silently swallow every input. Surface it
		// instead of falling through to a no-op delete.
		return nil, fmt.Errorf("dry-run apply returned no expanded manifests (server may be on an older version)")
	}

	return env.Data.Applied, nil
}

func runDelete(cmd *cobra.Command, f applyFlags) error {
	// Mirrors runApply's shape: server-side / direct invocations don't
	// prompt. The y/N confirmation lives one layer up in
	// runDeleteForwarded — that's the only path where there's a real
	// user terminal to talk to. When runDelete is reached directly
	// (local mode without a remote, server-side over SSH, tests), it
	// just executes the deletion the caller already asked for.
	//
	// f.autoApprove is accepted on the cobra surface so the flag can
	// appear in argv without erroring out, but is otherwise ignored
	// here — same dance as runApply uses for its own --auto-approve.
	mans, err := loadManifests(cmdBucketFetcher(cmd), f)
	if err != nil {
		return err
	}

	if len(mans) == 0 {
		return fmt.Errorf("no manifests found")
	}

	// Macro expansion before the per-manifest DELETE loop. A `redis "..." "..." {}`
	// block in the file is a plugin macro; the controller's /desired store
	// doesn't hold a "redis" kind — only the asset + statefulset (or whatever
	// shape the plugin emits) that came out of `redis:expand`. Issuing
	// DELETE /apply?kind=redis would 400 with "unknown kind redis".
	//
	// Two-step solve:
	//
	//   1. Round-trip the manifest set through POST /apply?dry_run=true.
	//      The server runs every plugin's expand command and returns the
	//      post-expand list under `applied`. No /desired writes happen
	//      because dry_run=true.
	//   2. Use that expanded list as the iteration basis for the actual
	//      DELETEs. Each entry is now a guaranteed-core kind the existing
	//      delete handler accepts.
	//
	// Skipped when nothing in the batch is a plugin macro — the existing
	// fast path stays a single network call per manifest.
	if hasPluginMacro(mans) {
		expanded, err := expandManifestsViaDryRun(cmd, mans)
		if err != nil {
			return fmt.Errorf("expand plugin manifests for delete: %w", err)
		}

		mans = expanded
	}

	// Plan rendering lives in the orchestrator (runDeleteForwarded),
	// not here — same as runApply doesn't render a plan and lets the
	// orchestrator's diff phase handle the preview. Printing it again
	// server-side just duplicates output the user already saw before
	// approving. The exception is --dry-run: when the operator asked
	// for the plan only, we owe them the rendering.
	if f.dryRun {
		palette := newDiffPalette(os.Stdout)

		renderDeletePlan(os.Stdout, mans, palette)

		fmt.Fprintln(os.Stdout, "\nDry-run: no DELETE issued.")

		return nil
	}

	root := cmd.Root()

	for _, m := range mans {
		q := url.Values{}
		q.Set("kind", string(m.Kind))
		q.Set("name", m.Name)

		if m.Scope != "" {
			q.Set("scope", m.Scope)
		}

		if f.prune {
			q.Set("prune", "true")
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
			return fmt.Errorf("delete %s/%s: %s", m.Kind, m.Name, formatControllerError(resp.StatusCode, raw))
		default:
			if f.prune {
				fmt.Printf("- %s/%s deleted (pruned)\n", m.Kind, m.Name)
			} else {
				fmt.Printf("- %s/%s deleted\n", m.Kind, m.Name)
			}
		}
	}

	return nil
}

// runScopeWipe is the `vd delete <scope> [--prune]` path: wipe every
// manifest in a scope across every kind, plus scope-level config.
// Hits DELETE /scope on the controller. With --prune, also nukes
// per-app config + filesystem + volumes; without it, those are
// preserved so an operator can re-apply later and reattach to data.
//
// Soft-wipe is the default because "clear every manifest, keep my
// data volumes" is the common dev/test reset cycle. Hard-wipe
// stays opt-in via --prune.
func runScopeWipe(cmd *cobra.Command, scope string, f applyFlags) error {
	if f.dryRun {
		mode := "soft-wipe"
		if f.prune {
			mode = "hard-wipe (--prune: also drops per-app config + volumes)"
		}

		fmt.Fprintf(os.Stdout, "Would %s scope %q (every manifest + scope config).\nDry-run: no DELETE issued.\n", mode, scope)

		return nil
	}

	root := cmd.Root()

	q := url.Values{}
	q.Set("scope", scope)

	if f.prune {
		q.Set("prune", "true")
	}

	resp, err := controllerDo(root, http.MethodDelete, "/scope", q.Encode(), nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, raw)
	}

	// Server returns the destructive summary so the operator sees
	// exactly what got nuked vs. what failed (filesystem permissions,
	// stale containers, etc.). Keep the rendering compact — the
	// transcript matters most when something went wrong.
	var env struct {
		Data struct {
			Scope            string `json:"scope"`
			ResourcesWiped   []struct {
				Kind   string `json:"kind"`
				Name   string `json:"name"`
				Pruned struct {
					ConfigWiped   bool     `json:"config_wiped"`
					AppDirRemoved string   `json:"app_dir_removed"`
					VolumeRemoved string   `json:"volume_removed"`
					Errors        []string `json:"errors"`
				} `json:"pruned"`
			} `json:"resources_wiped"`
			ScopeConfigWiped bool     `json:"scope_config_wiped"`
			Errors           []string `json:"errors"`
		} `json:"data"`
	}

	_ = json.Unmarshal(raw, &env)

	for _, w := range env.Data.ResourcesWiped {
		fmt.Printf("- %s/%s/%s wiped\n", w.Kind, scope, w.Name)

		for _, e := range w.Pruned.Errors {
			fmt.Printf("  ! %s\n", e)
		}
	}

	if env.Data.ScopeConfigWiped {
		fmt.Printf("- scope/%s config wiped\n", scope)
	}

	for _, e := range env.Data.Errors {
		fmt.Printf("! %s\n", e)
	}

	if len(env.Data.ResourcesWiped) == 0 && env.Data.ScopeConfigWiped {
		fmt.Println("(no manifests in scope; only scope-level config existed and was wiped)")
	}

	return nil
}

// parseResourceRef matches the `<scope>/<name>` shape — scope and
// name with no further suffix. Used by `vd delete <scope>/<name>`
// to route to the kind-agnostic single-resource delete endpoint.
//
// Returns ok=false for shapes the caller should NOT treat as a
// resource ref:
//
//   - bare `<scope>` (no slash) — scope-wide wipe
//   - `<scope>/<name>.<ordinal>` — per-pod (parsePodRef catches it
//     first; this function returns false to avoid a double-match)
//   - more than one slash, or empty segments — malformed
func parseResourceRef(ref string) (scope, name string, ok bool) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}

	scope, name = parts[0], parts[1]
	if scope == "" || name == "" {
		return "", "", false
	}

	// Per-pod refs end with `.<digits>`. Defer to parsePodRef so
	// the caller doesn't have to track which match wins.
	if _, _, _, isPod := parsePodRef(ref); isPod {
		return "", "", false
	}

	return scope, name, true
}

// parsePodRef matches `<scope>/<name>.<ordinal>` — statefulset
// per-pod ref. The ordinal is a non-negative integer suffix
// separated by a literal dot from the resource name. Returns
// ok=false for any other shape so parseResourceRef can take
// non-pod refs.
func parsePodRef(ref string) (scope, name string, ordinal int, ok bool) {
	slash := strings.IndexByte(ref, '/')
	if slash < 0 {
		return "", "", 0, false
	}

	scope = ref[:slash]
	rest := ref[slash+1:]

	dot := strings.LastIndexByte(rest, '.')
	if dot < 0 {
		return "", "", 0, false
	}

	name = rest[:dot]
	ordStr := rest[dot+1:]

	if scope == "" || name == "" || ordStr == "" {
		return "", "", 0, false
	}

	n, err := strconv.Atoi(ordStr)
	if err != nil || n < 0 {
		return "", "", 0, false
	}

	return scope, name, n, true
}

// runResourceDelete handles `vd delete <scope>/<name> [-y] [--prune]`.
// Hits DELETE /resource on the controller, which scans every
// scoped kind for a match and deletes each one (an `app` block
// emits both deployment + ingress so a single ref deletes both).
func runResourceDelete(cmd *cobra.Command, scope, name string, f applyFlags) error {
	if f.dryRun {
		mode := "soft-delete"
		if f.prune {
			mode = "hard-delete (--prune: also drops config + volumes)"
		}

		fmt.Fprintf(os.Stdout, "Would %s resource %s/%s.\nDry-run: no DELETE issued.\n", mode, scope, name)

		return nil
	}

	root := cmd.Root()

	q := url.Values{}
	q.Set("scope", scope)
	q.Set("name", name)

	if f.prune {
		q.Set("prune", "true")
	}

	resp, err := controllerDo(root, http.MethodDelete, "/resource", q.Encode(), nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, raw)
	}

	var env struct {
		Data struct {
			ResourcesWiped []struct {
				Kind   string `json:"kind"`
				Pruned struct {
					Errors []string `json:"errors"`
				} `json:"pruned"`
			} `json:"resources_wiped"`
			Errors []string `json:"errors"`
		} `json:"data"`
	}

	_ = json.Unmarshal(raw, &env)

	for _, w := range env.Data.ResourcesWiped {
		fmt.Printf("- %s/%s/%s deleted\n", w.Kind, scope, name)

		for _, e := range w.Pruned.Errors {
			fmt.Printf("  ! %s\n", e)
		}
	}

	for _, e := range env.Data.Errors {
		fmt.Printf("! %s\n", e)
	}

	if len(env.Data.ResourcesWiped) == 0 {
		fmt.Printf("(no resource found at %s/%s)\n", scope, name)
	}

	return nil
}

// runPodDelete handles `vd delete <scope>/<name>.<ordinal> [-y] [--prune]`.
// Statefulset-only — wipes one pod's container (and optionally
// its volume), then triggers reconcile so the missing ordinal
// comes back from spec. The wrapper's first-boot path runs if
// --prune was passed (volume freshly empty).
func runPodDelete(cmd *cobra.Command, scope, name string, ordinal int, f applyFlags) error {
	if f.dryRun {
		mode := "soft-delete"
		if f.prune {
			mode = "hard-delete (--prune: also wipes volume → fresh bootstrap on recreate)"
		}

		fmt.Fprintf(os.Stdout, "Would %s pod %s/%s ordinal %d (statefulset only).\nDry-run: no DELETE issued.\n",
			mode, scope, name, ordinal)

		return nil
	}

	root := cmd.Root()

	q := url.Values{}
	q.Set("scope", scope)
	q.Set("name", name)
	q.Set("ordinal", strconv.Itoa(ordinal))

	if f.prune {
		q.Set("prune", "true")
	}

	resp, err := controllerDo(root, http.MethodDelete, "/resource", q.Encode(), nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, raw)
	}

	var env struct {
		Data struct {
			Summary struct {
				ContainerRemoved string   `json:"container_removed"`
				VolumesRemoved   []string `json:"volumes_removed"`
				Errors           []string `json:"errors"`
			} `json:"summary"`
		} `json:"data"`
	}

	_ = json.Unmarshal(raw, &env)

	if env.Data.Summary.ContainerRemoved != "" {
		fmt.Printf("- container %s removed\n", env.Data.Summary.ContainerRemoved)
	}

	for _, v := range env.Data.Summary.VolumesRemoved {
		fmt.Printf("- volume %s removed\n", v)
	}

	for _, e := range env.Data.Summary.Errors {
		fmt.Printf("! %s\n", e)
	}

	fmt.Printf("- statefulset/%s/%s ordinal %d will be recreated by the reconciler\n",
		scope, name, ordinal)

	return nil
}

// renderDeletePlan prints the "will delete" preview block. Same red
// `-` marker as `voodu diff`'s prune section so the visual language
// stays consistent — operators already learned that "red minus =
// going away" from apply diffs.
//
// Exported (lowercase but package-visible) so the forwarded
// orchestrator can render the same plan client-side before SSHing.
func renderDeletePlan(w io.Writer, mans []controller.Manifest, p diffPalette) {
	noun := "resource"
	if len(mans) != 1 {
		noun = "resources"
	}

	fmt.Fprintf(w, "Will delete %d %s:\n\n", len(mans), noun)

	for _, m := range mans {
		fmt.Fprintf(w, "  %s %s\n", p.Del("-"), formatRef(m.Kind, m.Scope, m.Name))
	}
}

// promptDeleteConfirm is the destructive-operation cousin of
// promptConfirm. Same y/N shape (default = no), but the wording
// makes it explicit what's being asked — "Apply these changes?" and
// "Delete these resources?" reading the same on a glance was a
// recipe for muscle-memory accidents.
func promptDeleteConfirm(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprint(out, "\nDelete these resources? [y/N]: ")

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}

	fmt.Fprintln(out)

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}

	return false, nil
}

// loadManifests expands every -f argument (file, dir, stdin) into a flat
// list, applying ${VAR} interpolation from the current environment.
//
// When a manifest declares `env_from = [...]`, the referenced config
// buckets are fetched from the controller and layered into the
// interpolation context BEFORE `${VAR}` substitution runs. Shell env
// wins on collision — same posture as the runtime env_from path
// where spec.env overrides env_from layers. See apply_envfrom.go for
// the bucket-fetch + merge mechanics.
//
// `fetch` resolves env_from buckets for interpolation — cmdBucketFetcher
// (HTTP) for the local/server-side path, sshBucketFetcher for the
// client side of an SSH-forwarded apply. nil-tolerant: a nil fetcher
// skips env_from enrichment (offline / pure-shell interpolation).
func loadManifests(fetch bucketFetcher, f applyFlags) ([]controller.Manifest, error) {
	if len(f.files) == 0 {
		return nil, fmt.Errorf("at least one -f is required")
	}

	shellEnv := envAsMap()
	cache := newBucketCache()

	var out []controller.Manifest

	for _, path := range f.files {
		mans, err := loadOne(fetch, path, f.format, shellEnv, cache)
		if err != nil {
			return nil, err
		}

		out = append(out, mans...)
	}

	return out, nil
}

func loadOne(fetch bucketFetcher, path, stdinFormat string, shellEnv map[string]string, cache *bucketCache) ([]controller.Manifest, error) {
	if path == "-" {
		if stdinFormat == "" {
			return nil, fmt.Errorf("-f -: --format hcl is required for stdin")
		}

		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}

		env, err := enrichEnvFor(fetch, "<stdin>", raw, shellEnv, cache)
		if err != nil {
			return nil, err
		}

		mans, err := manifest.ParseReader(bytes.NewReader(raw), manifest.Format(stdinFormat), env)
		if err != nil {
			return nil, err
		}

		// stdin has no manifest-file path; env_file references resolve
		// against the operator's current working directory.
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}

		return mergeEnvFilesInManifests(mans, cwd)
	}

	resolved, info, err := resolveManifestPath(path)
	if err != nil {
		return nil, err
	}

	if info.IsDir() {
		// Directory case: walk files ourselves so each one gets its
		// own env_from enrichment pass (one file might `env_from`
		// "prod/shared", another "prod/worker-creds"; we don't want
		// to globally union them across the whole dir). The cache
		// dedup across files in the same apply session anyway.
		mans, err := loadDir(fetch, resolved, shellEnv, cache)
		if err != nil {
			return nil, err
		}

		return mergeEnvFilesInManifests(mans, resolved)
	}

	raw, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", resolved, err)
	}

	env, err := enrichEnvFor(fetch, resolved, raw, shellEnv, cache)
	if err != nil {
		return nil, err
	}

	mans, err := manifest.ParseFile(resolved, env)
	if err != nil {
		return nil, err
	}

	// docker-compose semantics: env_file paths resolve relative to the
	// directory of the manifest file (NOT the operator's CWD). That way
	// `voodu apply -f apps/web/web.voodu` finds `apps/web/.env` next to
	// the manifest, regardless of where the operator ran from.
	return mergeEnvFilesInManifests(mans, filepath.Dir(resolved))
}

// resolveManifestPath locates the manifest file for a `-f` argument,
// adding a manifest extension when the user omitted one AND preferring
// voodu's project home `.voodu/`.
//
// Search order, first hit wins:
//   1. `.voodu/<path>`  (+ `.hcl/.voodu/.vdu/.vd`)
//   2. `<path>`         (+ `.hcl/.voodu/.vdu/.vd`)  ← today's behaviour
//
// So `vd apply -f web` finds `.voodu/web.voodu` if present, else a
// root-level `web.voodu`. `vd apply -f infra/web` → `.voodu/infra/web.*`
// first, else `infra/web.*`. Keeping manifests under `.voodu/` (next to
// app.json) declutters the repo root and pairs with `--eject`, which
// writes there. Absolute paths and stdin ("-") skip the `.voodu/` prefix.
func resolveManifestPath(path string) (string, os.FileInfo, error) {
	bases := []string{path}
	if path != "-" && !filepath.IsAbs(path) {
		bases = []string{filepath.Join(vooduDir, path), path}
	}

	for _, base := range bases {
		// Exact match first (path already carries an extension, or names
		// a directory), then extension expansion.
		for _, ext := range []string{"", ".hcl", ".voodu", ".vdu", ".vd"} {
			candidate := base + ext

			if info, err := os.Stat(candidate); err == nil {
				return candidate, info, nil
			}
		}
	}

	// Fall back to the original path so the error message names what the
	// user typed, not a prefix/extension they didn't write.
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
//
// NOT for streaming endpoints (`/logs?follow=true`, future SSE, etc.) —
// `http.Client.Timeout` is the TOTAL request budget INCLUDING body read,
// so a 30s ceiling here kills any long-lived stream at the 30s mark with
// `context deadline exceeded`. Streaming callers should use
// `controllerStream` below.
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

// controllerStream is the streaming sibling of controllerDo. It honours
// the same controller-URL resolution but installs NO overall request
// timeout — the body is read for as long as the controller keeps writing,
// up to whatever ctx (typically `cmd.Context()`) cancels.
//
// Per-step transport guards stay in place (15s to connect + read response
// headers); only the body-read budget is unbounded. That keeps "controller
// is reachable but never sends headers" surfaceable as an error while
// letting a healthy `?follow=true` tail run indefinitely.
//
// Used by `vd logs -f` (multi-pod multiplex) and any future SSE / chunked
// endpoint added to the CLI. Apply/diff/delete keep `controllerDo` — they
// have an actual request body that should fit in the 30s budget.
func controllerStream(ctx context.Context, root *cobra.Command, method, path, rawQuery string, body io.Reader) (*http.Response, error) {
	base := strings.TrimRight(controllerURL(root), "/")
	full := base + path

	if rawQuery != "" {
		full += "?" + rawQuery
	}

	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	req.Header.Set("User-Agent", fmt.Sprintf("voodu-cli/%s", version))

	// Transport-level guards keep the failure-mode useful without ever
	// trimming the body-read budget — `Client.Timeout` is left zero.
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 15 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 15 * time.Second,
	}

	client := &http.Client{Transport: transport}

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

