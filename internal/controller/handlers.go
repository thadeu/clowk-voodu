package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.voodu.clowk.in/internal/containers"
	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// slotRolloutPause is the fixed sleep between sequential slot
// recreates / restarts during a rollout. It is a blunt instrument —
// just enough time for docker to wire the new container onto voodu0
// so ingress traffic keeps landing on at least one healthy replica.
// A real "wait for /healthz 200" probe can replace this once the
// deployment spec carries a liveness endpoint the reconciler trusts.
const slotRolloutPause = 2 * time.Second

// pluginErrorDetail extracts the most informative error string from a
// non-zero plugin exit. Plugins emit structured errors via their JSON
// envelope on stdout (envelope.Error), not stderr — voodu-caddy's
// `emit(envelope{Status: "error", ...}); os.Exit(1)` is the canonical
// shape. Without looking at the envelope first, the controller reports
// a bare "exited 1" with an empty stderr and operators are left
// guessing. Fallbacks preserve visibility for non-envelope plugins:
// stderr, then raw stdout, then "no output".
func pluginErrorDetail(res *plugins.Result) string {
	if res == nil {
		return "no output"
	}

	if res.Envelope != nil && res.Envelope.Error != "" {
		return res.Envelope.Error
	}

	if s := strings.TrimSpace(string(res.Stderr)); s != "" {
		return s
	}

	if s := strings.TrimSpace(string(res.Raw)); s != "" {
		return s
	}

	return "no output"
}

// databaseSpec / deploymentSpec are minimal, package-local mirrors of
// internal/manifest types. We duplicate a handful of JSON tags rather
// than import the manifest package (which already imports controller
// for the wire Manifest type — the two-way dep would be a cycle).
// Each spec decoder only reads the fields the reconciler actually
// cares about, so drift between client-side parse and server-side
// handling is limited to the fields we explicitly surface below.
type databaseSpec struct {
	Engine  string            `json:"engine"`
	Version string            `json:"version,omitempty"`
	Storage string            `json:"storage,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
}

type deploymentSpec struct {
	Image       string            `json:"image,omitempty"`
	Replicas    int               `json:"replicas,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Ports       []string          `json:"ports,omitempty"`
	Volumes     []string          `json:"volumes,omitempty"`
	Network     string            `json:"network,omitempty"`
	Networks    []string          `json:"networks,omitempty"`
	NetworkMode string            `json:"network_mode,omitempty"`
	Restart     string            `json:"restart,omitempty"`
	HealthCheck string            `json:"health_check,omitempty"`
	Release     *releaseSpec      `json:"release,omitempty"`
}

// releaseSpec mirrors manifest.ReleaseSpec but lives in the
// controller package so the handler can decode it from the
// deployment manifest's Spec blob without importing internal/manifest.
type releaseSpec struct {
	Command     []string `json:"command,omitempty"`
	PreCommand  []string `json:"pre_command,omitempty"`
	PostCommand []string `json:"post_command,omitempty"`
	Timeout     string   `json:"timeout,omitempty"`
}

// effectiveReplicas normalizes the replica count: manifest omits
// `replicas` → 1 (the overwhelmingly common shape). Negative values
// are clamped to 1 because zero-replica deployments have no meaning in
// the current architecture (we don't pause/drain; removing the manifest
// is how you scale to zero).
func effectiveReplicas(spec deploymentSpec) int {
	if spec.Replicas < 1 {
		return 1
	}

	return spec.Replicas
}

// newSlotName produces a fresh container name for a deployment
// replica. The replica id is opaque hex (4 chars) and exists only to
// disambiguate sibling containers in docker's flat namespace —
// replicas of the same deployment are interchangeable, and the
// reconciler should never depend on a specific id surviving across
// reconciles.
//
// Pre-M0 voodu used `<app>-<N>` with a numeric suffix (slot 0, slot
// 1, ...). Existing code that needs to look up a deployment's
// running containers must now query labels (Containers.ListByIdentity)
// instead of constructing names — names are no longer deterministic.
func newSlotName(scope, name string) string {
	return containers.ContainerName(scope, name, containers.NewReplicaID())
}

// DatabaseHandler reconciles database manifests by dispatching to the
// engine's plugin. It is intentionally thin: it does not spawn
// containers or manage storage itself — that's the plugin's job. The
// handler's responsibilities are exactly three:
//
//  1. On first-seen (no status yet): call <engine>:create and persist
//     whatever the plugin returns under /status/databases/<name>.
//  2. On delete: call <engine>:destroy and drop the status key.
//  3. Stay idempotent across restarts — the reconciler replays every
//     manifest on boot, and a working database must not be torn down
//     just because we restarted.
//
// Updates (version bump, params change) are not handled yet. The
// pragmatic reason: there's no generic "update" primitive on the
// plugin side — each engine would need its own migration story.
// Today the operator reapplies by deleting + re-creating; a later
// milestone can add <engine>:update once we know what it should mean.
type DatabaseHandler struct {
	Store   Store
	Invoker PluginInvoker
	Log     *log.Logger
}

