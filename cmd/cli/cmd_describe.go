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
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"go.voodu.clowk.in/internal/controller"
)

// newDescribeCmd builds `voodu describe <kind> <ref>`. Mirrors the
// kubectl-style verb so operators have one place to ask "what's going
// on with this thing?" — manifest, status blob, matching pods, run
// history (when applicable), all in one screen.
func newDescribeCmd() *cobra.Command {
	var showEnv bool

	cmd := &cobra.Command{
		Use:     "describe <kind> <ref>",
		Aliases: []string{"desc"},
		Short:   "Show detailed state of one resource (manifest + status + pods)",
		Long: `describe asks the controller for everything it knows about one
declared resource: the source manifest, the persisted status blob,
and any voodu-managed containers matching its (kind, scope, name)
identity.

<kind> is one of: deployment, database, ingress, job, cronjob, pod.
<ref>  is "<scope>/<name>" for scoped kinds, or "<name>" for an
unambiguous match. Unscoped kinds (database) take "<name>" only.

The "pod" (alias "pd") kind is the runtime view of a single
voodu-managed container. Its <ref> is the container name as it
appears in 'voodu get pods' (e.g. test-web.a3f9) — pods don't share
the kind/scope/name shape because more than one replica can match
the same identity.

For 'describe pod', env vars are listed by name only (values hidden).
Pass --show-env to reveal values — useful when actively debugging,
risky on a screen-share or in a recorded terminal session.

Examples:
  voodu describe deployment clowk/web
  voodu describe job api/migrate
  voodu describe cronjob ops/purge
  voodu describe database main
  voodu describe pod test-web.a3f9
  voodu desc pd test-web.a3f9 --show-env

Output formats:
  -o text  (default) human-friendly summary, no raw spec dump
  -o spec  text view + the manifest spec as pretty JSON
  -o json  raw envelope as JSON (machine-readable)
  -o yaml  raw envelope as YAML (machine-readable)`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDescribe(cmd, args[0], args[1], describeOptions{showEnv: showEnv})
		},
	}

	cmd.Flags().BoolVar(&showEnv, "show-env", false,
		"reveal env var values for 'describe pod' (default: list names only)")

	return cmd
}

// describeOptions threads command-level flags into the runners
// without polluting every helper's signature with bool soup. Today
// only describe pod cares about showEnv; future flags (e.g.
// --no-history for jobs) can join here.
type describeOptions struct {
	showEnv bool
}

// describeOutputMode reads --output and maps it onto the four modes
// describe actually understands. "spec" is describe-specific (no other
// command emits a spec dump), so we resolve it locally instead of
// polluting the shared outputFormat helper.
func describeOutputMode(root *cobra.Command) string {
	v, _ := root.PersistentFlags().GetString("output")

	switch strings.ToLower(strings.TrimSpace(v)) {
	case "json":
		return "json"
	case "yaml", "yml":
		return "yaml"
	case "spec":
		return "spec"
	default:
		return "text"
	}
}

