package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"go.voodu.clowk.in/internal/containers"
	"go.voodu.clowk.in/internal/paths"
	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// errScopeNotFound is returned by resolveScope when no manifest matches
// (kind, name) in any scope. applyDelete maps it to 404; ambiguous
// matches get a different error and map to 400.
var errScopeNotFound = errors.New("not found in any scope")

// API is the HTTP surface of the controller. Handlers are thin: decode,
// call into Store or a subsystem, encode. Business logic lives in the
// packages the handlers delegate to.
type API struct {
	Store Store
	// Version is reported by /health.
	Version string

	// PluginsRoot is the filesystem directory where plugins live. When
	// empty, /exec and /plugins endpoints return "no plugin system"
	// errors — used by unit tests that don't care about plugins.
	PluginsRoot string

	// NodeName and EtcdClient are passed to plugins via environment so
	// plugin authors can reach back into the cluster if they need to.
	NodeName   string
	EtcdClient string

	// ControllerURL is the HTTP base URL plugins use to call back
	// into the controller (GET /describe, GET /config, etc.) when
	// they need platform state. Set by the server wiring from the
	// configured HTTPAddr — typically http://127.0.0.1:8686 since
	// plugins run on the same host as the controller. Empty when
	// not configured (tests, or when plugins don't need callback).
	ControllerURL string

	// Invoker is the shared plugin-execution seam — the reconciler uses
	// the same interface for its handlers, so /exec and reconcile-time
	// calls go through one code path (env injection, plugin resolution,
	// envelope parsing). Nil means /exec falls back to loading plugins
	// directly from PluginsRoot; production wires DirInvoker.
	Invoker PluginInvoker

	// Pods is the source of /pods listings. Nil means the endpoint
	// falls back to a default DockerPodsLister; tests inject a fake
	// to avoid shelling out to docker.
	Pods PodsLister

	// Jobs powers /jobs/run — the imperative entry point for executing
	// a declared job. Nil means the endpoint returns 503 ("job runner
	// not configured"); production wires JobHandler. Kept as an
	// interface so tests can stub the runner without spinning up
	// docker.
	Jobs JobRunner

	// CronJobs powers /cronjobs/run — the imperative "trigger this
	// cronjob NOW, don't wait for the schedule" entry point. Nil
	// means the endpoint returns 503; production wires CronJobHandler.
	// Same shape as JobRunner so tests can re-use a single fake.
	CronJobs CronJobRunner

	// Execer powers /pods/{name}/exec — the kubectl-exec-style "run a
	// command inside a running container" path. Nil → 503. Production
	// wires DockerContainerManager (which satisfies the interface via
	// its Exec method); tests substitute a fake to avoid spinning up
	// a real docker daemon.
	Execer Execer

	// Deployments powers `POST /restart?kind=deployment&...` — the
	// imperative rolling-restart entry point. Nil → 503. Production
	// wires *DeploymentHandler (its Restart method matches the
	// signature). Tests substitute a fake to avoid spinning up
	// reconciler machinery.
	Deployments DeploymentRestarter

	// Statefulsets powers `POST /restart?kind=statefulset&...` and
	// `POST /rollback?kind=statefulset&...`. Nil → 503 for those
	// kinds. Production wires *StatefulsetHandler.
	Statefulsets StatefulsetRestarter

	// Logs powers /logs streaming. Nil means the endpoint returns 503
	// ("log streaming not configured"); production wires the
	// ContainerManager (which satisfies LogStreamer through its Logs
	// method). Kept as a single-method interface so tests don't need
	// to implement the whole ContainerManager just to assert the
	// streamer is wired correctly.
	Logs LogStreamer

	// PodLifecycle powers `POST /pods/{name}/stop` and
	// `POST /pods/{name}/start` — the imperative pod-lifecycle entry
	// points used by `vd stop` / `vd start`. Nil → 503. Production
	// wires DockerContainerManager (which already has Stop/Start +
	// InspectLabels methods).
	PodLifecycle PodLifecycler

	// PluginBlocks resolves plugin-block kinds (`postgres { … }`,
	// `redis { … }`) to the on-disk plugin that expands them into
	// core kinds. Nil → no plugin support; non-core kinds 400 with
	// a "no plugin registered" error. Production wires
	// *DirPluginRegistry pointed at PluginsRoot.
	PluginBlocks PluginBlockRegistry

	// PluginInstaller is the JIT-install seam: when a manifest
	// references a plugin block kind that has no matching plugin
	// under PluginsRoot, the apply path attempts to install it
	// from the convention repo `thadeu/voodu-<kind>` (or an
	// override via VOODU_PLUGIN_REPO_<KIND> env / block
	// `_repo` attribute) before failing the apply. Nil disables
	// JIT install — operators must pre-install plugins in that
	// case via `vd plugins:install <name>`.
	PluginInstaller *plugins.Installer
}

// LogStreamer is the seam /logs dispatches through. The production
// implementation is ContainerManager (its Logs method matches the
// signature exactly); tests substitute a fake reader.
type LogStreamer interface {
	Logs(name string, opts LogsOptions) (io.ReadCloser, error)
}

// JobRunner is the seam /jobs/run dispatches through. JobHandler is
// the production implementation; tests substitute a fake to avoid
// docker.
type JobRunner interface {
	RunOnce(ctx context.Context, scope, name string) (JobRun, error)
}

// CronJobRunner is the seam /cronjobs/run dispatches through.
// CronJobHandler.Tick implements this — same shape as the scheduler
// uses when a tick fires, but called imperatively from the CLI to
// force a run "now". Distinct from JobRunner because the underlying
// store key + handler differ (KindCronJob vs KindJob), even though
// the user-facing semantics are nearly identical.
type CronJobRunner interface {
	Tick(ctx context.Context, scope, name string) (JobRun, error)
}

// Execer is the seam /pods/{name}/exec dispatches through. Production:
// DockerContainerManager.Exec. Tests: a fake that captures the
// command + opts. Stream wiring is intentionally on the value (not
// the interface method) so a single Exec call can attach to the
// hijacked HTTP connection without buffering.
type Execer interface {
	Exec(name string, command []string, opts ExecOptions) (int, error)
}

// PodLifecycler is the seam `POST /pods/{name}/stop` and
// `POST /pods/{name}/start` dispatch through. DockerContainerManager
// satisfies this via its Stop/Start methods; tests substitute a fake
// to record calls without touching docker.
//
// InspectLabels is part of the surface because the freeze annotation
// keys off the container's voodu.* labels — the API needs to recover
// (kind, scope, name, ordinal) before it can persist the freeze.
// Bundling it here keeps the seam coherent: one fake fits all three.
type PodLifecycler interface {
	Stop(name string) error
	Start(name string) error
	InspectLabels(name string) (map[string]string, error)
}

// DeploymentRestarter is the seam `POST /restart?kind=deployment&...`
// dispatches through. *DeploymentHandler.Restart implements this in
// production; tests stub it to avoid touching docker.
//
// Release / Rollback live on the same surface so a single field on
// API covers all three M-5/M-6 verbs against deployments. Release
// takes an output writer so the HTTP handler can stream container
// logs live to the response body — operators (and CI) see migration
// output flow in real-time.
type DeploymentRestarter interface {
	Restart(ctx context.Context, scope, name string) error
	Release(ctx context.Context, scope, name string, output io.Writer) error
	Rollback(ctx context.Context, scope, name, targetID string) (newID string, err error)
}

// StatefulsetRestarter is the statefulset twin — same Restart and
// Rollback semantics, but NO Release method (statefulset workloads
// are databases / queues / caches, not apps with migration steps).
// The API dispatch picks this surface when `kind=statefulset` lands
// on /restart or /rollback.
//
// Volumes is the read-side seam used by handleDescribe to surface
// per-(claim, ordinal) docker volume names alongside the pod
// listing. Returns the volume names matching this statefulset;
// empty slice on lister failure or unconfigured Containers.
type StatefulsetRestarter interface {
	Restart(ctx context.Context, scope, name string) error
	Rollback(ctx context.Context, scope, name, targetID string) (newID string, err error)
	PruneVolumes(scope, name string) ([]string, error)
	Volumes(scope, name string) ([]string, error)
}

