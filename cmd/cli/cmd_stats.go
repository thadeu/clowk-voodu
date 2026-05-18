// cmd_stats.go is the operator surface of `vd stats` — a `docker
// stats` analog scoped to voodu-managed pods, joined with the
// configured limits from the manifest's resources block.
//
// Flow: parse positional ref or explicit --kind/--scope/--name →
// build a StatsFilter → GET /stats → render text table or pipe
// JSON. All the actual work (docker, manifest lookup, join) lives
// in the controller's StatsCollector; this file is purely the CLI
// I/O boundary.
//
// Positional ref shapes (matches `vd logs` / `vd get` conventions):
//
//	vd stats                             every running pod
//	vd stats clowk-lp                    every pod in scope clowk-lp
//	vd stats clowk-lp/web                every replica of clowk-lp/web
//	vd stats deployment                  every deployment (any scope)
//	vd stats deployment/clowk-lp/web     explicit kind path
//
// Single-segment refs disambiguate: known kinds (deployment,
// statefulset, job, cronjob) map to --kind, anything else to --scope.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"go.voodu.clowk.in/internal/controller"
)

// statsFlags carries the per-invocation knobs. Refs go through
// parsing into the same kind/scope/name fields, so the controller
// query string assembly is one path regardless of how the operator
// specified the filter.
type statsFlags struct {
	kind    string
	scope   string
	name    string
	orphans bool
}

// knownKinds is the set the positional-ref parser disambiguates
// against. A single-segment ref matching one of these is treated
// as --kind; anything else falls through to --scope. Kept local
// because the dispatch is CLI-specific — controller-side
// validation has its own ParseKind.
var knownKinds = map[string]bool{
	"deployment":  true,
	"statefulset": true,
	"job":         true,
	"cronjob":     true,
	"ingress":     true,
	"asset":       true,
}

func newStatsCmd() *cobra.Command {
	var f statsFlags

	cmd := &cobra.Command{
		Use:   "stats [ref]",
		Short: "Show live CPU/memory usage joined with configured limits",
		Long: `stats prints a table of every running voodu-managed container's CPU
and memory usage alongside the limits declared in its manifest's
resources block. Single-shot (` + "`docker stats --no-stream`" + `
semantics); pipe to ` + "`watch`" + ` if you want a refresh loop.

Output columns:
  KIND          deployment | statefulset | job | cronjob
  REF           scope/name.replica
  CPU%          host-relative (matches docker stats — 100% = one full core)
  MEM USED      bytes consumed (RSS, excludes page cache)
  MEM LIMIT     manifest's resources.limits.memory (— = unbounded)
  MEM%          USED / LIMIT * 100 (the docker-reported value)
  CPU LIMIT     manifest's resources.limits.cpu (— = unbounded)

Filtering:

  vd stats                             every running pod
  vd stats clowk-lp                    every pod in scope clowk-lp
  vd stats clowk-lp/web                every replica of clowk-lp/web
  vd stats deployment                  every deployment (any scope)
  vd stats deployment/clowk-lp/web     explicit kind/scope/name

Explicit flags (precedence: positional shape > flags. Pass either, not both):

  -k/--kind     deployment | statefulset | job | cronjob | ingress
  -s/--scope    scope filter
  -n/--name     resource name filter

Other:

  --orphans     include containers without a matching manifest
                (legacy pre-M0 pods or leaks where the manifest was
                deleted but the container survived)
  -o text|json  output format (default text)

Examples:
  vd stats                                              # full table
  vd stats clowk-lp/web                                 # one app
  vd stats deployment --orphans                         # plus any leaks
  vd stats -o json | jq '.[].usage.memory_percent'      # composable`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if f.kind != "" || f.scope != "" || f.name != "" {
					return fmt.Errorf("pass either a positional ref or --kind/--scope/--name, not both")
				}

				k, s, n, err := parseStatsRef(args[0])
				if err != nil {
					return err
				}

				f.kind, f.scope, f.name = k, s, n
			}

			return runStats(cmd, f)
		},
	}

	cmd.Flags().StringVarP(&f.kind, "kind", "k", "", "filter by kind (deployment, statefulset, job, cronjob)")
	cmd.Flags().StringVarP(&f.scope, "scope", "s", "", "filter by scope")
	cmd.Flags().StringVarP(&f.name, "name", "n", "", "filter by resource name")
	cmd.Flags().BoolVar(&f.orphans, "orphans", false, "include containers without a matching manifest (legacy / leaks)")

	return cmd
}

// parseStatsRef interprets the positional ref into (kind, scope,
// name) per the disambiguation rules documented on the command:
//
//   - "kind/scope/name" → three segments, all populated
//   - "scope/name"      → two segments, kind empty
//   - "deployment"      → one segment, looked up against knownKinds:
//                          match → kind, miss → scope
//   - ""                → all empty (no filter)
//
// Returns an error only for malformed input (empty segments,
// 4+ segments). Behaviour is defensive: a typo in a single-segment
// ref silently falls through as "scope" and produces an empty
// table, which the operator can then re-issue with the right
// vocabulary — no crash, no surprise reframing of intent.
func parseStatsRef(ref string) (kind, scope, name string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", "", nil
	}

	parts := strings.Split(ref, "/")
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			return "", "", "", fmt.Errorf("ref %q has empty segment", ref)
		}
	}

	switch len(parts) {
	case 1:
		if knownKinds[parts[0]] {
			return parts[0], "", "", nil
		}

		return "", parts[0], "", nil

	case 2:
		return "", parts[0], parts[1], nil

	case 3:
		if !knownKinds[parts[0]] {
			return "", "", "", fmt.Errorf("ref %q: first segment must be a known kind (deployment, statefulset, job, cronjob, ingress, asset)", ref)
		}

		return parts[0], parts[1], parts[2], nil

	default:
		return "", "", "", fmt.Errorf("ref %q: too many segments (max 3: kind/scope/name)", ref)
	}
}