// describeResponse mirrors the /describe envelope. Manifest is a
// pointer so an absent value (server-side 404, not currently
// happening but defensible) decodes cleanly.
type describeResponse struct {
	Status string `json:"status"`
	Data   struct {
		Manifest *controller.Manifest `json:"manifest"`
		Status   json.RawMessage      `json:"status,omitempty"`
		Pods     []controller.Pod     `json:"pods,omitempty"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

func runDescribe(cmd *cobra.Command, kindStr, ref string, opts describeOptions) error {
	// "pod" / "pd" is the runtime-only kind. It doesn't go through the
	// Kind enum (no manifest, no scope/name shape) — its <ref> is the
	// container name and it has its own dedicated endpoint.
	switch strings.ToLower(strings.TrimSpace(kindStr)) {
	case "pod", "pd":
		return runDescribePod(cmd, ref, opts)
	}

	kind, err := controller.ParseKind(kindStr)
	if err != nil {
		return err
	}

	scope, name := splitJobRef(ref)

	if name == "" {
		return fmt.Errorf("ref %q: name is empty", ref)
	}

	// Unscoped kinds must not carry a scope in the ref. Be strict so
	// a typo like "database/main" isn't silently treated as
	// scope="database", name="main" (which would 404 confusingly).
	if !controller.IsScoped(kind) && scope != "" {
		return fmt.Errorf("kind %s is unscoped; pass bare name (got %q)", kind, ref)
	}

	q := url.Values{}
	q.Set("kind", string(kind))
	q.Set("name", name)

	if scope != "" {
		q.Set("scope", scope)
	}

	root := cmd.Root()

	resp, err := controllerDo(root, http.MethodGet, "/describe", q.Encode(), nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var env describeResponse
	if jsonErr := json.Unmarshal(raw, &env); jsonErr != nil {
		return fmt.Errorf("decode response (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if env.Status == "error" || resp.StatusCode >= 400 {
		if env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return fmt.Errorf("controller returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	mode := describeOutputMode(root)

	switch mode {
	case "json":
		out := json.NewEncoder(os.Stdout)
		out.SetIndent("", "  ")

		return out.Encode(env.Data)

	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(env.Data)
	}

	// text + spec both render through the same path; `spec` just flips
	// on the raw spec dump section.
	return renderDescribe(os.Stdout, env.Data.Manifest, env.Data.Status, env.Data.Pods, mode == "spec")
}

// renderDescribe is the text-mode formatter. Header + per-kind
// summary + (optional) raw spec dump + pods table + history table.
// Each section silently elides itself when empty so a freshly-applied
// resource (no status, no pods) renders cleanly.
//
// showSpec is the `-o spec` toggle. The raw JSON dump is opt-in
// because the per-kind summaries already surface every field that
// matters to a human operator — dumping the spec by default just
// duplicates information, like the cronjob's `schedule` appearing in
// both the summary line and the JSON below it.
func renderDescribe(w io.Writer, manifest *controller.Manifest, statusBlob json.RawMessage, pods []controller.Pod, showSpec bool) error {
	if manifest == nil {
		return fmt.Errorf("empty response: no manifest returned")
	}

	if manifest.Scope != "" {
		fmt.Fprintf(w, "%s/%s/%s\n", manifest.Kind, manifest.Scope, manifest.Name)
	} else {
		fmt.Fprintf(w, "%s/%s\n", manifest.Kind, manifest.Name)
	}

	// Per-kind summary lines. When statusBlob is empty (just-applied,
	// reconciler hasn't run yet) the per-kind renderer prints "(no
	// status recorded yet)" so the operator knows it's not a missing
	// field but a missing record.
	//
	// Job and cronjob summaries take the manifest as well — most of the
	// scheduling/runtime knobs live in the spec, not the status, and
	// duplicating them via a JSON dump below was the duplication that
	// motivated this refactor.
	switch manifest.Kind {
	case controller.KindDeployment:
		renderDeploymentSummary(w, statusBlob)

	case controller.KindDatabase:
		renderDatabaseSummary(w, statusBlob)

	case controller.KindIngress:
		renderIngressSummary(w, statusBlob)

	case controller.KindJob:
		renderJobSummary(w, manifest, statusBlob)

	case controller.KindCronJob:
		renderCronJobSummary(w, manifest, statusBlob)
	}

	// Raw spec dump — opt-in via `-o spec`. Operators who need to eyeball
	// the manifest as it sits in etcd ask for it explicitly; the default
	// text view stays focused on derived/runtime fields that a JSON
	// dump can't pretty-print well (next_run, history, pods).
	if showSpec && len(manifest.Spec) > 0 {
		fmt.Fprintln(w, "\nspec:")

		var pretty bytes.Buffer

		if err := json.Indent(&pretty, manifest.Spec, "  ", "  "); err == nil {
			fmt.Fprintf(w, "  %s\n", pretty.String())
		} else {
			fmt.Fprintf(w, "  %s\n", string(manifest.Spec))
		}
	}

	// Pods section — only render when there's something to show.
	// Plugin-managed kinds (ingress = caddy on the host, database =
	// plugin-owned containers without our labels) typically come back
	// empty; skipping the heading keeps the output uncluttered.
	if len(pods) > 0 {
		fmt.Fprintf(w, "\npods (%d):\n", len(pods))

		if err := renderDescribePodsTable(w, pods); err != nil {
			return err
		}
	}

	// History section for kinds that record run history.
	switch manifest.Kind {
	case controller.KindJob:
		renderJobHistory(w, statusBlob)

	case controller.KindCronJob:
		renderCronJobHistory(w, statusBlob)
	}

	return nil
}

// renderDescribePodsTable prints the same columns as `voodu get pods`,
// minus the kind/scope/name (already in the describe header) — the
// extra context would just push the useful columns off the screen.
func renderDescribePodsTable(w io.Writer, pods []controller.Pod) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintln(tw, "  NAME\tREPLICA\tIMAGE\tSTATUS\tCREATED")

	for _, p := range pods {
		status := p.Status
		if status == "" {
			if p.Running {
				status = "running"
			} else {
				status = "stopped"
			}
		}

		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			p.Name, dashIfEmpty(p.ReplicaID), p.Image, status, p.CreatedAt)
	}

	return tw.Flush()
}

// --- Per-kind summary renderers -------------------------------------

func renderDeploymentSummary(w io.Writer, blob json.RawMessage) {
	var st controller.DeploymentStatus
	if !decodeStatus(w, blob, &st) {
		return
	}

	fmt.Fprintf(w, "  image:     %s\n", dashIfEmpty(st.Image))
	fmt.Fprintf(w, "  spec_hash: %s\n", dashIfEmpty(st.SpecHash))
}

func renderDatabaseSummary(w io.Writer, blob json.RawMessage) {
	var st controller.DatabaseStatus
	if !decodeStatus(w, blob, &st) {
		return
	}

	fmt.Fprintf(w, "  engine:  %s\n", dashIfEmpty(st.Engine))
	fmt.Fprintf(w, "  version: %s\n", dashIfEmpty(st.Version))

	if len(st.Data) > 0 {
		fmt.Fprintln(w, "  data:")

		for k, v := range st.Data {
			fmt.Fprintf(w, "    %s: %v\n", k, v)
		}
	}

	if len(st.Params) > 0 {
		fmt.Fprintln(w, "  params:")

		for k, v := range st.Params {
			fmt.Fprintf(w, "    %s: %s\n", k, v)
		}
	}
}

func renderIngressSummary(w io.Writer, blob json.RawMessage) {
	var st controller.IngressStatus
	if !decodeStatus(w, blob, &st) {
		return
	}

	fmt.Fprintf(w, "  plugin: %s\n", dashIfEmpty(st.Plugin))

	if len(st.Data) > 0 {
		fmt.Fprintln(w, "  data:")

		for k, v := range st.Data {
			fmt.Fprintf(w, "    %s: %v\n", k, v)
		}
	}
}

// renderJobSummary now reads the manifest too so command/timeout
// (which used to be visible only via the raw spec dump) make it into
// the summary. Image is taken from the manifest as the canonical
// "what will the next run use" value; status.Image is shown only when
// it disagrees, signalling a pending reconcile.
func renderJobSummary(w io.Writer, manifest *controller.Manifest, blob json.RawMessage) {
	spec := decodeJobSpecLocal(manifest.Spec)

	fmt.Fprintf(w, "  image:    %s\n", dashIfEmpty(spec.Image))

	if cmd := strings.Join(spec.Command, " "); cmd != "" {
		fmt.Fprintf(w, "  command:  %s\n", cmd)
	}

	if spec.Timeout != "" {
		fmt.Fprintf(w, "  timeout:  %s\n", spec.Timeout)
	}

	if n := len(spec.Env); n > 0 {
		fmt.Fprintf(w, "  env:      %d var(s)\n", n)
	}

	var st controller.JobStatus
	if !decodeStatus(w, blob, &st) {
		return
	}

	if st.Image != "" && st.Image != spec.Image {
		fmt.Fprintf(w, "  image (last run): %s\n", st.Image)
	}

	fmt.Fprintf(w, "  last_run: %s\n", formatTimePtr(st.LastRun))
	fmt.Fprintf(w, "  history:  %d run(s)\n", len(st.History))
}

// renderCronJobSummary needs the manifest because next_run is computed
// fresh from the schedule rather than persisted. The status blob's
// LastRun is still authoritative since "when did the last tick fire"
// is observed history, not a derived value.
//
// Spec fields (image, command, timeout, history limits, schedule,
// timezone, suspend, concurrency_policy) all surface here so that
// dropping the raw spec dump from the default text view doesn't lose
// any operator-relevant detail.
func renderCronJobSummary(w io.Writer, manifest *controller.Manifest, blob json.RawMessage) {
	var st controller.CronJobStatus

	hasStatus := decodeStatus(w, blob, &st)

	spec := decodeCronJobSpecLocal(manifest.Spec)

	fmt.Fprintf(w, "  schedule:    %s\n", dashIfEmpty(spec.Schedule))

	tz := spec.Timezone
	if tz == "" {
		tz = "UTC"
	}

	fmt.Fprintf(w, "  timezone:    %s\n", tz)
	fmt.Fprintf(w, "  suspended:   %t\n", spec.Suspend)

	cp := spec.ConcurrencyPolicy
	if cp == "" {
		cp = "Allow"
	}

	fmt.Fprintf(w, "  concurrency: %s\n", cp)

	// Compute next_run client-side from the live schedule so a fresh
	// describe always shows the upcoming fire even if the controller
	// hasn't ticked yet. Suspend means no upcoming fire — say so.
	if !spec.Suspend && spec.Schedule != "" {
		if sched, err := controller.ParseSchedule(spec.Schedule, spec.Timezone); err == nil {
			next := sched.Next(time.Now())
			fmt.Fprintf(w, "  next_run:    %s\n", next.UTC().Format(time.RFC3339))
		}
	} else if spec.Suspend {
		fmt.Fprintln(w, "  next_run:    — (suspended)")
	}

	// Embedded job container detail. The cronjob's "what runs" lives
	// under spec.job — same shape as a job's spec — and the operator
	// shouldn't have to flip to `-o spec` to see what command will fire.
	fmt.Fprintf(w, "  image:       %s\n", dashIfEmpty(spec.Job.Image))

	if cmdLine := strings.Join(spec.Job.Command, " "); cmdLine != "" {
		fmt.Fprintf(w, "  command:     %s\n", cmdLine)
	}

	if spec.Job.Timeout != "" {
		fmt.Fprintf(w, "  timeout:     %s\n", spec.Job.Timeout)
	}

	if n := len(spec.Job.Env); n > 0 {
		fmt.Fprintf(w, "  env:         %d var(s)\n", n)
	}

	// History caps default to "use the system default" when zero — show
	// only if the operator set them explicitly (non-zero), otherwise the
	// line just adds noise.
	if spec.SuccessfulHistoryLimit > 0 || spec.FailedHistoryLimit > 0 {
		fmt.Fprintf(w, "  history limits: success=%d, failed=%d\n",
			spec.SuccessfulHistoryLimit, spec.FailedHistoryLimit)
	}

	if hasStatus {
		if st.Image != "" && st.Image != spec.Job.Image {
			fmt.Fprintf(w, "  image (last run): %s\n", st.Image)
		}

		fmt.Fprintf(w, "  last_run:    %s\n", formatTimePtr(st.LastRun))
		fmt.Fprintf(w, "  history:     %d run(s)\n", len(st.History))
	}
}

// --- History table renderers ----------------------------------------

func renderJobHistory(w io.Writer, blob json.RawMessage) {
	var st controller.JobStatus
	if !decodeStatusSilent(blob, &st) || len(st.History) == 0 {
		return
	}

	fmt.Fprintf(w, "\nhistory (%d, newest first):\n", len(st.History))

	renderRunsTable(w, st.History)
}

func renderCronJobHistory(w io.Writer, blob json.RawMessage) {
	var st controller.CronJobStatus
	if !decodeStatusSilent(blob, &st) || len(st.History) == 0 {
		return
	}

	fmt.Fprintf(w, "\nhistory (%d, newest first):\n", len(st.History))

	renderRunsTable(w, st.History)
}

// renderRunsTable is the shared run-history formatter. Same columns
// for jobs and cronjobs so an operator's eye doesn't have to relearn
// the layout when switching between kinds.
func renderRunsTable(w io.Writer, runs []controller.JobRun) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintln(tw, "  RUN_ID\tSTATUS\tEXIT\tDURATION\tSTARTED")

	for _, r := range runs {
		duration := "-"
		if !r.EndedAt.IsZero() && !r.StartedAt.IsZero() {
			duration = r.EndedAt.Sub(r.StartedAt).Round(time.Millisecond).String()
		}

		started := "-"
		if !r.StartedAt.IsZero() {
			started = r.StartedAt.UTC().Format(time.RFC3339)
		}

		fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\t%s\n",
			r.RunID, r.Status, r.ExitCode, duration, started)
	}

	_ = tw.Flush()
}

// --- Helpers --------------------------------------------------------

// decodeStatus prints "(no status recorded yet)" when blob is empty so
// a fresh apply renders intelligibly, returning false to let the caller
// skip status-derived fields. Decode failures are surfaced verbatim.
func decodeStatus(w io.Writer, blob json.RawMessage, into any) bool {
	if len(blob) == 0 || string(blob) == "null" {
		fmt.Fprintln(w, "  (no status recorded yet)")
		return false
	}

	if err := json.Unmarshal(blob, into); err != nil {
		fmt.Fprintf(w, "  (status decode failed: %v)\n", err)
		return false
	}

	return true
}

// decodeStatusSilent is for the history table — when the status is
// missing we elide the whole section rather than printing an empty
// header.
func decodeStatusSilent(blob json.RawMessage, into any) bool {
	if len(blob) == 0 || string(blob) == "null" {
		return false
	}

	return json.Unmarshal(blob, into) == nil
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

func formatTimePtr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}

	return t.UTC().Format(time.RFC3339)
}

// jobSpecView and cronJobSpecView are local CLI-side mirrors of the
// controller's jobSpec / cronJobSpec types. The controller types are
// unexported (no import-cycle reason — they're just internal to the
// reconciler), so we re-declare the shape here. Only the fields the
// describe summary actually shows are listed; new fields the operator
// should see can be added one-by-one.
type jobSpecView struct {
	Image   string            `json:"image"`
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

type cronJobSpecView struct {
	Schedule          string      `json:"schedule"`
	Job               jobSpecView `json:"job"`
	ConcurrencyPolicy string      `json:"concurrency_policy,omitempty"`
	Timezone          string      `json:"timezone,omitempty"`
	Suspend           bool        `json:"suspend,omitempty"`

	SuccessfulHistoryLimit int `json:"successful_history_limit,omitempty"`
	FailedHistoryLimit     int `json:"failed_history_limit,omitempty"`
}

// decodeJobSpecLocal returns the zero value on decode failure rather
// than propagating the error. The describe summary degrades gracefully
// (most fields print "-") and the raw spec is still reachable via
// `-o spec` for forensic inspection.
func decodeJobSpecLocal(blob json.RawMessage) jobSpecView {
	var v jobSpecView

	if len(blob) > 0 {
		_ = json.Unmarshal(blob, &v)
	}

	return v
}

func decodeCronJobSpecLocal(blob json.RawMessage) cronJobSpecView {
	var v cronJobSpecView

	if len(blob) > 0 {
		_ = json.Unmarshal(blob, &v)
	}

	return v
}

// --- Pod describe ---------------------------------------------------

// describePodResponse mirrors the /pods/{name} envelope. Pod is a
// pointer so 404 (no body) decodes cleanly to nil — though in practice
// the controller emits an error envelope for that case.
type describePodResponse struct {
	Status string `json:"status"`
	Data   struct {
		Pod *controller.PodDetail `json:"pod"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// runDescribePod fetches GET /pods/{name} and renders the rich detail.
// `ref` here is the docker container name (e.g. "test-web.a3f9") —
// pods don't share the kind/scope/name shape because more than one
// replica can match the same identity.
//
// Output formats follow the same -o text|json|yaml|spec contract as
// the other describe variants. "spec" falls through to text since
// there's no manifest spec to dump for a runtime container.
//
// opts.showEnv toggles env-var value visibility in text mode. JSON
// and YAML modes pass the server response through verbatim — anyone
// asking for machine-readable output is presumed to know they're
// getting the full dump.
func runDescribePod(cmd *cobra.Command, ref string, opts describeOptions) error {
	ref = strings.TrimSpace(ref)

	if ref == "" {
		return fmt.Errorf("pod name is empty")
	}

	// Defensive: a slash here means the operator typed "pod scope/name"
	// expecting the same ref shape as the other kinds. Tell them
	// explicitly what we expect — pods are addressed by container name.
	if strings.Contains(ref, "/") {
		return fmt.Errorf("pod ref %q contains a slash — pods are addressed by container name (e.g. test-web.a3f9), not scope/name", ref)
	}

	root := cmd.Root()

	// PathEscape the name in case it contains characters URL-special
	// characters. Container names from voodu are safe (alphanum + dash
	// + dot), but the legacy / non-voodu path could surface anything.
	resp, err := controllerDo(root, http.MethodGet, "/pods/"+url.PathEscape(ref), "", nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var env describePodResponse
	if jsonErr := json.Unmarshal(raw, &env); jsonErr != nil {
		return fmt.Errorf("decode response (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if env.Status == "error" || resp.StatusCode >= 400 {
		if env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return fmt.Errorf("controller returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if env.Data.Pod == nil {
		return fmt.Errorf("empty response: no pod detail returned")
	}

	switch describeOutputMode(root) {
	case "json":
		out := json.NewEncoder(os.Stdout)
		out.SetIndent("", "  ")

		return out.Encode(env.Data)

	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(env.Data)
	}

	// "spec" falls through to text — runtime containers have no
	// manifest spec to dump. The text view is already complete.
	return renderPodDetail(os.Stdout, env.Data.Pod, opts.showEnv)
}

// renderPodDetail prints the runtime view of one container as
// per-section blocks. Each section silently elides itself when
// empty so a freshly-created pod (no networks attached yet, no
// mounts) renders cleanly.
//
// Section order roughly tracks "what does the operator look at
// first": identity → state → image → command → networks → ports →
// mounts → env. Env is last because it's typically the longest
// section and pushes everything else off-screen if printed earlier.
//
// showEnv toggles env-var value visibility. Default false because
// env vars routinely carry secrets (DATABASE_URL with a password,
// API keys, JWT secrets) and a screen-share / recorded session is
// the worst place to discover that. Operators who need the values
// pass --show-env explicitly; everyone else gets a name-only listing
// they can scan for "is FOO_BAR set?" without leaking the value.
func renderPodDetail(w io.Writer, p *controller.PodDetail, showEnv bool) error {
	// Header: container name. When voodu identity labels are present
	// we prefix with kind/scope/name so the operator knows which
	// declared resource this replica belongs to.
	if p.Pod.Kind != "" {
		if p.Pod.Scope != "" {
			fmt.Fprintf(w, "pod %s/%s/%s (%s)\n",
				p.Pod.Kind, p.Pod.Scope, p.Pod.ResourceName, p.Pod.Name)
		} else {
			fmt.Fprintf(w, "pod %s/%s (%s)\n",
				p.Pod.Kind, p.Pod.ResourceName, p.Pod.Name)
		}
	} else {
		fmt.Fprintf(w, "pod %s\n", p.Pod.Name)
		fmt.Fprintln(w, "  (no voodu identity labels — legacy or non-voodu container)")
	}

	if p.Pod.ReplicaID != "" {
		fmt.Fprintf(w, "  replica:        %s\n", p.Pod.ReplicaID)
	}

	if p.ID != "" {
		shortID := p.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}

		fmt.Fprintf(w, "  container_id:   %s\n", shortID)
	}

	fmt.Fprintf(w, "  image:          %s\n", dashIfEmpty(p.Pod.Image))

	// State block. "status" alone (e.g. "exited") is ambiguous on its
	// own — pair with running flag and exit code so the operator gets
	// the whole story in one line. CreatedAt comes from the voodu
	// label (when present) so it survives across re-inspects.
	fmt.Fprintf(w, "  status:         %s\n", dashIfEmpty(p.State.Status))
	fmt.Fprintf(w, "  running:        %t\n", p.State.Running)

	if !p.State.Running {
		fmt.Fprintf(w, "  exit_code:      %d\n", p.State.ExitCode)
	}

	if p.State.StartedAt != "" {
		fmt.Fprintf(w, "  started_at:     %s\n", p.State.StartedAt)
	}

	if !p.State.Running && p.State.FinishedAt != "" {
		fmt.Fprintf(w, "  finished_at:    %s\n", p.State.FinishedAt)
	}

	if p.State.Restarts > 0 {
		fmt.Fprintf(w, "  restart_count:  %d\n", p.State.Restarts)
	}

	if p.RestartPolicy != "" {
		fmt.Fprintf(w, "  restart_policy: %s\n", p.RestartPolicy)
	}

	if p.Pod.CreatedAt != "" {
		fmt.Fprintf(w, "  created_at:     %s\n", p.Pod.CreatedAt)
	}

	if p.WorkingDir != "" {
		fmt.Fprintf(w, "  working_dir:    %s\n", p.WorkingDir)
	}

	if cmdLine := strings.Join(p.Command, " "); cmdLine != "" {
		fmt.Fprintf(w, "  command:        %s\n", cmdLine)
	}

	if entry := strings.Join(p.Entrypoint, " "); entry != "" {
		fmt.Fprintf(w, "  entrypoint:     %s\n", entry)
	}

	// Networks: render each attached network with its IP and any
	// aliases. Aliases are how docker DNS routes service names within
	// a network; surfacing them helps debug "why can't web reach db".
	if len(p.Networks) > 0 {
		// Stable order so the rendering is deterministic across runs —
		// docker returns the map in random iteration order otherwise.
		names := make([]string, 0, len(p.Networks))
		for n := range p.Networks {
			names = append(names, n)
		}

		sort.Strings(names)

		fmt.Fprintf(w, "\nnetworks (%d):\n", len(p.Networks))

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NETWORK\tIP\tGATEWAY\tALIASES")

		for _, n := range names {
			net := p.Networks[n]

			aliases := strings.Join(net.Aliases, ",")
			if aliases == "" {
				aliases = "-"
			}

			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				n, dashIfEmpty(net.IPAddress), dashIfEmpty(net.Gateway), aliases)
		}

		_ = tw.Flush()
	}

	// Ports: docker renders these as "container/proto" → "host:port".
	// Empty bindings (port exposed but not published) still appear so
	// the operator can see what's reachable in-network.
	if len(p.Ports) > 0 {
		fmt.Fprintf(w, "\nports (%d):\n", len(p.Ports))

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  CONTAINER\tHOST")

		for _, port := range p.Ports {
			host := "-"
			if port.HostPort != "" {
				if port.HostIP != "" {
					host = port.HostIP + ":" + port.HostPort
				} else {
					host = port.HostPort
				}
			}

			fmt.Fprintf(tw, "  %s\t%s\n", port.Container, host)
		}

		_ = tw.Flush()
	}

	// Mounts: bind mounts and named volumes. RW flag matters because
	// a read-only mount can silently break apps that try to write.
	if len(p.Mounts) > 0 {
		fmt.Fprintf(w, "\nmounts (%d):\n", len(p.Mounts))

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  TYPE\tSOURCE\tDESTINATION\tMODE")

		for _, m := range p.Mounts {
			mode := m.Mode
			if mode == "" {
				if m.RW {
					mode = "rw"
				} else {
					mode = "ro"
				}
			}

			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				dashIfEmpty(m.Type), dashIfEmpty(m.Source), dashIfEmpty(m.Destination), mode)
		}

		_ = tw.Flush()
	}

	// Env last. Both names and values are hidden by default — even key
	// names leak intent (STRIPE_SECRET_KEY, AWS_ACCESS_KEY_ID, etc. tell
	// an attacker watching a screen-share what to look for next). Only
	// the count is surfaced so the operator knows env vars exist; the
	// --show-env opt-in reveals the full list when actively debugging.
	if len(p.Env) > 0 {
		if !showEnv {
			fmt.Fprintf(w, "\nenv: %d var(s) hidden (pass --show-env to reveal)\n", len(p.Env))
			return nil
		}

		keys := make([]string, 0, len(p.Env))
		for k := range p.Env {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		fmt.Fprintf(w, "\nenv (%d):\n", len(p.Env))

		for _, k := range keys {
			fmt.Fprintf(w, "  %s=%s\n", k, p.Env[k])
		}
	}

	return nil
}