// ExecOptions mirrors docker.ExecOptions field-for-field but lives
// in the controller package so callers (handlers, CLI when running
// in-process) don't need to import internal/docker just to pass
// options through. The handler converts to docker.ExecOptions
// before invoking the runtime — that's the only translation point.
type ExecOptions struct {
	TTY         bool
	Interactive bool
	WorkingDir  string
	User        string
	Env         []string

	// Cols/Rows describe the operator's terminal size. Forwarded to
	// pty.Setsize on the docker side when TTY=true; ignored otherwise.
	Cols uint16
	Rows uint16

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Handler returns an http.Handler with all endpoints registered.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/apply", a.handleApply)
	mux.HandleFunc("/describe", a.handleDescribe)
	mux.HandleFunc("/pods", a.handlePods)
	mux.HandleFunc("GET /pods/{name}", a.handlePodDescribe)
	mux.HandleFunc("POST /plugins/exec", a.handleExec)
	mux.HandleFunc("POST /pods/{name}/exec", a.handlePodExec)
	mux.HandleFunc("GET /pods/{name}/logs", a.handlePodLogs)
	mux.HandleFunc("POST /pods/{name}/stop", a.handlePodStop)
	mux.HandleFunc("POST /pods/{name}/start", a.handlePodStart)
	mux.HandleFunc("/plugins", a.handlePlugins)
	mux.HandleFunc("POST /plugins/install", a.handlePluginInstall)
	mux.HandleFunc("DELETE /plugins/{name}", a.handlePluginRemove)
	mux.HandleFunc("POST /plugin/{name}/{command}", a.handlePluginCommand)
	mux.HandleFunc("POST /jobs/run", a.handleJobRun)
	mux.HandleFunc("POST /cronjobs/run", a.handleCronJobRun)
	mux.HandleFunc("/config", a.handleConfig)
	mux.HandleFunc("POST /restart", a.handleRestart)
	mux.HandleFunc("POST /releases/run", a.handleReleaseRun)
	mux.HandleFunc("POST /rollback", a.handleRollback)
	mux.HandleFunc("DELETE /scope", a.handleScopeWipe)

	return logRequests(mux)
}

// envelope is the response shape used for /exec and /plugins. Matches
// the plugin JSON protocol so the CLI can render any of them uniformly.
type envelope struct {
	Status string `json:"status"`
	Data   any    `json:"data,omitempty"`
	Error  string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	// SetEscapeHTML(false): keep `<`, `>`, `&` literal in the JSON
	// output. Default Go encoder escapes them to `<`, `>`,
	// `&` for HTML-embed safety — we never embed in HTML, and
	// the escapes show up verbatim when the CLI prints an error
	// like `need at least <plugin> <command>`. The literal angle
	// brackets are far more readable for the operator.
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, envelope{Status: "error", Error: err.Error()})
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": a.Version,
	})
}

// handleApply accepts POST (upsert) / GET (list) / DELETE (remove).
// POST body is either a single manifest or a JSON array of manifests.
// DELETE takes ?kind=<k>&name=<n>.
func (a *API) handleApply(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		a.applyPost(w, r)
	case http.MethodGet:
		a.applyGet(w, r)
	case http.MethodDelete:
		a.applyDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))
	}
}

func (a *API) applyPost(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	manifests, err := decodeManifests(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	if len(manifests) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("no manifests in body"))
		return
	}

	// Two ingresses pointing at the same Host produce duplicate caddy
	// automation policies, which caddy rejects at /load with an opaque
	// error. Refuse here so the operator sees a clean "host already in
	// use" message instead of a reconcile-time cascade.
	if err := a.checkIngressHostCollisions(r.Context(), manifests); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// dry_run=true makes this a plan-only call: `voodu diff` uses it to
	// ask "what would happen if I applied this?" without touching the
	// store. We still run every validation (host collisions already
	// ran above) and fetch each input's current on-disk manifest so
	// the CLI can render a proper field-by-field diff client-side.
	dryRun := r.URL.Query().Get("dry_run") == "true"

	// `?prune=false` is the escape hatch for shared-scope setups where
	// several independent applies (different repos, different pipelines)
	// each declare only a slice of the scope. Default remains prune=on —
	// that's the source-of-truth contract most mono-repo users want, and
	// renames don't leave zombies behind. See README "Shared scope" for
	// the intended usage pattern.
	prune := r.URL.Query().Get("prune") != "false"

	// Plugin-block expansion: any manifest whose Kind is not a
	// core kind gets expanded by an installed plugin into one or
	// more core-kind manifests. Operator wrote `postgres "data"
	// "main" { … }` → expand to `statefulset "data" "main" { … }`.
	// Failure modes:
	//
	//   - No plugin installed for that kind: JIT install attempts
	//     `thadeu/voodu-<kind>` (or block-level _repo override),
	//     fails 400 if even that doesn't resolve.
	//   - Plugin's expand command failed: 400 with the plugin's
	//     stderr / envelope error verbatim — operator sees the
	//     real cause without spelunking.
	//
	// Done before the dry-run branch so `vd diff` shows the
	// EXPANDED manifests (the operator wants to see the
	// statefulset that will materialise, not the postgres macro).
	expandedManifests, expansions, installs, err := a.expandPluginBlocks(r.Context(), manifests)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	manifests = expandedManifests

	// Asset-digest stamping. Walks the post-expand batch, hashes
	// every asset's bytes, and embeds the resulting digests under
	// `_asset_digests` on each consumer's spec (deployment,
	// statefulset, job, cronjob). The hash function later reads
	// from there instead of doing a /status side-channel lookup,
	// so:
	//
	//   - The diff is honest: when an asset's bytes change, the
	//     consumer's stamped digest changes, and the consumer's
	//     /desired blob therefore textually differs.
	//   - Restart triggers via the normal watch path: /desired
	//     changed → handler runs → spec hash drift → rolling
	//     restart. No fan-out from asset → consumer needed.
	//   - Cross-batch refs work: a consumer in this apply can
	//     reference an asset already in /status from a previous
	//     apply, and stamping pulls the prior digest.
	//
	// Estrita on unresolved refs: if a consumer references an
	// asset that's neither in the batch nor in /status, the apply
	// rejects with the formatted ref in the error. Catches typos
	// and missing-asset cases at apply time instead of at
	// container-start-time.
	if err := StampAssetDigests(r.Context(), a.Store, nil, nil, manifests); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("asset stamping: %w", err))
		return
	}

	var (
		applied = make([]*Manifest, 0, len(manifests))
		current = make([]*Manifest, 0, len(manifests))
	)

	for _, m := range manifests {
		// Capture the existing on-disk manifest BEFORE we overwrite it,
		// so diff can compare "was vs. will be". Nil slot = a create.
		// Unscoped kinds use empty scope; Store.Get handles that.
		before, err := a.Store.Get(r.Context(), m.Kind, m.Scope, m.Name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("read current: %w", err))
			return
		}

		current = append(current, before)

		if dryRun {
			applied = append(applied, m)
			continue
		}

		stored, err := a.Store.Put(r.Context(), m)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}

		applied = append(applied, stored)
	}

	var pruned []pruneTarget

	if prune {
		targets, err := a.computePruneTargets(r.Context(), manifests)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("prune plan: %w", err))
			return
		}

		pruned = targets

		if !dryRun {
			if err := a.applyPrune(r.Context(), pruned); err != nil {
				writeErr(w, http.StatusInternalServerError, fmt.Errorf("prune: %w", err))
				return
			}
		}
	}

	prunedRefs := make([]string, 0, len(pruned))
	for _, t := range pruned {
		prunedRefs = append(prunedRefs, t.String())
	}

	data := map[string]any{
		"applied": applied,
		"pruned":  prunedRefs,
	}

	// Plugin-expansion bookkeeping — surfaced for the CLI to log
	// `installed plugin X v0.2.0 from <repo>` and `expanded
	// postgres/data/main → statefulset/data/main` so operators
	// see exactly what the macro layer did. Empty arrays are
	// omitted at JSON encoding time (omitempty on the slice
	// types, falsy on nil).
	if len(installs) > 0 {
		data["plugin_installs"] = installs
	}

	if len(expansions) > 0 {
		data["plugin_expansions"] = expansions
	}

	// `current` is only meaningful for dry-run — it's the "before"
	// side of the diff. Regular apply callers don't need it and the
	// CLI's apply-output doesn't render it, so skip the payload bulk.
	if dryRun {
		data["current"] = current
		data["dry_run"] = true
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   data,
	})
}

