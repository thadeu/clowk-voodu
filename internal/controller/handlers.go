package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"

	"go.voodu.clowk.in/pkg/plugin"
)

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
	Image   string            `json:"image,omitempty"`
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Ports   []string          `json:"ports,omitempty"`
	Volumes []string          `json:"volumes,omitempty"`
	Network string            `json:"network,omitempty"`
	Restart string            `json:"restart,omitempty"`
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
		return fmt.Errorf("%s create exited %d: %s", spec.Engine, res.ExitCode, string(res.Stderr))
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
		return fmt.Errorf("%s destroy exited %d: %s", status.Engine, res.ExitCode, string(res.Stderr))
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
// When spec.Image is empty, the handler assumes git-push owns the
// container lifecycle (the legacy Gokku-style path) and only touches
// env + restart. That split lets manifest-driven apps coexist with
// code-pushed apps on the same host.
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
}

func (h *DeploymentHandler) Handle(ctx context.Context, ev WatchEvent) error {
	switch ev.Type {
	case WatchPut:
		return h.apply(ctx, ev)

	case WatchDelete:
		// Don't nuke the app's env on deployment delete — the user may
		// have secrets set through `voodu config set` that survive
		// deployment churn. Explicit `voodu config unset` or removing
		// the app is the right escape hatch.
		h.logf("deployment/%s deleted (env left intact)", ev.Name)
		return nil
	}

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

	envChanged := false

	if len(spec.Env) > 0 {
		changed, err := h.linkEnv(ctx, ev.Name, spec.Env)
		if err != nil {
			return err
		}

		envChanged = changed
	} else {
		h.logf("deployment/%s no env to link", ev.Name)
	}

	created, err := h.ensureContainer(ev.Name, spec)
	if err != nil {
		return err
	}

	if h.Containers == nil {
		return nil
	}

	// Image drift trumps restart: if the manifest points at a different
	// image than what's running, a plain restart wouldn't pick it up.
	// Recreate also absorbs any env change — no need for a separate
	// restart after.
	if !created && spec.Image != "" {
		recreated, err := h.recreateIfImageChanged(ev.Name, spec)
		if err != nil {
			return err
		}

		if recreated {
			return nil
		}
	}

	// Fresh containers come up with the current .env already mounted,
	// so restarting right after Ensure is redundant churn. Only cycle
	// when env moved and the container was already there.
	if envChanged && !created {
		if err := h.Containers.Restart(ev.Name); err != nil {
			// Restart failure doesn't unwind the reconcile — env is on
			// disk, the next deploy or manual restart picks it up.
			h.logf("deployment/%s restart failed (env already written): %v", ev.Name, err)
		} else {
			h.logf("deployment/%s restarted (env changed)", ev.Name)
		}
	}

	return nil
}

// recreateIfImageChanged inspects the running container and recreates
// it when the desired Image differs. Returns whether a recreate
// happened so the caller can skip the env-only restart path.
func (h *DeploymentHandler) recreateIfImageChanged(app string, spec deploymentSpec) (bool, error) {
	current, err := h.Containers.Image(app)
	if err != nil {
		// Image lookup failures are surprising but non-fatal: leave the
		// container alone, surface the error to the operator via logs.
		h.logf("deployment/%s image lookup failed: %v", app, err)
		return false, nil
	}

	if current == "" || current == spec.Image {
		return false, nil
	}

	h.logf("deployment/%s image drift: %s → %s, recreating", app, current, spec.Image)

	envFile := ""
	if h.EnvFilePath != nil {
		envFile = h.EnvFilePath(app)
	}

	if err := h.Containers.Recreate(ContainerSpec{
		Name:    app,
		Image:   spec.Image,
		Command: spec.Command,
		Ports:   spec.Ports,
		Volumes: spec.Volumes,
		Network: spec.Network,
		Restart: spec.Restart,
		EnvFile: envFile,
	}); err != nil {
		return false, fmt.Errorf("recreate container: %w", err)
	}

	return true, nil
}

// linkEnv resolves refs and writes the result to the app's env file.
// Split out of apply() so the ensure-container step can run independently
// when the manifest has no env block. Returns whether the write actually
// changed any value — used to gate restart.
func (h *DeploymentHandler) linkEnv(ctx context.Context, app string, env map[string]string) (bool, error) {
	lookup := h.refLookup(ctx)

	resolved, err := InterpolateRefsMap(env, lookup)
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

// ensureContainer delegates to the container manager when the manifest
// opts into reconciler-owned spawn (Image set) and a manager is wired.
// Returns whether a container was created this call — false means the
// container already existed (so restart logic can kick in) or the
// manifest is env-only.
func (h *DeploymentHandler) ensureContainer(app string, spec deploymentSpec) (bool, error) {
	if spec.Image == "" || h.Containers == nil {
		return false, nil
	}

	envFile := ""
	if h.EnvFilePath != nil {
		envFile = h.EnvFilePath(app)
	}

	created, err := h.Containers.Ensure(ContainerSpec{
		Name:    app,
		Image:   spec.Image,
		Command: spec.Command,
		Ports:   spec.Ports,
		Volumes: spec.Volumes,
		Network: spec.Network,
		Restart: spec.Restart,
		EnvFile: envFile,
	})
	if err != nil {
		return false, fmt.Errorf("ensure container: %w", err)
	}

	if created {
		h.logf("deployment/%s container created (image=%s)", app, spec.Image)
	}

	return created, nil
}

// refLookup closes over the store so InterpolateRefsMap can resolve
// ${ref.<kind>.<name>.<field>} by reading /status/<kind>s/<name>.
//
// The status shape is deliberately uniform: every kind persists a blob
// that has a top-level `data` object. That way this closure doesn't
// need to switch on kind — any future kind (service, ingress, queue)
// becomes referenceable just by following the convention.
func (h *DeploymentHandler) refLookup(ctx context.Context) RefLookup {
	return func(kind, name, field string) (string, bool) {
		k, err := ParseKind(kind)
		if err != nil {
			return "", false
		}

		raw, err := h.Store.GetStatus(ctx, k, name)
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