// DatabaseStatus is the shape persisted at /status/databases/<name>. It
// wraps whatever the plugin returned in Data so callers (the deployment
// reconciler, voodu status) can inspect without re-running the plugin.
//
// Engine/Version/Params are copied from the desired manifest so status
// readers don't need to cross-reference /desired to know which plugin
// produced the data, AND so the handler can detect drift on replay
// without a second etcd round-trip. A Version diff is the canonical
// "operator meant to upgrade" signal.
type DatabaseStatus struct {
	Engine  string            `json:"engine"`
	Version string            `json:"version,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
	Data    map[string]any    `json:"data,omitempty"`
}

// Handle implements HandlerFunc. It dispatches on WatchEvent type and
// delegates to create/destroy. Errors bubble up to the reconciler,
// which logs + retries transient ones.
func (h *DatabaseHandler) Handle(ctx context.Context, ev WatchEvent) error {
	switch ev.Type {
	case WatchPut:
		return h.create(ctx, ev)

	case WatchDelete:
		return h.destroy(ctx, ev)
	}

	return nil
}

func (h *DatabaseHandler) create(ctx context.Context, ev WatchEvent) error {
	if ev.Manifest == nil {
		return fmt.Errorf("put event without manifest")
	}

	existing, err := h.Store.GetStatus(ctx, KindDatabase, ev.Name)
	if err != nil {
		return err
	}

	spec, err := decodeDBSpec(ev.Manifest)
	if err != nil {
		return err
	}

	if existing != nil {
		// Already reconciled. Check for drift — the most common case is
		// a no-op replay, but version/params changes deserve operator
		// visibility. We don't auto-migrate (plugins don't yet speak
		// `update`); logging is the escape hatch: the operator runs
		// `voodu delete` + re-apply when they see the warning.
		h.checkDBDrift(existing, spec, ev.Name)
		return nil
	}

	if spec.Engine == "" {
		return fmt.Errorf("database/%s: engine is required", ev.Name)
	}

	env := databaseCreateEnv(ev.Name, spec)

	res, err := h.Invoker.Invoke(ctx, spec.Engine, "create", []string{ev.Name}, env)
	if err != nil {
		return err
	}

	if res.ExitCode != 0 {
		return fmt.Errorf("%s create exited %d: %s", spec.Engine, res.ExitCode, pluginErrorDetail(res))
	}

	status := DatabaseStatus{
		Engine:  spec.Engine,
		Version: spec.Version,
		Params:  spec.Params,
	}

	if res.Envelope != nil {
		if res.Envelope.Status == "error" {
			return fmt.Errorf("%s create: %s", spec.Engine, res.Envelope.Error)
		}

		status.Data = envelopeDataAsMap(res.Envelope.Data)
	}

	blob, err := json.Marshal(status)
	if err != nil {
		return err
	}

	if err := h.Store.PutStatus(ctx, KindDatabase, ev.Name, blob); err != nil {
		return err
	}

	h.logf("database/%s ready (engine=%s)", ev.Name, spec.Engine)

	return nil
}

func (h *DatabaseHandler) destroy(ctx context.Context, ev WatchEvent) error {
	// Engine was known at create time but the WatchEvent for delete
	// carries no manifest, so read engine from the persisted status.
	raw, err := h.Store.GetStatus(ctx, KindDatabase, ev.Name)
	if err != nil {
		return err
	}

	if raw == nil {
		// Never reconciled or already cleaned up. Nothing to do.
		return nil
	}

	var status DatabaseStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return fmt.Errorf("decode status: %w", err)
	}

	if status.Engine == "" {
		// Defensive: no engine recorded means we can't call the plugin
		// safely. Clear the status key anyway so the user can retry.
		return h.Store.DeleteStatus(ctx, KindDatabase, ev.Name)
	}

	res, err := h.Invoker.Invoke(ctx, status.Engine, "destroy", []string{ev.Name}, map[string]string{
		plugin.EnvApp: ev.Name,
	})
	if err != nil {
		return err
	}

	if res.ExitCode != 0 {
		return fmt.Errorf("%s destroy exited %d: %s", status.Engine, res.ExitCode, pluginErrorDetail(res))
	}

	if err := h.Store.DeleteStatus(ctx, KindDatabase, ev.Name); err != nil {
		return err
	}

	h.logf("database/%s destroyed", ev.Name)

	return nil
}

// checkDBDrift compares the desired spec against the persisted status
// and logs a warning for each meaningful difference. No-op on no drift.
// Kept separate from the create path so the already-reconciled case
// stays cheap: a single unmarshal, a few string compares, done.
func (h *DatabaseHandler) checkDBDrift(existingStatus []byte, spec databaseSpec, name string) {
	var prev DatabaseStatus

	if err := json.Unmarshal(existingStatus, &prev); err != nil {
		// Corrupt status blob is a separate problem; surface it but
		// don't pretend we detected drift we couldn't measure.
		h.logf("database/%s status decode failed: %v", name, err)
		return
	}

	if prev.Engine != "" && prev.Engine != spec.Engine {
		h.logf("database/%s engine drift: running=%s desired=%s — manual delete+recreate required", name, prev.Engine, spec.Engine)
	}

	if prev.Version != spec.Version {
		h.logf("database/%s version drift: running=%q desired=%q — plugin `update` not wired; delete+recreate to apply", name, prev.Version, spec.Version)
	}

	if !stringMapsEqual(prev.Params, spec.Params) {
		h.logf("database/%s params drift — plugin `update` not wired; delete+recreate to apply", name)
	}
}

// stringMapsEqual is the package-level equality helper used by both
// DB drift detection and the env-changed signal in server.go's
// WriteEnv callback. reflect.DeepEqual would work but this is cheaper
// and sidesteps dragging reflect into hot reconcile loops.
func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}

	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}

	return true
}

// decodeDBSpec re-decodes the wire JSON into a typed spec. The
// reconciler gets raw json.RawMessage Specs from etcd, so we can't avoid
// a second parse — the upside is type safety on spec.Engine, etc.
func decodeDBSpec(m *Manifest) (databaseSpec, error) {
	var spec databaseSpec

	if len(m.Spec) == 0 {
		return spec, fmt.Errorf("database/%s: empty spec", m.Name)
	}

	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return spec, fmt.Errorf("decode database spec: %w", err)
	}

	return spec, nil
}

// databaseCreateEnv packs the spec into the env map the plugin receives.
// We avoid positional CLI args for anything optional — env is more
// extensible: new spec fields become new env vars without breaking
// existing plugins that ignore them.
func databaseCreateEnv(name string, spec databaseSpec) map[string]string {
	env := map[string]string{
		plugin.EnvApp:       name,
		plugin.EnvDBName:    name,
		plugin.EnvDBEngine:  spec.Engine,
		plugin.EnvDBVersion: spec.Version,
		plugin.EnvDBStorage: spec.Storage,
	}

	if len(spec.Params) > 0 {
		if b, err := json.Marshal(spec.Params); err == nil {
			env[plugin.EnvDBParams] = string(b)
		}
	}

	return env
}

// envelopeDataAsMap normalises the plugin's Data field to a string-keyed
// map. Plugins that emit `{"url": "..."}` produce map[string]any when
// json.Decode targets `any`; handle the two common shapes (object, nil)
// and fall through for anything unexpected.
func envelopeDataAsMap(data any) map[string]any {
	if data == nil {
		return nil
	}

	if m, ok := data.(map[string]any); ok {
		return m
	}

	// A plugin that emitted `"data": "some string"` or similar can't be
	// referenced by field, but we still persist the raw value under a
	// `value` key so voodu status shows something useful.
	return map[string]any{"value": data}
}

func (h *DatabaseHandler) logf(format string, args ...any) {
	if h.Log == nil {
		return
	}

	h.Log.Printf(format, args...)
}

// DeploymentHandler reconciles deployment manifests. Three jobs:
//
//  1. Link refs into env — resolve ${ref.<kind>.<name>.<field>} against
//     the status store and persist the result to the app's .env file.
//  2. Ensure a container is running — when spec.Image is set, delegate
//     to ContainerManager.Ensure on first sight.
//  3. Restart on env change — if the env actually changed and we did
//     not just create a fresh container (fresh containers already
//     pick up the new env), cycle the container so the process sees
//     the new values.
//
// Registry-mode vs build-mode split:
//
//   - spec.Image != "" (registry-mode): the manifest names a pullable
//     image; the handler owns the full container lifecycle.
//   - spec.Image == "" (build-mode): the image is built from a tarball
//     the CLI streams over SSH before POSTing the manifests. `voodu
//     receive-pack` extracts the tarball and runs the build/swap/
//     start-container pipeline. The handler here still runs linkEnv so
//     ${ref.*} resolves to the on-disk .env file that pipeline will
//     mount, but it leaves container orchestration to that path. A
//     future refactor can collapse both modes under the handler once
//     receive-pack is demoted to a pure build step.
type DeploymentHandler struct {
	Store Store
	Log   *log.Logger

	// WriteEnv persists (app, KEY=VAL...) to the app's env file and
	// reports whether anything actually changed. The changed flag gates
	// container restart — re-running the same reconcile on startup
	// replays every manifest, and restarting unchanged apps is the
	// kind of noise that burns uptime for nothing.
	WriteEnv func(app string, pairs []string) (changed bool, err error)

	// EnvFilePath resolves an app name to the .env path passed to the
	// container runtime. Optional — when nil, containers start without
	// --env-file (useful in tests). Production wires paths.AppEnvFile.
	EnvFilePath func(app string) string

	// Containers is the runtime surface for ensure/restart. Optional:
	// when nil, the handler stays env-only and skips spawn/restart.
	Containers ContainerManager

	// releaseLocks serialises release execution per-deployment.
	// Two concurrent `vd release run` (or apply-time auto-trigger
	// racing with manual run) for the same deployment are forced
	// to be sequential — preventing the classic "two migrations
	// fighting for the same DB transaction" footgun. Other
	// deployments run concurrently as today; the lock granularity
	// is (scope, name).
	//
	// Map value is *sync.Mutex. Acquired via TryLock so an
	// already-running release fails fast with a clear error
	// instead of silently queueing.
	releaseLocks sync.Map
}

func (h *DeploymentHandler) Handle(ctx context.Context, ev WatchEvent) error {
	switch ev.Type {
	case WatchPut:
		return h.apply(ctx, ev)

	case WatchDelete:
		return h.remove(ctx, ev)
	}

	return nil
}

// remove tears down every container that belongs to this deployment's
// AppID and clears its status blob. Filesystem state (env file, release
// dirs, shared volumes) is intentionally left in place — two reasons:
//
//   1. `voodu config set` writes secrets that outlive any single manifest
//      revision. A user who re-applies the deployment expects their
//      secrets to still be there.
//   2. A release dir carries the build context from the last `voodu
//      deploy`. Keeping it around lets the user roll back by re-applying
//      without re-building.
//
// Operators who want a full wipe still have the shell: `rm -rf
// /opt/voodu/apps/<app>` is the explicit, loud way to do that.
func (h *DeploymentHandler) remove(ctx context.Context, ev WatchEvent) error {
	app := AppID(ev.Scope, ev.Name)

	if h.Containers != nil {
		// Two passes: live (M0-labeled) replicas, then any pre-M0
		// containers still around. Both must go on a delete — the
		// user's mental model is "manifest gone, runtime gone".
		slots, err := h.Containers.ListByIdentity(string(KindDeployment), ev.Scope, ev.Name)
		if err != nil {
			return fmt.Errorf("list replicas: %w", err)
		}

		for _, slot := range slots {
			h.logf("deployment/%s removing replica %s", ev.Name, slot.Name)

			if err := h.Containers.Remove(slot.Name); err != nil {
				return fmt.Errorf("remove %s: %w", slot.Name, err)
			}
		}

		legacy, err := h.Containers.ListLegacyByApp(app)
		if err != nil {
			return fmt.Errorf("list legacy replicas: %w", err)
		}

		for _, name := range legacy {
			h.logf("deployment/%s removing legacy replica %s", ev.Name, name)

			if err := h.Containers.Remove(name); err != nil {
				return fmt.Errorf("remove %s: %w", name, err)
			}
		}
	}

	// Status blob is keyed by AppID (see writeDeploymentStatus). Clearing
	// it matters: the next `voodu apply` of the same name re-baselines
	// cleanly instead of comparing fresh containers against a stale
	// spec-hash from the previous incarnation.
	if err := h.Store.DeleteStatus(ctx, KindDeployment, app); err != nil {
		return fmt.Errorf("clear deployment status: %w", err)
	}

	h.logf("deployment/%s deleted (containers removed, env+releases left intact)", ev.Name)

	return nil
}

func (h *DeploymentHandler) apply(ctx context.Context, ev WatchEvent) error {
	if ev.Manifest == nil {
		return fmt.Errorf("put event without manifest")
	}

	spec, err := decodeDeploymentSpec(ev.Manifest)
	if err != nil {
		return err
	}

	// Single on-host identity for this deployment: container slots,
	// image tag, env file, release directory — everything is keyed by
	// AppID so two scopes can both declare `deployment "web"` without
	// fighting over the same docker name or filesystem path.
	app := AppID(ev.Scope, ev.Name)

	// Normalize image, networks, ports, restart policy in one place
	// so Release()/Restart() (which decode the same spec from the
	// store) get an identical shape. See applyDeploymentSpecDefaults
	// for what the contract covers.
	if err := applyDeploymentSpecDefaults(&spec, app); err != nil {
		return err
	}

	// Always link env, even when spec.Env is empty. Two reasons:
	//   1. docker run is invoked with --env-file unconditionally, so the
	//      file must exist or the container fails to start.
	//   2. `voodu config set` writes to the same file; linkEnv's Load/
	//      Save round-trip preserves those values when the spec declares
	//      no Env block of its own.
	envChanged, err := h.linkEnv(ctx, ev.Scope, ev.Name, app, spec.Env)
	if err != nil {
		return err
	}

	if h.Containers == nil {
		return nil
	}

	replicas := effectiveReplicas(spec)

	// Pre-M0 containers (no voodu.* labels, names like `<app>` or
	// `<app>-<N>`) are detected by name pattern and removed up front.
	// They can't be adopted in place — labels are set at create time
	// — so the only safe path is replace-on-next-apply. The rolling
	// recreate below handles any visible churn.
	if spec.Image != "" {
		if err := h.pruneLegacyContainers(app); err != nil {
			h.logf("deployment/%s legacy prune failed: %v", ev.Name, err)
		}
	}

	// Identity-based replica reconcile. Three signals from the live
	// runtime drive the decision:
	//
	//   live  — M0-labeled containers matching (kind, scope, name)
	//   want  — desired replica count
	//   delta — sign tells us whether to add, remove, or hold steady
	//
	// Replicas are interchangeable, so removal picks any extras and
	// addition picks fresh names. No notion of "which slot index" —
	// that was the pre-M0 model and it carried the wrong implication.
	live, err := h.Containers.ListByIdentity(string(KindDeployment), ev.Scope, ev.Name)
	if err != nil {
		return fmt.Errorf("list deployment %s replicas: %w", app, err)
	}

	hash := deploymentSpecHash(spec)

	createdNames, err := h.ensureReplicaCount(ev.Scope, ev.Name, app, live, replicas, spec, hash)
	if err != nil {
		return err
	}

	createdSet := setOf(createdNames)
	createdAny := len(createdNames) > 0

	if err := h.pruneExtraReplicas(ev.Name, app, live, replicas, createdSet); err != nil {
		h.logf("deployment/%s scale-down failed: %v", ev.Name, err)
	}

	if createdAny {
		// Baseline the spec hash so the next reconcile has something
		// to compare against. Without this, the very next apply would
		// see no persisted status and treat a real drift as first-seen.
		if err := h.writeDeploymentStatus(ctx, app, spec.Image, hash); err != nil {
			h.logf("deployment/%s status persist failed: %v", ev.Name, err)
		}
	}

	// Spec drift trumps restart: any change in runtime-relevant fields
	// (image, ports, volumes, network, restart policy, command) means
	// the running containers have the wrong shape — Recreate absorbs
	// env changes, so no follow-up restart either. Rollout is
	// sequential, one replica at a time, with a short pause so ingress
	// always has at least one healthy peer to route to.
	recreatedAny := false

	if spec.Image != "" {
		// Re-list after the create+prune passes so the drift loop
		// targets the post-scale set, not stale candidates that have
		// already been removed.
		current, err := h.Containers.ListByIdentity(string(KindDeployment), ev.Scope, ev.Name)
		if err != nil {
			return fmt.Errorf("list deployment %s replicas (post-scale): %w", app, err)
		}

		// The just-created replicas already carry the desired
		// manifest_hash — they don't need to be recreated again. Skip
		// them so a fresh deployment doesn't churn through its brand
		// new containers.
		toCheck := filterSlots(current, func(s ContainerSlot) bool {
			_, justCreated := createdSet[s.Name]
			return !justCreated
		})

		r, err := h.recreateReplicasIfSpecChanged(ctx, ev.Scope, ev.Name, app, toCheck, spec, hash)
		if err != nil {
			return err
		}

		recreatedAny = r
	}

	// Fresh/recreated containers come up with the current .env already
	// mounted, so restarting right after is redundant churn. Only
	// cycle the replicas that were neither freshly created this reconcile
	// nor just recreated (recreate already absorbed the env), and only
	// when env actually moved.
	if envChanged && !recreatedAny {
		// Re-query because recreate may have left the live set
		// untouched but env-only restart still needs a fresh view.
		current, err := h.Containers.ListByIdentity(string(KindDeployment), ev.Scope, ev.Name)
		if err == nil {
			h.restartReplicas(ev.Name, current, createdSet)
		}
	}

	return nil
}

// setOf is the obvious string-slice → set helper. Used by apply() to
// remember which container names this reconcile JUST created so the
// drift / restart paths can skip them.
func setOf(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))

	for _, n := range names {
		out[n] = struct{}{}
	}

	return out
}