// checkIngressHostCollisions rejects any apply that would leave two
// ingresses sharing the same Host. Caddy treats host as the
// automation-policy key, so duplicates produce "cannot apply more than
// one automation policy to host" at /load time. Enforcing uniqueness
// here trades a fuzzy reconcile error for a crisp validation error.
//
// The check covers both intra-request duplicates (two ingresses in the
// same body) and cross-request ones (the body's hosts against what's
// already in etcd, minus any ingress being re-applied under the same
// (scope, name) — that's an update, not a collision).
func (a *API) checkIngressHostCollisions(ctx context.Context, manifests []*Manifest) error {
	incoming := map[string]string{}

	for _, m := range manifests {
		if m.Kind != KindIngress {
			continue
		}

		host, err := ingressHost(m)
		if err != nil {
			return err
		}

		if host == "" {
			continue
		}

		ref := fmt.Sprintf("%s/%s", m.Scope, m.Name)

		if prev, dup := incoming[host]; dup {
			return fmt.Errorf("ingress host %q claimed by both %s and %s in this apply", host, prev, ref)
		}

		incoming[host] = ref
	}

	existing, err := a.Store.List(ctx, KindIngress)
	if err != nil {
		return err
	}

	replacing := map[string]struct{}{}

	for _, m := range manifests {
		if m.Kind == KindIngress {
			replacing[m.Scope+"/"+m.Name] = struct{}{}
		}
	}

	for _, e := range existing {
		if _, updating := replacing[e.Scope+"/"+e.Name]; updating {
			continue
		}

		host, err := ingressHost(e)
		if err != nil {
			return err
		}

		if host == "" {
			continue
		}

		if claimant, dup := incoming[host]; dup {
			return fmt.Errorf(
				"ingress host %q already owned by %s/%s — delete it or rename %s",
				host, e.Scope, e.Name, claimant,
			)
		}
	}

	return nil
}

// ingressHost pulls the `host` field out of the ingress spec JSON. Kept
// local to the validator so the API layer doesn't depend on the typed
// manifest package just for a single field read.
func ingressHost(m *Manifest) (string, error) {
	if len(m.Spec) == 0 {
		return "", nil
	}

	var spec struct {
		Host string `json:"host"`
	}

	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return "", fmt.Errorf("ingress/%s: decode spec: %w", m.Name, err)
	}

	return spec.Host, nil
}

// pruneTarget names one resource that would be (or has been) pruned.
// Kept as a struct so diff can surface structured info; the string form
// `kind/scope/name` matches what the store keys on disk.
type pruneTarget struct {
	Kind  Kind
	Scope string
	Name  string
}

func (p pruneTarget) String() string {
	return fmt.Sprintf("%s/%s/%s", p.Kind, p.Scope, p.Name)
}

// computePruneTargets is the pure half of prune: it figures out what
// *would* be deleted without touching the store. Splitting this from
// applyPrune lets `/apply?dry_run=true` answer "what would change" for
// `voodu diff`, while the real apply path runs both halves.
//
// The per-(scope, kind) granularity is deliberate: an apply of
// `deployments.hcl` won't touch ingresses in the same scope, so callers
// can decompose by kind without losing the pair they didn't include.
func (a *API) computePruneTargets(ctx context.Context, manifests []*Manifest) ([]pruneTarget, error) {
	keep := map[string]map[string]struct{}{}

	for _, m := range manifests {
		if !IsScoped(m.Kind) {
			continue
		}

		bucket := string(m.Kind) + "/" + m.Scope

		if _, ok := keep[bucket]; !ok {
			keep[bucket] = map[string]struct{}{}
		}

		keep[bucket][m.Name] = struct{}{}
	}

	var targets []pruneTarget

	for bucket, names := range keep {
		i := strings.Index(bucket, "/")
		kind := Kind(bucket[:i])
		scope := bucket[i+1:]

		existing, err := a.Store.ListByScope(ctx, kind, scope)
		if err != nil {
			return nil, err
		}

		for _, e := range existing {
			if _, kept := names[e.Name]; kept {
				continue
			}

			targets = append(targets, pruneTarget{Kind: e.Kind, Scope: e.Scope, Name: e.Name})
		}
	}

	return targets, nil
}

// applyPrune is the side-effect half: given a plan from
// computePruneTargets, delete each target from the store.
func (a *API) applyPrune(ctx context.Context, targets []pruneTarget) error {
	for _, t := range targets {
		if _, err := a.Store.Delete(ctx, t.Kind, t.Scope, t.Name); err != nil {
			return err
		}
	}

	return nil
}

func (a *API) applyGet(w http.ResponseWriter, r *http.Request) {
	kindStr := r.URL.Query().Get("kind")

	if kindStr == "" {
		list, err := a.Store.ListAll(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}

		writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: list})
		return
	}

	kind, err := ParseKind(kindStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	list, err := a.Store.List(r.Context(), kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: list})
}

func (a *API) applyDelete(w http.ResponseWriter, r *http.Request) {
	kindStr := r.URL.Query().Get("kind")
	name := r.URL.Query().Get("name")
	scope := r.URL.Query().Get("scope")

	if kindStr == "" || name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("kind and name are required"))
		return
	}

	kind, err := ParseKind(kindStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	if IsScoped(kind) && scope == "" {
		// For scoped kinds the CLI must provide scope. Scan and return a
		// clear error if there's a single match, or ask for disambiguation.
		resolved, err := resolveScope(r.Context(), a.Store, kind, name)
		if err != nil {
			if errors.Is(err, errScopeNotFound) {
				writeErr(w, http.StatusNotFound, err)
				return
			}

			writeErr(w, http.StatusBadRequest, err)
			return
		}

		scope = resolved
	}

	deleted, err := a.Store.Delete(r.Context(), kind, scope, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if !deleted {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%s/%s not found", kind, name))
		return
	}

	prune := r.URL.Query().Get("prune") == "true"

	pruned := pruneSummary{}

	if prune {
		// Prune happens AFTER the manifest delete fires its watch
		// event so the reconciler's handler.remove() has already
		// torn down containers + cleared status. Then we go after
		// the long-lived state the soft-delete deliberately keeps:
		// app-level config in etcd, on-disk env file / releases /
		// volumes. Best-effort — failures are logged into the
		// response so the operator can finish manually but don't
		// fail the whole DELETE.
		pruned = pruneResource(r.Context(), a.Store, kind, scope, name)

		// Statefulset-only: remove the per-pod docker volumes the
		// handler created. Soft-delete leaves them so an operator
		// can re-apply and recover the data; --prune is the
		// explicit opt-in to wipe. Best-effort: errors append to
		// the summary but don't fail the delete.
		if kind == KindStatefulset && a.Statefulsets != nil {
			vols, err := a.Statefulsets.PruneVolumes(scope, name)
			if err != nil {
				pruned.Errors = append(pruned.Errors, fmt.Sprintf("prune volumes: %v", err))
			} else {
				pruned.VolumesRemoved = vols
			}
		}
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data: map[string]any{
			"pruned": pruned,
		},
	})
}

// pruneSummary captures the destructive cleanup outcome so the
// CLI can surface "config wiped, /opt/voodu/apps/X removed, 1
// volume left intact" to the operator. Fields stay populated even
// on partial failure so the human knows which steps need a manual
// follow-up.
type pruneSummary struct {
	ConfigWiped   bool   `json:"config_wiped,omitempty"`
	AppDirRemoved string `json:"app_dir_removed,omitempty"`
	VolumeRemoved string `json:"volume_removed,omitempty"`

	// VolumesRemoved is the list of docker named volumes wiped
	// by `vd delete statefulset/... --prune`. Populated only for
	// the statefulset kind — other kinds don't own per-pod
	// docker volumes. Empty slice is normal (no claims declared).
	VolumesRemoved []string `json:"volumes_removed,omitempty"`

	Errors []string `json:"errors,omitempty"`
}