// statsResponse mirrors the /stats envelope. Kept local so the CLI
// only depends on the controller's PodStats wire shape, not on the
// API envelope wrapper.
type statsResponse struct {
	Status string `json:"status"`
	Data   struct {
		Pods []controller.PodStats `json:"pods"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

func runStats(cmd *cobra.Command, f statsFlags) error {
	root := cmd.Root()

	q := url.Values{}

	if f.kind != "" {
		q.Set("kind", f.kind)
	}

	if f.scope != "" {
		q.Set("scope", f.scope)
	}

	if f.name != "" {
		q.Set("name", f.name)
	}

	if f.orphans {
		q.Set("orphans", "true")
	}

	resp, err := controllerDo(root, http.MethodGet, "/stats", q.Encode(), nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, raw)
	}

	var env statsResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode stats: %w", err)
	}

	if env.Status == "error" {
		return fmt.Errorf("%s", env.Error)
	}

	switch outputFormat(root) {
	case "json":
		out := json.NewEncoder(os.Stdout)
		out.SetIndent("", "  ")

		return out.Encode(env.Data.Pods)

	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(env.Data.Pods)
	}

	return renderStatsTable(os.Stdout, env.Data.Pods)
}

// renderStatsTable prints the operator-facing view: one row per
// pod, columns aligned via tabwriter so the numbers line up.
// Orphan rows are marked with "(orphan)" in the KIND column so
// they're scannable without breaking the column structure.
//
// "—" in a column means "no data" (no limit configured for that
// dimension, or stats unavailable). Using a dash rather than "N/A"
// or "-" keeps the eye flowing past unset cells instead of
// pausing on them.
func renderStatsTable(w io.Writer, pods []controller.PodStats) error {
	if len(pods) == 0 {
		fmt.Fprintln(w, "No running pods matched the filter.")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintln(tw, "KIND\tREF\tCPU%\tMEM USED\tMEM LIMIT\tMEM%\tCPU LIMIT")

	for _, p := range pods {
		kind := p.Identity.Kind
		if kind == "" {
			kind = "(orphan)"
		} else if p.Orphan {
			kind = kind + " (orphan)"
		}

		ref := formatStatsRef(p)

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			kind,
			ref,
			formatPercent(p.Usage.CPUPercent),
			formatBytes(p.Usage.MemoryUsageBytes),
			formatLimitMemory(p.Limits),
			formatPercent(p.Usage.MemoryPercent),
			formatLimitCPU(p.Limits),
		)
	}

	return tw.Flush()
}

// formatStatsRef builds the visible "scope/name.replica" reference.
// Falls back to the container name when identity is incomplete
// (orphan case) — the operator can still grep for the row.
func formatStatsRef(p controller.PodStats) string {
	if p.Identity.Name == "" {
		return p.ContainerName
	}

	ref := p.Identity.Name

	if p.Identity.Scope != "" {
		ref = p.Identity.Scope + "/" + ref
	}

	if p.Identity.ReplicaID != "" {
		ref = ref + "." + p.Identity.ReplicaID
	}

	return ref
}

// formatPercent emits "47.5%" or "—" for unset. One decimal place
// matches docker stats output; more is noise for an eyeball
// reading.
func formatPercent(v float64) string {
	if v == 0 {
		return "—"
	}

	return fmt.Sprintf("%.1f%%", v)
}

// formatBytes emits "120MiB" / "1.5GiB" etc — same scale ladder
// docker uses for readability. Stops at GiB (anything past that
// is unusual for a single container and the precision drop helps
// the table fit narrower terminals).
func formatBytes(b uint64) string {
	if b == 0 {
		return "—"
	}

	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)

	switch {
	case b >= GiB:
		return fmt.Sprintf("%.1fGiB", float64(b)/float64(GiB))
	case b >= MiB:
		return fmt.Sprintf("%.1fMiB", float64(b)/float64(MiB))
	case b >= KiB:
		return fmt.Sprintf("%.1fKiB", float64(b)/float64(KiB))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// formatLimitMemory echoes the operator's verbatim string (e.g.
// "254Mi") when available. Empty → "—" (no limit declared).
// Showing the manifest text rather than reformatting it to bytes
// reinforces "this is what YOU wrote" — operators recognise their
// own units faster than a recomputed number.
func formatLimitMemory(l controller.LimitStats) string {
	if l.Memory == "" {
		return "—"
	}

	return l.Memory
}

// formatLimitCPU mirrors formatLimitMemory for the CPU field.
// Echoes the operator's input ("0.4", "500m") verbatim.
func formatLimitCPU(l controller.LimitStats) string {
	if l.CPU == "" {
		return "—"
	}

	return l.CPU
}