// filterSlots returns the ContainerSlots for which keep returns true.
// Preserves order of the input so logs read predictably.
func filterSlots(in []ContainerSlot, keep func(ContainerSlot) bool) []ContainerSlot {
	out := make([]ContainerSlot, 0, len(in))

	for _, s := range in {
		if keep(s) {
			out = append(out, s)
		}
	}

	return out
}

// ensureReplicaCount creates `want - len(live)` new replicas when the
// deployment is short, no-ops otherwise. Returns the names of the
// just-created containers so the caller can skip restart/recreate on
// them (they came up with the current spec already).
//
// Each new replica gets a fresh opaque replica id (4-char hex) and
// the full voodu.* label set, so the next reconcile finds it by
// label without needing the controller to remember the names.
func (h *DeploymentHandler) ensureReplicaCount(scope, name, app string, live []ContainerSlot, want int, spec deploymentSpec, hash string) ([]string, error) {
	if spec.Image == "" {
		// Build-mode without an image is the receive-pack pipeline's
		// territory; the handler stays env-only here.
		return nil, nil
	}

	have := len(live)
	if have >= want {
		return nil, nil
	}

	envFile := ""
	if h.EnvFilePath != nil {
		envFile = h.EnvFilePath(app)
	}

	created := make([]string, 0, want-have)

	for i := have; i < want; i++ {
		replicaID := containers.NewReplicaID()
		cname := containers.ContainerName(scope, name, replicaID)

		labels := containers.BuildLabels(containers.Identity{
			Kind:         containers.KindDeployment,
			Scope:        scope,
			Name:         name,
			ReplicaID:    replicaID,
			ManifestHash: hash,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		})

		_, err := h.Containers.Ensure(ContainerSpec{
			Name:           cname,
			Image:          spec.Image,
			Command:        spec.Command,
			Ports:          spec.Ports,
			Volumes:        spec.Volumes,
			Networks:       spec.Networks,
			NetworkMode:    spec.NetworkMode,
			NetworkAliases: BuildNetworkAliases(scope, name),
			Restart:        spec.Restart,
			EnvFile:        envFile,
			Labels:         labels,
		})
		if err != nil {
			return created, fmt.Errorf("ensure %s: %w", cname, err)
		}

		h.logf("deployment/%s replica %s created (image=%s)", name, cname, spec.Image)

		created = append(created, cname)
	}

	return created, nil
}