// handleScopeWipe is the nuclear option: wipe every manifest in
// `scope` across every scoped kind, plus the scope-level config
// bucket, plus per-app filesystem state for each app inside.
//
//	DELETE /scope?scope=clowk-lp&prune=true
//
// `prune=true` is required — there's no useful "soft scope wipe"
// (you'd just be removing the scope label which doesn't exist as
// its own resource). The query gate enforces "operator typed
// --prune explicitly", same belt the CLI applies.
//
// Per-resource teardown reuses pruneResource so the per-app
// behaviour matches a `vd delete -f one.hcl --prune` of every
// resource in the file. Scope-level config is wiped after the
// per-resource passes so a half-failed wipe leaves consistent
// state (apps gone, scope-level config still there) instead of
// orphan apps with no scope-level config.
func (a *API) handleScopeWipe(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("scope is required"))
		return
	}

	if r.URL.Query().Get("prune") != "true" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("scope wipe requires ?prune=true (this destroys every manifest, config and on-disk state in the scope)"))
		return
	}

	ctx := r.Context()

	type wipedResource struct {
		Kind   Kind         `json:"kind"`
		Name   string       `json:"name"`
		Pruned pruneSummary `json:"pruned"`
	}

	var wiped []wipedResource

	scopedErrors := []string{}

	for kind := range ScopedKinds {
		mans, err := a.Store.ListByScope(ctx, kind, scope)
		if err != nil {
			scopedErrors = append(scopedErrors, fmt.Sprintf("list %s: %v", kind, err))
			continue
		}

		for _, m := range mans {
			if _, err := a.Store.Delete(ctx, kind, scope, m.Name); err != nil {
				scopedErrors = append(scopedErrors, fmt.Sprintf("delete %s/%s/%s: %v", kind, scope, m.Name, err))
				continue
			}

			sum := pruneResource(ctx, a.Store, kind, scope, m.Name)
			wiped = append(wiped, wipedResource{Kind: kind, Name: m.Name, Pruned: sum})
		}
	}

	// Scope-level config bucket (the shared `vd config <scope> set`
	// values). Wiped last so per-app removals are done first; an
	// early failure here would orphan apps without scope env.
	scopeConfigErr := a.Store.DeleteConfig(ctx, scope, "")

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data: map[string]any{
			"scope":              scope,
			"resources_wiped":    wiped,
			"scope_config_wiped": scopeConfigErr == nil,
			"errors":             scopedErrors,
		},
	})
}

// pruneResource is the per-resource destructive cleanup. Wipes the
// app-level config bucket and (for kinds that own filesystem state)
// removes the app dir + volume dir from /opt/voodu. Scope-level
// config is NOT touched here — that's the scope-wipe path's job.
//
// All operations are best-effort: we want a partial wipe to still
// progress as far as possible (filesystem remove can fail mid-way,
// config delete can fail on etcd hiccup, etc.) and report what
// landed in the summary.
func pruneResource(ctx context.Context, store Store, kind Kind, scope, name string) pruneSummary {
	var sum pruneSummary

	if err := store.DeleteConfig(ctx, scope, name); err != nil {
		sum.Errors = append(sum.Errors, fmt.Sprintf("delete config: %v", err))
	} else {
		sum.ConfigWiped = true
	}

	// Filesystem cleanup only makes sense for kinds that have a
	// per-app on-disk footprint. Today: deployments (env file,
	// release dirs, volumes); jobs/cronjobs share the same layout
	// because they shell out through the same paths helpers;
	// statefulset has the env file too (write_env writes to
	// AppEnvFile under AppDir) so it joins the family. Other
	// kinds (ingress) don't own any filesystem of their own —
	// their state lives in plugins / etcd.
	if kind == KindDeployment || kind == KindStatefulset || kind == KindJob || kind == KindCronJob {
		app := AppID(scope, name)

		appDir := paths.AppDir(app)
		if err := os.RemoveAll(appDir); err != nil {
			sum.Errors = append(sum.Errors, fmt.Sprintf("remove %s: %v", appDir, err))
		} else {
			sum.AppDirRemoved = appDir
		}

		volDir := paths.AppVolumeDir(app)
		if err := os.RemoveAll(volDir); err != nil {
			sum.Errors = append(sum.Errors, fmt.Sprintf("remove %s: %v", volDir, err))
		} else {
			sum.VolumeRemoved = volDir
		}
	}

	return sum
}

// resolveScope finds the single scope that owns (kind, name) when the
// caller didn't provide one. Used by DELETE/GET convenience paths so
// `voodu scale -a web` keeps working without forcing the operator to
// remember which scope `web` lives in. Ambiguous matches return an
// error listing the candidates — there's no safe default to pick.
func resolveScope(ctx context.Context, store Store, kind Kind, name string) (string, error) {
	all, err := store.List(ctx, kind)
	if err != nil {
		return "", err
	}

	var matches []string

	for _, m := range all {
		if m.Name == name {
			matches = append(matches, m.Scope)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%s/%s: %w", kind, name, errScopeNotFound)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%s/%s is ambiguous across scopes %v — pass ?scope=...", kind, name, matches)
	}
}


// handleDescribe returns the full picture for a single declared
// resource: the source manifest, its persisted status blob, and any
// voodu-managed containers matching the (kind, scope, name) identity.
//
// GET /describe?kind=<k>&name=<n>[&scope=<s>]
//
// Scoped kinds without an explicit scope are auto-resolved when
// unambiguous (single match across scopes); ambiguous matches return
// 400 with the candidate scopes named so the operator can re-issue
// with ?scope=. Missing manifest → 404.
//
// Pod listing failures are tolerated: docker-side issues shouldn't
// black-hole the whole describe — the manifest+status part is still
// useful. The CLI renders an empty pods section in that case.
func (a *API) handleDescribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	kindStr := strings.TrimSpace(r.URL.Query().Get("kind"))
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))

	if kindStr == "" || name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("kind and name are required"))
		return
	}

	kind, err := ParseKind(kindStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	if IsScoped(kind) && scope == "" {
		resolved, err := resolveScope(r.Context(), a.Store, kind, name)
		if err != nil {
			if errors.Is(err, errScopeNotFound) {
				writeErr(w, http.StatusNotFound, err)
				return
			}

			writeErr(w, http.StatusBadRequest, err)

			return
		}

		scope = resolved
	}

	manifest, err := a.Store.Get(r.Context(), kind, scope, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if manifest == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%s/%s/%s not found", kind, scope, name))
		return
	}

	// Status blob is keyed by AppID(scope,name) for scoped kinds and by
	// the bare name for unscoped ones — AppID returns the right shape
	// either way (empty scope → bare name).
	appID := AppID(scope, name)

	statusBlob, err := a.Store.GetStatus(r.Context(), kind, appID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	var status json.RawMessage
	if len(statusBlob) > 0 {
		status = statusBlob
	}

	// Pod listing: filter the host's voodu-labeled containers down to
	// those matching this resource. Pod failures are intentionally
	// non-fatal here — the manifest + status carry most of the value
	// of describe, and an operator with a broken docker daemon already
	// knows to look at `voodu get pods` for runtime issues.
	pods := a.matchingPods(kind, scope, name)

	data := map[string]any{
		"manifest": manifest,
		"status":   status,
		"pods":     pods,
	}

	// Statefulset-only: surface per-pod volume names so the operator
	// can see which docker volumes carry the persistent data. Failure
	// listing volumes is best-effort (same posture as pod listing) —
	// the manifest+pods rendering still works.
	if kind == KindStatefulset {
		data["volumes"] = a.matchingStatefulsetVolumes(scope, name)
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   data,
	})
}

// matchingStatefulsetVolumes enumerates the docker named volumes a
// statefulset owns — one per (claim, ordinal). Delegates to the
// Statefulsets seam (which reaches the labelled-volume listing
// inside its ContainerManager). Returns nil on missing seam or
// lister failure so the describe payload stays well-formed.
func (a *API) matchingStatefulsetVolumes(scope, name string) []string {
	if a.Statefulsets == nil {
		return nil
	}

	vols, err := a.Statefulsets.Volumes(scope, name)
	if err != nil {
		return nil
	}

	return vols
}

// matchingPods filters the host's voodu-labeled containers to the
// (kind, scope, name) identity. Returns an empty slice on lister
// failure so handleDescribe stays useful when docker is degraded.
func (a *API) matchingPods(kind Kind, scope, name string) []Pod {
	lister := a.Pods
	if lister == nil {
		lister = DockerPodsLister{}
	}

	all, err := lister.ListPods()
	if err != nil {
		return nil
	}

	out := make([]Pod, 0)

	for _, p := range all {
		if string(kind) != p.Kind {
			continue
		}

		if p.ResourceName != name {
			continue
		}

		if IsScoped(kind) && scope != p.Scope {
			continue
		}

		out = append(out, p)
	}

	return out
}