// pruneLegacyContainers removes pre-M0 containers (`<app>`,
// `<app>-<N>`) that lack voodu.* labels. The reconcile path that
// follows then provisions M0 replicas to take their place. We do
// not try to adopt them in place — labels can't be applied to a
// stopped container.
func (h *DeploymentHandler) pruneLegacyContainers(app string) error {
	if h.Containers == nil {
		return nil
	}

	legacy, err := h.Containers.ListLegacyByApp(app)
	if err != nil {
		return err
	}

	for _, name := range legacy {
		h.logf("deployment/%s removing legacy non-M0 container %s", app, name)

		if err := h.Containers.Remove(name); err != nil {
			return fmt.Errorf("remove legacy %s: %w", name, err)
		}
	}

	return nil
}

// pruneExtraReplicas removes any live replicas above the desired
// count. Selection of which to kill is intentionally not based on
// "highest index" anymore — replicas are interchangeable. We sort by
// container name for determinism (stable ordering across reconciles
// helps log diffs) and keep the first `want`, dropping the rest.
//
// The just-created replicas (createdSet) are guaranteed to survive
// this pass: they're new, the operator just asked for them, and we
// already counted them in the live set when computing `have`.
func (h *DeploymentHandler) pruneExtraReplicas(name, app string, live []ContainerSlot, want int, createdSet map[string]struct{}) error {
	if h.Containers == nil {
		return nil
	}

	if len(live) <= want {
		return nil
	}

	// Sort by name so the same set of containers always picks the
	// same survivors. Without this the choice would depend on docker
	// ps ordering, which can shuffle between calls.
	candidates := append([]ContainerSlot(nil), live...)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})

	keep := make(map[string]struct{}, want)

	// Always keep just-created replicas first — they're the freshest
	// and the operator's most recent intent.
	for cname := range createdSet {
		keep[cname] = struct{}{}
	}

	// Fill remaining slots from the sorted candidates.
	for _, s := range candidates {
		if len(keep) >= want {
			break
		}

		if _, already := keep[s.Name]; already {
			continue
		}

		keep[s.Name] = struct{}{}
	}

	for _, s := range candidates {
		if _, k := keep[s.Name]; k {
			continue
		}

		h.logf("deployment/%s scale-down: removing %s", name, s.Name)

		if err := h.Containers.Remove(s.Name); err != nil {
			return fmt.Errorf("remove %s: %w", s.Name, err)
		}
	}

	return nil
}