// handlePods returns every voodu-managed container on the host with
// its structured identity (kind, scope, name, replica id) plus the
// runtime status fields the CLI needs to render `voodu get pods`.
//
// Optional ?kind=<deployment|job|cronjob> filters the result on the
// label vocabulary. ?scope=<scope> narrows further. Both filters apply
// post-listing so the docker call stays a single shot — pod counts on
// a single host are tiny by k8s standards (tens, not thousands).
func (a *API) handlePods(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	lister := a.Pods
	if lister == nil {
		lister = DockerPodsLister{}
	}

	pods, err := lister.ListPods()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	wantKind := strings.TrimSpace(r.URL.Query().Get("kind"))
	wantScope := strings.TrimSpace(r.URL.Query().Get("scope"))
	wantName := strings.TrimSpace(r.URL.Query().Get("name"))

	if wantKind != "" || wantScope != "" || wantName != "" {
		filtered := pods[:0]

		for _, p := range pods {
			if wantKind != "" && p.Kind != wantKind {
				continue
			}

			if wantScope != "" && p.Scope != wantScope {
				continue
			}

			if wantName != "" && p.ResourceName != wantName {
				continue
			}

			filtered = append(filtered, p)
		}

		pods = filtered
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   map[string]any{"pods": pods},
	})
}

// handlePodDescribe returns the rich `docker inspect` view of one
// container, scoped to the per-pod detail the CLI's `voodu describe
// pod <name>` renders.
//
// GET /pods/{name}
//
// {name} is the docker container name as it appears in `voodu get
// pods` (e.g. "test-web.a3f9"). Pods don't share the kind/scope/name
// shape because more than one replica can match the same identity —
// the operator points at a specific container.
//
// Returns 404 when the container doesn't exist on this host, 503 when
// the API was wired without a PodDescriber (test setups), 500 on any
// other inspect failure.
func (a *API) handlePodDescribe(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("pod name is required"))
		return
	}

	// Hostile names — slashes, leading dots — would either confuse
	// docker or escape into adjacent paths. Guard at the surface so a
	// typo doesn't turn into a strange daemon error.
	if strings.ContainsAny(name, "/ \t\n") || strings.HasPrefix(name, ".") {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid pod name: %q", name))
		return
	}

	// PodDescriber is a separate interface from PodsLister so tests
	// that only wire one or the other don't have to satisfy both.
	// Production's DockerPodsLister satisfies both — fall back to it
	// when neither field is set.
	describer, ok := a.Pods.(PodDescriber)
	if !ok {
		if a.Pods == nil {
			describer = DockerPodsLister{}
		} else {
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("pod describer not configured"))
			return
		}
	}

	detail, err := describer.GetPod(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if detail == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("pod %q not found", name))
		return
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   map[string]any{"pod": detail},
	})
}

// handleExec dispatches unknown CLI commands to a plugin. Body is
// {"args": ["<plugin>", "<cmd>", ...]} plus optional env. The CLI's
// colon rewriter already split "postgres:create" into two args.
//
// Response is the plugin JSON envelope when the plugin emitted one,
// or a synthetic envelope wrapping plain-text stdout so the CLI can
// render with -o text|json|yaml uniformly.
func (a *API) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	var req struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"env,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}

	if len(req.Args) < 2 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("need at least <plugin> <command>"))
		return
	}

	if a.PluginsRoot == "" {
		writeErr(w, http.StatusNotFound, fmt.Errorf(
			"no builtin and no plugin registered for %q",
			strings.Join(req.Args, " "),
		))

		return
	}

	pluginName, cmd, cmdArgs := req.Args[0], req.Args[1], req.Args[2:]

	res, err := a.invoker().Invoke(r.Context(), pluginName, cmd, cmdArgs, req.Env)
	if err != nil {
		// LoadFromDir failure maps to 404 — everything else is 500.
		// A dedicated error type would be cleaner, but string matching
		// on the DirInvoker's wrapped error is good enough for one site.
		if strings.Contains(err.Error(), "plugin ") && strings.Contains(err.Error(), pluginName) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("plugin %q not installed", pluginName))
			return
		}

		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	if res.Envelope != nil {
		writeJSON(w, pluginExitToHTTP(res.ExitCode), res.Envelope)
		return
	}

	// Plain-text plugins: wrap stdout in an envelope so downstream
	// formatters have a consistent shape. Exit != 0 becomes error.
	if res.ExitCode != 0 {
		writeErr(w, pluginExitToHTTP(res.ExitCode), fmt.Errorf("%s", strings.TrimSpace(res.CombinedOutput())))
		return
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   map[string]string{"stdout": string(res.Raw)},
	})
}

// invoker returns the configured PluginInvoker, falling back to a
// DirInvoker built from the API's plugins root. Lazy so callers that
// explicitly wire Invoker don't pay for the DirInvoker allocation.
func (a *API) invoker() PluginInvoker {
	if a.Invoker != nil {
		return a.Invoker
	}

	return &DirInvoker{
		PluginsRoot: a.PluginsRoot,
		NodeName:    a.NodeName,
		EtcdClient:  a.EtcdClient,
	}
}

// pluginExitToHTTP maps a plugin exit code to an HTTP status. Zero is
// 200, anything else is 500 — plugins should use the envelope's Error
// field for structured failure, not HTTP semantics.
func pluginExitToHTTP(code int) int {
	if code == 0 {
		return http.StatusOK
	}

	return http.StatusInternalServerError
}

// handlePodLogs streams stdout+stderr of one voodu-managed container
// back to the caller, addressed by docker container name.
//
// GET /pods/{name}/logs?follow=&tail=
//
// {name} is the docker container name as it appears in `voodu get pods`
// (e.g. "clowk-lp-web.a3f9"). The kind-aware /logs endpoint that used
// to do scope/name/run resolution server-side is gone — the CLI now
// resolves to a list of container names client-side via /pods?...,
// then opens one stream per match. Centralising fan-out on the client
// keeps the server's job stupid (one container, one stream) and makes
// multi-replica `vd logs <scope>/<name>` natural.
//
// `follow=true` keeps the stream open until the container exits or
// the caller disconnects (chunked transfer with periodic Flush so the
// CLI sees lines as they arrive).
//
// Response body is the raw log stream (text/plain). Errors from
// lookup / open land on a JSON envelope BEFORE any byte of the stream
// is written so the CLI's JSON path stays usable.
func (a *API) handlePodLogs(w http.ResponseWriter, r *http.Request) {
	if a.Logs == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("log streaming not configured"))
		return
	}

	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("pod name is required"))
		return
	}

	if strings.ContainsAny(name, "/ \t\n") || strings.HasPrefix(name, ".") {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid pod name: %q", name))
		return
	}

	q := r.URL.Query()
	follow := q.Get("follow") == "true"

	tail := 0
	if t := strings.TrimSpace(q.Get("tail")); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("tail must be a non-negative integer"))
			return
		}

		tail = n
	}

	stream, err := a.Logs.Logs(name, LogsOptions{Follow: follow, Tail: tail})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("open log stream: %w", err))
		return
	}

	defer stream.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Voodu-Container", name)

	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	// Small buffer + Flush after every read keeps `voodu logs -f` lines
	// from sitting in the chunked-transfer buffer until the next page
	// boundary. Non-follow reads still benefit (faster TTFB) without
	// extra cost.
	buf := make([]byte, 4096)

	for {
		n, readErr := stream.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}

			if flusher != nil {
				flusher.Flush()
			}
		}

		if readErr != nil {
			return
		}
	}
}

// handlePodStop stops one container by docker name. When the
// `?freeze=true` query param is set (default in the CLI), the
// pod's ordinal is also added to the persistent frozen-ordinals
// list so the reconciler skips it on every subsequent
// ensureOrdinalsUp / rolling-restart pass — extended downtime
// without docker-shell access. Without freeze, the stop is
// transient: any next env-change or spec-drift event will
// recreate the container.
//
//	POST /pods/{name}/stop?freeze=true|false
//
// Today supported only for statefulset pods (deployment replica
// IDs are non-stable across spawns; freeze can't survive scale
// events). Containers without a recognised voodu identity
// (legacy / hand-spawned) error with 400 — the operator can
// still `docker stop` them directly.
func (a *API) handlePodStop(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("pod name is required"))
		return
	}

	if strings.ContainsAny(name, "/ \t\n") || strings.HasPrefix(name, ".") {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid pod name: %q", name))
		return
	}

	if a.PodLifecycle == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("pod lifecycle manager not configured"))
		return
	}

	freeze := r.URL.Query().Get("freeze") != "false"

	// Recover identity from the container's labels so we know
	// what (kind, scope, name, replica_id) to add to the frozen
	// list. Skipped when freeze=false — that path doesn't touch
	// the store, so the identity lookup is wasted work.
	var (
		ident containers.Identity
		hasID bool
	)

	if freeze {
		labels, err := a.PodLifecycle.InspectLabels(name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("inspect %s: %w", name, err))
			return
		}

		ident, hasID = containers.ParseLabels(labels)

		if !hasID {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("pod %q has no voodu identity (was it created by `vd apply`?); cannot persist freeze", name))
			return
		}

		// Both statefulset (ordinal-as-string) and deployment
		// (hex) replica IDs flow through the same annotation
		// list — buildFrozenSet's lookup is map[string]bool, so
		// "0" and "a3f9" coexist without type juggling.
		if ident.ReplicaID == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("pod %q has no replica_id label", name))
			return
		}
	}

	if err := a.PodLifecycle.Stop(name); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("stop %s: %w", name, err))
		return
	}

	if freeze {
		current, _ := a.Store.GetFrozenReplicaIDs(r.Context(), Kind(ident.Kind), ident.Scope, ident.Name)

		updated := addReplicaID(current, ident.ReplicaID)

		if err := a.Store.SetFrozenReplicaIDs(r.Context(), Kind(ident.Kind), ident.Scope, ident.Name, updated); err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("persist frozen replicas: %w", err))
			return
		}
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data: map[string]any{
			"name":   name,
			"freeze": freeze,
		},
	})
}