// restartReplicas cycles each replica in sequence with a short pause
// between cycles so ingress load-balances onto the still-running
// peers during the rollout. Skips just-created replicas in `skip`
// since fresh containers already carry the current env.
func (h *DeploymentHandler) restartReplicas(name string, live []ContainerSlot, skip map[string]struct{}) {
	targets := filterSlots(live, func(s ContainerSlot) bool {
		_, just := skip[s.Name]
		return !just
	})

	for i, s := range targets {
		if err := h.Containers.Restart(s.Name); err != nil {
			h.logf("deployment/%s replica %s restart failed (env already written): %v", name, s.Name, err)
			continue
		}

		h.logf("deployment/%s replica %s restarted (env changed)", name, s.Name)

		if i < len(targets)-1 {
			time.Sleep(slotRolloutPause)
		}
	}
}

// recreateReplicasIfSpecChanged detects drift against the persisted
// spec hash and image ID, then rolls the fleet one replica at a time
// when drift is real. Single shared hash across replicas — they're
// siblings by construction.
//
// When no status exists yet (first reconcile after a controller upgrade
// that introduced status persistence) we baseline the hash without
// recreating: the running containers may well match the spec, and
// churning every pre-existing deploy on upgrade is exactly the kind of
// surprise this handler is meant to avoid.
//
// On recreate we generate a fresh replica id for the new container —
// the old one is gone, so there's no name to reuse, and the opaque
// id keeps emphasising that replicas are interchangeable.
func (h *DeploymentHandler) recreateReplicasIfSpecChanged(ctx context.Context, scope, name, app string, live []ContainerSlot, spec deploymentSpec, hash string) (bool, error) {
	raw, err := h.Store.GetStatus(ctx, KindDeployment, app)
	if err != nil {
		return false, fmt.Errorf("read deployment status: %w", err)
	}

	if raw == nil {
		// Baseline — no recreate, just record what's running so the
		// next apply has a reference point.
		if err := h.writeDeploymentStatus(ctx, app, spec.Image, hash); err != nil {
			h.logf("deployment/%s status persist failed: %v", app, err)
		}

		return false, nil
	}

	var prev DeploymentStatus
	if err := json.Unmarshal(raw, &prev); err != nil {
		// Corrupt status: treat as missing and re-baseline. Alternative
		// would be erroring out, but that traps the user — they'd have
		// to hand-edit etcd to unblock a reconcile.
		h.logf("deployment/%s status decode failed, re-baselining: %v", app, err)

		if err := h.writeDeploymentStatus(ctx, app, spec.Image, hash); err != nil {
			h.logf("deployment/%s status persist failed: %v", app, err)
		}

		return false, nil
	}

	recreateNeeded := prev.SpecHash != hash
	reason := ""

	if recreateNeeded {
		reason = fmt.Sprintf("spec drift (hash %s → %s)", shortHash(prev.SpecHash), shortHash(hash))
	} else if len(live) > 0 {
		// Spec text is stable, but build-mode rebuilds `<app>:latest` on
		// every apply — the tag string is identical yet the underlying
		// image ID changes. Containers freeze the image ID at create
		// time, so the running process stays on the old layers until we
		// explicitly recreate. Spec-hash can't catch this (manifest text
		// didn't move); only an image-ID comparison can. The first live
		// replica is the canary — all replicas share the image, so
		// checking one is enough.
		differ, err := h.Containers.ImageIDsDiffer(live[0].Name, spec.Image)
		if err != nil {
			// Treat ID-check errors as "no drift" to avoid unnecessary
			// recreates on transient docker CLI failures. The next apply
			// will try again.
			h.logf("deployment/%s image id check failed: %v", app, err)
			return false, nil
		}

		if differ {
			recreateNeeded = true
			reason = fmt.Sprintf("image id drift (tag %s rebuilt under same name)", spec.Image)
		}
	}

	if !recreateNeeded {
		return false, nil
	}

	// Deployments with a release block are NOT auto-restarted by
	// the reconciler — the operator's `vd apply` (CLI) calls
	// /releases/run after persisting, which orchestrates release
	// command + rolling restart with streaming logs back to the
	// caller. Doing both in the reconciler AND the CLI would race
	// on the per-deployment lock.
	//
	// For deployments without a release block, the reconciler is
	// the trigger — same as before. Spec change → rolling restart.
	if spec.Release != nil {
		h.logf("deployment/%s %s, but release block present; awaiting `vd release run` for orchestrated restart", app, reason)

		return false, nil
	}

	h.logf("deployment/%s %s, recreating %d replica(s)", app, reason, len(live))

	if err := h.rollingReplaceReplicas(ctx, scope, name, app, live, spec, hash); err != nil {
		return false, err
	}

	if err := h.writeDeploymentStatus(ctx, app, spec.Image, hash); err != nil {
		h.logf("deployment/%s status persist failed: %v", app, err)
	}

	return true, nil
}