// handlePodStart starts a stopped container by docker name and
// removes its ordinal from the frozen list (if present), so
// subsequent reconciles include it in the rolling restart paths
// again.
//
//	POST /pods/{name}/start
//
// Idempotent — already-running containers and never-frozen pods
// both succeed. Errors when the container doesn't exist on the
// host (the `Ensure`-shaped "create if missing" semantic doesn't
// belong here; that's `vd apply`'s job).
func (a *API) handlePodStart(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("pod name is required"))
		return
	}

	if strings.ContainsAny(name, "/ \t\n") || strings.HasPrefix(name, ".") {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid pod name: %q", name))
		return
	}

	if a.PodLifecycle == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("pod lifecycle manager not configured"))
		return
	}

	// Recover identity to clear the freeze (best-effort — labels
	// may be missing on legacy containers, in which case there's
	// nothing to unfreeze and we just start the container).
	labels, _ := a.PodLifecycle.InspectLabels(name)

	ident, hasID := containers.ParseLabels(labels)

	if err := a.PodLifecycle.Start(name); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("start %s: %w", name, err))
		return
	}

	cleared := false

	if hasID && ident.ReplicaID != "" {
		current, _ := a.Store.GetFrozenReplicaIDs(r.Context(), Kind(ident.Kind), ident.Scope, ident.Name)

		updated := removeReplicaID(current, ident.ReplicaID)

		if len(updated) != len(current) {
			if err := a.Store.SetFrozenReplicaIDs(r.Context(), Kind(ident.Kind), ident.Scope, ident.Name, updated); err != nil {
				writeErr(w, http.StatusInternalServerError, fmt.Errorf("clear frozen replica: %w", err))
				return
			}

			cleared = true
		}
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data: map[string]any{
			"name":          name,
			"unfroze":       cleared,
			"started_clean": true,
		},
	})
}

// addReplicaID returns the frozen list with `id` added, deduped
// and sorted. Sort is for deterministic etcd writes — same set
// of replicas always serialises identically.
func addReplicaID(existing []string, id string) []string {
	for _, e := range existing {
		if e == id {
			// Already in list — return a deterministic copy
			// instead of mutating the caller's slice.
			out := append([]string(nil), existing...)
			sort.Strings(out)

			return out
		}
	}

	out := append([]string(nil), existing...)
	out = append(out, id)
	sort.Strings(out)

	return out
}

// removeReplicaID returns the frozen list with every occurrence
// of `id` removed. Length comparison vs the input lets callers
// detect the no-op cheaply.
func removeReplicaID(existing []string, id string) []string {
	out := make([]string, 0, len(existing))
	for _, e := range existing {
		if e == id {
			continue
		}

		out = append(out, e)
	}

	return out
}

// handlePlugins lists plugins currently installed under PluginsRoot.
// Plugins that fail to load are reported as errors alongside the list —
// the controller does not hide a broken plugin from the operator.
func (a *API) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))

		return
	}

	if a.PluginsRoot == "" {
		writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: []any{}})
		return
	}

	loaded, loadErrs := plugins.LoadAll(a.PluginsRoot)

	manifests := make([]plugin.Manifest, 0, len(loaded))
	for _, p := range loaded {
		manifests = append(manifests, p.Manifest)
	}

	errs := make([]string, 0, len(loadErrs))
	for _, e := range loadErrs {
		errs = append(errs, e.Error())
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data: map[string]any{
			"plugins": manifests,
			"errors":  errs,
		},
	})
}