// rollingReplaceReplicas is the shared rolling-replace loop used by
// both the spec-drift recreate path and the imperative `vd restart`
// path. Each replica is replaced one at a time with slotRolloutPause
// between to keep zero-downtime under a load balancer.
//
// Replacements get a fresh replica id (the old name disappears, so
// the new container avoids docker --name collisions). The rest of
// the spec — image, command, networks, env file, labels — comes
// from the same source the apply path uses, so a restart never
// drifts from what apply would produce.
func (h *DeploymentHandler) rollingReplaceReplicas(_ context.Context, scope, name, app string, live []ContainerSlot, spec deploymentSpec, hash string) error {
	envFile := ""
	if h.EnvFilePath != nil {
		envFile = h.EnvFilePath(app)
	}

	for i, s := range live {
		newReplicaID := containers.NewReplicaID()
		newName := containers.ContainerName(scope, name, newReplicaID)

		labels := containers.BuildLabels(containers.Identity{
			Kind:         containers.KindDeployment,
			Scope:        scope,
			Name:         name,
			ReplicaID:    newReplicaID,
			ManifestHash: hash,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		})

		if err := h.Containers.Remove(s.Name); err != nil {
			return fmt.Errorf("remove %s during rolling restart: %w", s.Name, err)
		}

		if _, err := h.Containers.Ensure(ContainerSpec{
			Name:           newName,
			Image:          spec.Image,
			Command:        spec.Command,
			Ports:          spec.Ports,
			Volumes:        spec.Volumes,
			Networks:       spec.Networks,
			NetworkMode:    spec.NetworkMode,
			NetworkAliases: BuildNetworkAliases(scope, name),
			Restart:        spec.Restart,
			EnvFile:        envFile,
			Labels:         labels,
		}); err != nil {
			return fmt.Errorf("spawn replacement %s: %w", newName, err)
		}

		h.logf("deployment/%s replica %s replaced by %s", name, s.Name, newName)

		if i < len(live)-1 {
			time.Sleep(slotRolloutPause)
		}
	}

	return nil
}

// Restart performs an imperative rolling restart of every live
// replica of the named deployment, regardless of spec drift. Used
// by `vd restart <scope>/<name>` to refresh long-running processes
// after migrations / config changes / image rebuilds without
// requiring a manifest edit.
//
// The flow mirrors a normal apply-time recreate (rolling, one
// replica at a time, slotRolloutPause between) but bypasses the
// hash check — the operator EXPLICITLY asked for restart, so
// "spec unchanged" doesn't short-circuit. Status hash isn't
// rewritten because the spec didn't actually drift; reapply
// continues to be authoritative for that.
func (h *DeploymentHandler) Restart(ctx context.Context, scope, name string) error {
	app := AppID(scope, name)

	manifest, err := h.Store.Get(ctx, KindDeployment, scope, name)
	if err != nil {
		return fmt.Errorf("read deployment manifest: %w", err)
	}

	if manifest == nil {
		return fmt.Errorf("deployment/%s/%s not found", scope, name)
	}

	spec, err := decodeDeploymentSpec(manifest)
	if err != nil {
		return err
	}

	if err := applyDeploymentSpecDefaults(&spec, app); err != nil {
		return err
	}

	live, err := h.Containers.ListByIdentity(string(KindDeployment), scope, name)
	if err != nil {
		return fmt.Errorf("list replicas: %w", err)
	}

	if len(live) == 0 {
		return fmt.Errorf("deployment/%s has no live replicas to restart", app)
	}

	// Refresh env before restart so any /config changes accumulated
	// since the last apply land in the new replicas. Mirrors what
	// the regular apply path does.
	if _, err := h.linkEnv(ctx, scope, name, app, spec.Env); err != nil {
		return fmt.Errorf("link env: %w", err)
	}

	hash := deploymentSpecHash(spec)

	h.logf("deployment/%s rolling restart of %d replica(s) requested", app, len(live))

	return h.rollingReplaceReplicas(ctx, scope, name, app, live, spec, hash)
}

// linkEnv resolves refs and writes the result to the app's env file.
// Split out of apply() so the ensure-container step can run independently
// when the manifest has no env block. Returns whether the write actually
// changed any value — used to gate restart.
//
// scope is the deployment's scope — used by refLookup to AppID-ify
// lookups for scoped kinds (ingress) so a deployment in scope
// "clowk-lp" resolves `${ref.ingress.api}` against its own scope's
// ingress, not another scope's same-named one.
func (h *DeploymentHandler) linkEnv(ctx context.Context, scope, name, app string, env map[string]string) (bool, error) {
	lookup := h.refLookup(ctx, scope)

	// Pull controller-managed config (scope-level + app-level merged)
	// and overlay the manifest's declared env on top. Precedence:
	//
	//   scope config (via /config -s scope)
	//   ↓
	//   app config (via /config -s scope -n name)
	//   ↓
	//   manifest spec.env (HCL `env = { ... }`)
	//
	// Manifest wins because the HCL is the declarative source of
	// truth; config fills in everything the manifest deliberately
	// left out (secrets, per-environment values). If the operator
	// wants config to override a manifest key, they should remove
	// it from the manifest.
	merged := map[string]string{}

	if h.Store != nil {
		ctrlConfig, err := h.Store.ResolveConfig(ctx, scope, name)
		if err == nil {
			for k, v := range ctrlConfig {
				merged[k] = v
			}
		}
	}

	for k, v := range env {
		merged[k] = v
	}

	resolved, err := InterpolateRefsMap(merged, lookup)
	if err != nil {
		// Unresolved refs are almost always a timing race: the
		// referenced resource hasn't been reconciled yet. Mark
		// transient so the reconciler retries after the DB (or other
		// dependency) lands.
		return false, Transient(err)
	}

	if h.WriteEnv == nil {
		return false, fmt.Errorf("no WriteEnv configured — reconciler cannot persist env")
	}

	pairs := envMapToPairs(resolved)

	changed, err := h.WriteEnv(app, pairs)
	if err != nil {
		return false, fmt.Errorf("write env: %w", err)
	}

	h.logf("deployment/%s env linked (%d keys, changed=%v)", app, len(pairs), changed)

	return changed, nil
}


// refLookup closes over the store so InterpolateRefsMap can resolve
// ${ref.<kind>.<name>.<field>} by reading /status/<kind>s/<name>.
//
// The status shape is deliberately uniform: every kind persists a blob
// that has a top-level `data` object. That way this closure doesn't
// need to switch on kind — any future kind (ingress, queue, job)
// becomes referenceable just by following the convention.
//
// Scoped-kind status is keyed by AppID, so the closure prefixes `name`
// with `scope-` for any IsScoped kind. Unscoped kinds (database) stay
// keyed by bare name. scope is the calling deployment's scope — that's
// the lens through which refs resolve.
func (h *DeploymentHandler) refLookup(ctx context.Context, scope string) RefLookup {
	return func(kind, name, field string) (string, bool) {
		k, err := ParseKind(kind)
		if err != nil {
			return "", false
		}

		key := name
		if IsScoped(k) {
			key = AppID(scope, name)
		}

		raw, err := h.Store.GetStatus(ctx, k, key)
		if err != nil || raw == nil {
			return "", false
		}

		var envelope struct {
			Data map[string]any `json:"data,omitempty"`
		}

		if err := json.Unmarshal(raw, &envelope); err != nil {
			return "", false
		}

		v, ok := envelope.Data[field]
		if !ok {
			return "", false
		}

		return fmt.Sprintf("%v", v), true
	}
}

func decodeDeploymentSpec(m *Manifest) (deploymentSpec, error) {
	var spec deploymentSpec

	if len(m.Spec) == 0 {
		return spec, fmt.Errorf("deployment/%s: empty spec", m.Name)
	}

	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return spec, fmt.Errorf("decode deployment spec: %w", err)
	}

	return spec, nil
}

// applyDeploymentSpecDefaults fills in everything the reconciler's
// apply() path expects to be normalized before it builds a
// ContainerSpec — image fallback, voodu0 auto-join, port
// localhost-binding, restart policy. The release/restart imperative
// entry points decode the raw spec from the store and need the same
// shape, so this lives once and the three call sites
// (apply/Release/Restart) share the result.
//
// Returns an error only on validation failures (mutual exclusivity
// of network_mode vs networks). Defaults themselves never fail.
//
// The fields covered:
//
//   - Image: empty → "<app>:latest" (build-mode convention)
//   - NetworkMode/Networks: validate exclusivity; bridge mode adds
//     voodu0 if missing
//   - Ports: normalize to loopback-only unless operator pinned an IP
//   - Restart: empty → "unless-stopped"
//
// Without this consolidation, build-mode + Release() would trip
// "invalid reference format" (empty image) and bridge-mode +
// Release()/Restart() would trip "network-scoped aliases are only
// supported for user-defined networks" (no voodu0 in spec.Networks
// → docker tries to apply --network-alias on the default bridge).
func applyDeploymentSpecDefaults(spec *deploymentSpec, app string) error {
	if spec.Image == "" {
		spec.Image = app + ":latest"
	}

	switch spec.NetworkMode {
	case "":
		// Bridge mode — voodu0 auto-join below.
	case "host", "none":
		if len(spec.Networks) > 0 || spec.Network != "" {
			return fmt.Errorf("deployment/%s: network_mode=%q is mutually exclusive with network/networks", app, spec.NetworkMode)
		}
	default:
		return fmt.Errorf("deployment/%s: network_mode=%q not supported (want \"host\" or \"none\"; omit for bridge mode)", app, spec.NetworkMode)
	}

	if spec.NetworkMode == "" {
		if len(spec.Networks) == 0 && spec.Network != "" {
			spec.Networks = []string{spec.Network}
		}

		if !slices.Contains(spec.Networks, "voodu0") {
			spec.Networks = append(spec.Networks, "voodu0")
		}
	}

	spec.Ports = normalizePorts(spec.Ports)

	if spec.Restart == "" {
		spec.Restart = "unless-stopped"
	}

	return nil
}

// DeploymentStatus is persisted at /status/deployments/<name> after
// every successful Ensure or Recreate. SpecHash is the sha256 of the
// runtime-relevant fields of the spec (see deploymentSpecHash) and is
// how the handler detects drift across reconciles.
//
// Env is deliberately excluded from the hash: env changes are handled
// by Restart (env file is mounted at runtime, no recreate needed), so
// hashing it would cause unnecessary Recreate churn on every config set.
type DeploymentStatus struct {
	Image    string `json:"image,omitempty"`
	SpecHash string `json:"spec_hash,omitempty"`

	// Releases is the deployment's release-phase history, newest
	// first, capped at maxReleaseHistory entries. Each record carries
	// the spec snapshot used at release time, which is what
	// `vd release rollback` re-applies to revert.
	Releases []ReleaseRecord `json:"releases,omitempty"`
}

// ReleaseRecord is one entry in the deployment's release history.
// Mirrors JobRun's spirit but specialised for the release-phase
// flow: tracks spec snapshot for rollback, separate exit codes for
// pre/main/post, and a single Status that summarises the whole run.
//
// IDs are short sortable hashes generated independently per
// release — no read-modify-write race like a monotonic counter
// would have. Lexical sort matches creation order, so listing the
// history in reverse (newest first) is a single sort.Sort.
//
// Heroku-style "v1, v2, v3" was tempting for UX but every read of
// the current max would require either a global lock or risk of
// collision under concurrent applies. The 9-char hash sidesteps
// that entirely; operators copy IDs from `vd release <ref>` output
// when they need to rollback.
type ReleaseRecord struct {
	// ID is the sortable 9-char hash unique to this release. The
	// only operator-facing identifier; what `vd rollback web <id>`
	// expects. Format: base36(unix_seconds) + 2 hex random chars,
	// e.g. "1ksdtcj7e". Sortable lexicographically by creation time.
	ID string `json:"id"`

	// RolledBackFrom is the ID this record was created by rolling
	// back TO. Empty for normal releases. Lets `vd release <ref>`
	// render "this release was a rollback to <id>" so the timeline
	// stays auditable.
	RolledBackFrom string `json:"rolled_back_from,omitempty"`

	// SpecHash is the deploymentSpecHash() of the spec at release
	// time. The idempotency check before running uses this: a spec
	// already-Succeeded skips the run on rollback / re-apply.
	SpecHash string `json:"spec_hash"`

	// Image is the image rolled out by this release. Surfaced in
	// the history table so the operator can correlate "release X
	// took 30s" with "image vd-web:1.2.3".
	Image string `json:"image,omitempty"`

	Status    ReleaseStatus `json:"status"`
	StartedAt time.Time     `json:"started_at,omitempty"`
	EndedAt   time.Time     `json:"ended_at,omitempty"`

	// Per-step exit codes. Zero means "step succeeded or didn't run";
	// PreExitCode for pre_command, ExitCode for the main command,
	// PostExitCode for post_command. Step that aborted the run
	// matches Status (e.g. Status=failed + ExitCode=42 means main
	// command exited 42).
	PreExitCode  int `json:"pre_exit_code,omitempty"`
	ExitCode     int `json:"exit_code,omitempty"`
	PostExitCode int `json:"post_exit_code,omitempty"`

	// Error carries the first error that aborted the run — typically
	// the docker-side message ("container exited 1: <stderr>") or a
	// timeout signal. Empty for successful releases.
	Error string `json:"error,omitempty"`

	// SpecSnapshot is the full Manifest.Spec JSON at release time.
	// `vd release rollback` re-applies this snapshot to revert. Kept
	// alongside SpecHash because "the spec that ran X" needs to be
	// reconstructible without consulting git history.
	SpecSnapshot json.RawMessage `json:"spec_snapshot,omitempty"`
}