// handlePluginInstall materialises a plugin from the given source (local
// path or git URL). Body: {"source": "owner/repo"} or {"source": "/path"}.
func (a *API) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	if a.PluginsRoot == "" {
		writeErr(w, http.StatusNotFound, fmt.Errorf("plugin system not configured"))
		return
	}

	var req struct {
		Source  string `json:"source"`
		Version string `json:"version,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}

	if strings.TrimSpace(req.Source) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("source is required"))
		return
	}

	inst := &plugins.Installer{Root: a.PluginsRoot}

	loaded, err := inst.Install(r.Context(), req.Source, req.Version)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: loaded.Manifest})
}

// handlePluginRemove deletes the plugin directory. 404 if the plugin
// isn't installed; the DELETE is otherwise idempotent.
func (a *API) handlePluginRemove(w http.ResponseWriter, r *http.Request) {
	if a.PluginsRoot == "" {
		writeErr(w, http.StatusNotFound, fmt.Errorf("plugin system not configured"))
		return
	}

	name := r.PathValue("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("plugin name is required"))
		return
	}

	inst := &plugins.Installer{Root: a.PluginsRoot}

	ok, err := inst.Remove(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("plugin %q not installed", name))
		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok"})
}

// handleJobRun executes a previously-applied job synchronously and
// returns the run record (run id, exit code, duration). Two query
// parameters drive it:
//
//	?scope=<scope>&name=<name>
//
// `scope` may be omitted when only one job in the store carries the
// requested name; we resolve it via resolveScope, the same disambiguator
// the delete path uses. Body is unused — the spec to run comes from
// the persisted manifest, not the request — so the call is a plain
// POST with no payload.
//
// Synchronous on purpose (M3): the connection stays open until the job
// completes. Long-running jobs benefit from a kick-off + poll shape
// (M5+) but the migration / one-shot script use cases this kind
// targets fit in a single round-trip.
func (a *API) handleJobRun(w http.ResponseWriter, r *http.Request) {
	if a.Jobs == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("job runner not configured"))
		return
	}

	name := strings.TrimSpace(r.URL.Query().Get("name"))
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))

	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("name is required"))
		return
	}

	if scope == "" {
		resolved, err := resolveScope(r.Context(), a.Store, KindJob, name)
		if err != nil {
			if errors.Is(err, errScopeNotFound) {
				writeErr(w, http.StatusNotFound, err)
				return
			}

			writeErr(w, http.StatusBadRequest, err)
			return
		}

		scope = resolved
	}

	run, err := a.Jobs.RunOnce(r.Context(), scope, name)
	if err != nil {
		// The runner already wrote the failure into status. Return the
		// run record alongside the error so the CLI can render exit
		// code + duration even when the response is a 500.
		writeJSON(w, http.StatusInternalServerError, envelope{
			Status: "error",
			Error:  err.Error(),
			Data:   run,
		})

		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: run})
}

// handlePodExec runs a command inside a running container —
// kubectl-exec-style streaming. Uses HTTP hijack to take over the
// raw connection because the chunked-transfer model behind a normal
// http.ResponseWriter doesn't carry stdin (data only flows server →
// client). Hijack gives us the underlying TCP/TLS conn, and we can
// run a bidirectional stream over it.
//
//	POST /pods/{name}/exec?tty=&interactive=&user=&workdir=
//
// Body: JSON `{"command": ["/bin/sh", "-c", "ls"]}` — the command
// to exec. Empty/missing command → 400.
//
// Wire protocol: after the handshake (HTTP 101 Switching Protocols),
// the connection becomes a raw bidi stream. Client → server payload
// is the child's stdin; server → client is multiplexed
// stdout+stderr (TTY mode merges them, non-TTY keeps stdout only —
// stderr is dropped to keep the wire simple, since 99% of exec
// targets are interactive shells where the distinction doesn't
// matter).
//
// Exit code is communicated via the connection close: clean close
// = 0, otherwise the child's exit code is logged server-side and
// the close happens after stdout drains. The CLI can probe by
// observing the close, or rely on the underlying shell exit.
func (a *API) handlePodExec(w http.ResponseWriter, r *http.Request) {
	if a.Execer == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("exec not configured"))
		return
	}

	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("pod name is required"))
		return
	}

	if strings.ContainsAny(name, "/ \t\n") || strings.HasPrefix(name, ".") {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid pod name: %q", name))
		return
	}

	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	var req struct {
		Command []string `json:"command"`
		Env     []string `json:"env,omitempty"`
	}

	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}

	if len(req.Command) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("command is required"))
		return
	}

	q := r.URL.Query()
	tty := q.Get("tty") == "true"
	interactive := q.Get("interactive") == "true"
	workdir := strings.TrimSpace(q.Get("workdir"))
	user := strings.TrimSpace(q.Get("user"))

	// Window size for PTY allocation. Best-effort: malformed values
	// just default to zero, in which case the kernel picks the pty's
	// default (24x80). Cols/Rows are uint16 in the kernel; clamp on
	// parse to avoid surprising overflows.
	cols := parseUint16Query(q.Get("cols"))
	rows := parseUint16Query(q.Get("rows"))

	// Hijack the connection so we can stream stdin/stdout bidirectionally.
	// Without hijack the response writer can only push bytes one-way; the
	// client's stdin would be stuck in the request body which Go's HTTP
	// stack closes once the handler returns.
	//
	// Hijack BEFORE writing headers — that's the canonical pattern. Go's
	// net/http buffers the response writer in a way that interleaves
	// awkwardly with raw conn writes; hijacking first gives us full
	// ownership and we craft the response status line manually.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("hijack not supported"))
		return
	}

	conn, brw, err := hijacker.Hijack()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("hijack: %w", err))
		return
	}

	defer conn.Close()

	// Manually write the HTTP/1.1 response head. Connection: close so
	// the client knows the body is close-delimited; X-Voodu-Container
	// echoes back the resolved target for the operator to confirm.
	statusLine := "HTTP/1.1 200 OK\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"X-Voodu-Container: " + name + "\r\n" +
		"Connection: close\r\n" +
		"\r\n"

	if _, err := brw.Writer.WriteString(statusLine); err != nil {
		return
	}

	if err := brw.Writer.Flush(); err != nil {
		return
	}

	opts := ExecOptions{
		TTY:         tty,
		Interactive: interactive,
		WorkingDir:  workdir,
		User:        user,
		Env:         req.Env,
		Cols:        cols,
		Rows:        rows,
		Stdin:       conn,
		Stdout:      conn,
		Stderr:      conn,
	}

	// Block until docker exec exits; the connection close signals
	// completion to the client. Exit code is logged but not propagated
	// over the wire today — the CLI relies on the spawned shell's own
	// exit semantics. A future protocol version could prefix a status
	// frame, but for `vd exec -- bash` the current shape is enough.
	_, _ = a.Execer.Exec(name, req.Command, opts)
}

// handleConfig is the multi-method dispatcher for /config — the
// CRUD surface for env vars stored in etcd. Replaces the old
// filesystem-based secrets.* path so an operator can `vd config
// set FOO=bar` from their dev Mac without SSHing into the server.
//
// GET    /config?scope=&name=               list keys (merged scope+app)
// GET    /config?scope=&name=&key=KEY        single value
// POST   /config?scope=&name=                patch keys (body: {KEY: VAL, ...})
// DELETE /config?scope=&name=&key=KEY        unset one key
//
// Scope is required for all operations. name is optional — when
// omitted, ops target the scope-level bucket (shared env across
// resources in scope). On GET/list with name set, the response is
// the MERGED config (scope-level + app-level overrides), since
// that's what the operator usually wants to inspect ("what env
// will my container see?"). Set ?merge=false to get only the
// app-specific bucket.
//
// Auto-restart: a successful POST or DELETE re-emits Apply events
// for every resource the change affects, which the reconciler
// picks up and restarts (Recreate). Operators can opt out with
// ?restart=false on the request.
func (a *API) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.configGet(w, r)
	case http.MethodPost:
		a.configPost(w, r)
	case http.MethodDelete:
		a.configDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))
	}
}

func (a *API) configGet(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	key := strings.TrimSpace(r.URL.Query().Get("key"))

	if scope == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("scope is required"))
		return
	}

	merge := r.URL.Query().Get("merge") != "false"

	var (
		vars map[string]string
		err  error
	)

	if name != "" && merge {
		vars, err = a.Store.ResolveConfig(r.Context(), scope, name)
	} else {
		vars, err = a.Store.GetConfig(r.Context(), scope, name)
	}

	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	if key != "" {
		v, ok := vars[key]
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("key %q not set", key))
			return
		}

		writeJSON(w, http.StatusOK, envelope{
			Status: "ok",
			Data:   map[string]string{key: v},
		})

		return
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   map[string]any{"vars": vars},
	})
}

func (a *API) configPost(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	name := strings.TrimSpace(r.URL.Query().Get("name"))

	if scope == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("scope is required"))
		return
	}

	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	var payload map[string]string
	if err := json.Unmarshal(body, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}

	if len(payload) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("body must be a non-empty {KEY: VALUE} object"))
		return
	}

	if err := a.Store.PatchConfig(r.Context(), scope, name, payload); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	a.maybeRestartAffected(r, scope, name)

	writeJSON(w, http.StatusOK, envelope{Status: "ok"})
}

func (a *API) configDelete(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	key := strings.TrimSpace(r.URL.Query().Get("key"))

	if scope == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("scope is required"))
		return
	}

	if key == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("key is required for DELETE"))
		return
	}

	if _, err := a.Store.DeleteConfigKey(r.Context(), scope, name, key); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	a.maybeRestartAffected(r, scope, name)

	writeJSON(w, http.StatusOK, envelope{Status: "ok"})
}

// maybeRestartAffected re-emits Apply events for every container-
// producing manifest the config change touches. When name is set,
// only that resource. When name is empty (scope-level), every
// deployment / statefulset / job / cronjob in the scope. The
// reconciler's existing change detection handles the "no actual env
// changed → no-op" case so it's safe to over-trigger.
//
// Statefulset is in the list because plugins (voodu-redis at
// minimum) emit `config_set` actions that must propagate into the
// running pods — REDIS_PASSWORD on first apply, REDIS_MASTER_ORDINAL
// on `vd redis:failover`. Leaving statefulset out of this fan-out
// means failover writes the bucket but never restarts the pods,
// stranding the cluster on the old role assignment until the
// operator runs a manual `vd restart`.
//
// The ?restart=false query param turns this off — useful when the
// operator wants to batch multiple set/unset calls and only restart
// once at the end.
func (a *API) maybeRestartAffected(r *http.Request, scope, name string) {
	if r.URL.Query().Get("restart") == "false" {
		return
	}

	if a.Store == nil {
		return
	}

	ctx := r.Context()

	kinds := []Kind{KindDeployment, KindStatefulset, KindJob, KindCronJob}

	for _, kind := range kinds {
		mans, err := a.Store.ListByScope(ctx, kind, scope)
		if err != nil {
			continue
		}

		for _, m := range mans {
			if name != "" && m.Name != name {
				continue
			}

			// Re-Put the manifest unchanged to fire a watch event.
			// The reconciler will see PUT, fetch fresh config, and
			// Recreate when env actually differs.
			_, _ = a.Store.Put(ctx, m)
		}
	}
}

// parseUint16Query is a forgiving uint16 parser for query params:
// blank or malformed values return 0 (which the pty path treats as
// "use the kernel default"), valid integers clamp to uint16's range
// before returning. Wider values (negative, >65535) silently fall
// to zero — callers that care about validation should do their own.
func parseUint16Query(s string) uint16 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 65535 {
		return 0
	}

	return uint16(n)
}