// ReleaseStatus is the lifecycle of a single ReleaseRecord. Same
// shape as JobStatus but the values are release-specific (succeeded
// includes both rolling restart + post hook completion; failed
// covers any step before that).
type ReleaseStatus string

const (
	ReleaseStatusRunning   ReleaseStatus = "running"
	ReleaseStatusSucceeded ReleaseStatus = "succeeded"
	ReleaseStatusFailed    ReleaseStatus = "failed"
)

// maxReleaseHistory caps the number of records we persist per
// deployment. 10 is enough for "last few rollbacks" without
// bloating the status blob — the file lives in etcd and we read it
// every reconcile. New releases prepend; older ones drop off.
const maxReleaseHistory = 10

// defaultReleaseTimeout caps each release-phase command (pre, main,
// post) at 10 minutes when the manifest doesn't say otherwise.
// Generous enough for slow migrations on big tables, short enough
// that a stuck command doesn't pin the rollout forever.
const defaultReleaseTimeout = 10 * time.Minute

// deploymentSpecHash canonicalises the runtime-shaping fields and
// hashes them. The hash input is a struct (not the raw deploymentSpec)
// so adding new irrelevant fields to deploymentSpec doesn't silently
// change the hash and trigger spurious recreates.
func deploymentSpecHash(spec deploymentSpec) string {
	// Networks membership is what the runtime sees — order inside
	// `networks = [...]` is not semantic (docker's join order doesn't
	// affect reachability), so we sort a copy before hashing to avoid
	// spurious Recreates when the operator reshuffles the list.
	nets := append([]string(nil), spec.Networks...)
	sort.Strings(nets)

	input := struct {
		Image       string   `json:"image"`
		Command     []string `json:"command"`
		Ports       []string `json:"ports"`
		Volumes     []string `json:"volumes"`
		Networks    []string `json:"networks"`
		NetworkMode string   `json:"network_mode"`
		Restart     string   `json:"restart"`
	}{
		Image:       spec.Image,
		Command:     spec.Command,
		Ports:       spec.Ports,
		Volumes:     spec.Volumes,
		Networks:    nets,
		NetworkMode: spec.NetworkMode,
		Restart:     spec.Restart,
	}

	// json.Marshal emits slice elements in declared order and struct
	// fields in declaration order, so the output is deterministic for
	// a given spec. Ports/Volumes stay in user order — semantic there
	// (first port entry often maps to the ingress default).
	b, _ := json.Marshal(input)
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

func shortHash(h string) string {
	if len(h) <= 8 {
		return h
	}

	return h[:8]
}

func (h *DeploymentHandler) putDeploymentStatus(ctx context.Context, app string, spec deploymentSpec) error {
	return h.writeDeploymentStatus(ctx, app, spec.Image, deploymentSpecHash(spec))
}

func (h *DeploymentHandler) writeDeploymentStatus(ctx context.Context, app, image, hash string) error {
	blob, err := json.Marshal(DeploymentStatus{Image: image, SpecHash: hash})
	if err != nil {
		return err
	}

	return h.Store.PutStatus(ctx, KindDeployment, app, blob)
}

// normalizePorts applies voodu's "private by default" posture to
// docker port specs. Input follows docker's shape — one of:
//
//	"80"                    bare container port
//	"80/udp"                bare, non-default proto
//	"3000:80"               host:container
//	"3000:80/udp"           host:container with proto
//	"127.0.0.1:3000:80"     ip:host:container (already explicit)
//	"0.0.0.0:3000:80"       bind-all, operator opted into exposure
//	"[::1]:3000:80"         IPv6 literal (pass-through)
//
// When the spec does NOT carry an IP prefix we wedge `127.0.0.1:` in
// front so the publish only reaches the host's loopback. Specs that
// already name an IP (including `0.0.0.0`) are the operator's
// declaration of intent — we pass them through untouched. Docker
// itself then does the right thing: `-p 127.0.0.1::80` maps a random
// host port on loopback, `-p 0.0.0.0:5432:5432` exposes publicly.
//
// This is the only gate between "deploy with ports = [...]" and "your
// service is on the open internet". Caddy ingress is unaffected:
// caddy dials by container name over the voodu0 bridge, which
// bypasses host-side port publishing entirely.
func normalizePorts(ports []string) []string {
	if len(ports) == 0 {
		return ports
	}

	out := make([]string, 0, len(ports))

	for _, p := range ports {
		out = append(out, normalizePort(p))
	}

	return out
}

func normalizePort(p string) string {
	// IPv6 literal wrapped in brackets — docker's own syntax, pass through.
	if strings.HasPrefix(p, "[") {
		return p
	}

	switch strings.Count(p, ":") {
	case 0:
		// "80" or "80/udp" — random host port, loopback-only.
		return "127.0.0.1::" + p
	case 1:
		// "3000:80" or "3000:80/udp" — explicit host port, loopback-only.
		return "127.0.0.1:" + p
	default:
		// 2+ colons means the first field is an IP (0.0.0.0, 127.0.0.1,
		// a pinned interface IP). Operator was explicit — respect it.
		return p
	}
}

// envMapToPairs flattens {K: V} into ["K=V", ...] with deterministic
// ordering — helps tests and keeps the resulting .env file stable
// across reconciles.
func envMapToPairs(env map[string]string) []string {
	keys := make([]string, 0, len(env))

	for k := range env {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+env[k])
	}

	return pairs
}

func (h *DeploymentHandler) logf(format string, args ...any) {
	if h.Log == nil {
		return
	}

	h.Log.Printf(format, args...)
}