// handleRestart triggers a rolling restart of a deployment's
// replicas without requiring a manifest edit. Common after running
// migrations / config rotation / image rebuild — the spec didn't
// drift, but the operator wants the running processes refreshed.
//
//	POST /restart?kind=deployment&scope=<scope>&name=<name>
//
// Only `kind=deployment` is supported today: jobs and cronjobs are
// transient by design (you re-trigger them via /jobs/run /
// /cronjobs/run), and databases / ingress are plugin-managed in a
// way that doesn't fit the rolling-replace model.
//
// Synchronous: the call blocks until every replica has been
// replaced (slotRolloutPause between, ~2s default). Returns the
// per-replica progress in the response data so a future CLI render
// can show "1/3 replaced, 2/3...". For now the body is just a
// status envelope.
func (a *API) handleRestart(w http.ResponseWriter, r *http.Request) {
	kindStr := strings.TrimSpace(r.URL.Query().Get("kind"))
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	name := strings.TrimSpace(r.URL.Query().Get("name"))

	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("name is required"))
		return
	}

	// Default kind to deployment for ergonomics — `vd restart X`
	// almost always means "restart the deployment". Statefulsets
	// must be addressed explicitly via kind=statefulset.
	if kindStr == "" {
		kindStr = string(KindDeployment)
	}

	kind, err := ParseKind(kindStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	type restartFn func(ctx context.Context, scope, name string) error

	var fn restartFn

	switch kind {
	case KindDeployment:
		if a.Deployments == nil {
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("deployment restart not configured"))
			return
		}

		fn = a.Deployments.Restart

	case KindStatefulset:
		if a.Statefulsets == nil {
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("statefulset restart not configured"))
			return
		}

		fn = a.Statefulsets.Restart

	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("restart not supported for kind %q (deployment, statefulset)", kind))
		return
	}

	if scope == "" {
		resolved, err := resolveScope(r.Context(), a.Store, kind, name)
		if err != nil {
			if errors.Is(err, errScopeNotFound) {
				writeErr(w, http.StatusNotFound, err)
				return
			}

			writeErr(w, http.StatusBadRequest, err)
			return
		}

		scope = resolved
	}

	if err := fn(r.Context(), scope, name); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data: map[string]string{
			"scope": scope,
			"name":  name,
			"kind":  string(kind),
		},
	})
}

// handleReleaseRun manually triggers the release-phase command(s)
// for a deployment. Same flow as the apply-time auto-trigger but
// invoked imperatively so the operator can re-run a fix without
// editing the manifest.
//
//	POST /releases/run?scope=&name=
//
// Response: text/plain streamed in real-time. The release
// container's stdout+stderr flows through the response body as
// the migration runs, with marker lines (-----> Release X: command,
// -----> Release X failed in command, etc.) bracketing each phase.
// CI runners see the migration logs flow live; operator running
// interactively sees them too. Markers share the `----->` prefix the
// rest of the apply pipeline uses so the CLI's stream filter can
// render them as spinner steps.
//
// Always 200 — failure is communicated via the trailing marker
// line ("-----> Release X failed in command (exit 42)") so the CLI
// can read the body verbatim and decide on exit code at the end.
func (a *API) handleReleaseRun(w http.ResponseWriter, r *http.Request) {
	// Release-phase commands are deployment-only — statefulsets
	// don't carry a release block. resolveScope walks the
	// deployments tree to find an unambiguous bare-name target.
	scope, name, err := a.releaseTarget(r, KindDeployment)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	if a.Deployments == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("release runner not configured"))
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Voodu-Scope", scope)
	w.Header().Set("X-Voodu-Name", name)
	w.WriteHeader(http.StatusOK)

	// Wrap the response writer in a flusher so each line reaches
	// the client immediately rather than sitting in the chunked-
	// transfer buffer. Without this, CI would see the entire
	// migration output in one burst at the end.
	flushed := newFlushingWriter(w)

	if err := a.Deployments.Release(r.Context(), scope, name, flushed); err != nil {
		// Trailing line so the CLI can detect failure even though
		// status is already 200 by now. The Release method's
		// internal output already wrote a "-----> Release X failed"
		// marker; this is a fallback for errors that happen
		// outside the release runner (manifest read failure, etc.).
		// The "failed" keyword is what the CLI greps for to set
		// its exit code.
		fmt.Fprintf(flushed, "-----> Release failed: %v\n", err)
	}
}

// flushingWriter wraps an http.ResponseWriter so every Write
// triggers an immediate Flush — needed for real-time log streaming
// over chunked transfer encoding. Without this, Go's net/http
// buffers writes until the response handler returns, defeating the
// streaming UX entirely.
type flushingWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newFlushingWriter(w http.ResponseWriter) *flushingWriter {
	flusher, _ := w.(http.Flusher)

	return &flushingWriter{w: w, f: flusher}
}

func (fw *flushingWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)

	if fw.f != nil {
		fw.f.Flush()
	}

	return n, err
}

// handleRollback re-applies a specific past release's spec to the
// deployment. Heroku-style: operator picks the release ID
// (`vd rollback web 1ksdtcj7e`) and voodu re-Puts that snapshot
// into etcd, triggering a normal recreate flow. Migration doesn't
// re-run because the target's hash already has a Succeeded record.
//
//	POST /rollback?scope=&name=&release_id=
//
// release_id="" (or omitted): rollback to the release immediately
// before the current. Otherwise: exact target. Errors when the
// target doesn't exist or isn't a Succeeded release.
//
// Top-level /rollback (not /releases/rollback) because rollback
// is its own verb conceptually — the release is the snapshot;
// rollback is "go back to that snapshot". Different concerns.
func (a *API) handleRollback(w http.ResponseWriter, r *http.Request) {
	kindStr := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kindStr == "" {
		kindStr = string(KindDeployment)
	}

	kind, err := ParseKind(kindStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	scope, name, err := a.releaseTarget(r, kind)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	type rollbackFn func(ctx context.Context, scope, name, targetID string) (string, error)

	var fn rollbackFn

	switch kind {
	case KindDeployment:
		if a.Deployments == nil {
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("deployment rollback not configured"))
			return
		}

		fn = a.Deployments.Rollback

	case KindStatefulset:
		if a.Statefulsets == nil {
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("statefulset rollback not configured"))
			return
		}

		fn = a.Statefulsets.Rollback

	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("rollback not supported for kind %q", kind))
		return
	}

	targetID := strings.TrimSpace(r.URL.Query().Get("release_id"))

	newID, err := fn(r.Context(), scope, name, targetID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data: map[string]any{
			"scope":          scope,
			"name":           name,
			"kind":           string(kind),
			"rolled_back_to": targetID,
			"new_release":    newID,
		},
	})
}

// releaseTarget pulls and validates scope/name from query params,
// resolving an unambiguous bare name when scope is omitted. Shared
// helper because /releases/run and /rollback both need the same
// shape — kind selects which manifest tree resolveScope walks.
func (a *API) releaseTarget(r *http.Request, kind Kind) (string, string, error) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))

	if name == "" {
		return "", "", fmt.Errorf("name is required")
	}

	if scope == "" {
		resolved, err := resolveScope(r.Context(), a.Store, kind, name)
		if err != nil {
			return "", "", err
		}

		scope = resolved
	}

	return scope, name, nil
}

// handleCronJobRun forces an immediate execution of a previously-
// applied cronjob, bypassing the scheduler. Mirror shape of
// handleJobRun: synchronous, returns the run record alongside any
// error so the CLI can render exit code + duration.
//
//	POST /cronjobs/run?scope=<scope>&name=<name>
//
// Use case: an operator just shipped a fix to a cronjob and wants to
// verify it works before waiting for the next scheduled tick. The
// scheduler's normal cadence is unaffected — this just triggers one
// extra run synchronously.
func (a *API) handleCronJobRun(w http.ResponseWriter, r *http.Request) {
	if a.CronJobs == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("cronjob runner not configured"))
		return
	}

	name := strings.TrimSpace(r.URL.Query().Get("name"))
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))

	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("name is required"))
		return
	}

	if scope == "" {
		resolved, err := resolveScope(r.Context(), a.Store, KindCronJob, name)
		if err != nil {
			if errors.Is(err, errScopeNotFound) {
				writeErr(w, http.StatusNotFound, err)
				return
			}

			writeErr(w, http.StatusBadRequest, err)
			return
		}

		scope = resolved
	}

	run, err := a.CronJobs.Tick(r.Context(), scope, name)
	if err != nil {
		// Same as job run: status carries the failure already, return
		// the run record alongside the error so the CLI's --output
		// can render exit code + duration.
		writeJSON(w, http.StatusInternalServerError, envelope{
			Status: "error",
			Error:  err.Error(),
			Data:   run,
		})

		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: run})
}

// decodeManifests accepts either a single-object or array-of-objects body.
func decodeManifests(body []byte) ([]*Manifest, error) {
	trimmed := strings.TrimSpace(string(body))

	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty body")
	}

	if trimmed[0] == '[' {
		var list []*Manifest
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("decode manifest array: %w", err)
		}

		return list, nil
	}

	var single Manifest
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	return []*Manifest{&single}, nil
}

func readBody(r *http.Request) ([]byte, error) {
	const maxBody = 1 << 20

	return io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
}
