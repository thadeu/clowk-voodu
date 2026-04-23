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

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

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
	Command     []string          `json:"command,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Ports       []string          `json:"ports,omitempty"`
	Volumes     []string          `json:"volumes,omitempty"`
	Network     string            `json:"network,omitempty"`
	Networks    []string          `json:"networks,omitempty"`
	NetworkMode string            `json:"network_mode,omitempty"`
	Restart     string            `json:"restart,omitempty"`
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
//   - spec.Image == "" (build-mode): the image comes from a git push.
//     `voodu apply` fires the push automatically before POSTing the
//     manifests, and the bare repo's post-receive hook runs the legacy
//     `voodu deploy` pipeline (extract → build → start container).
//     The handler here still runs linkEnv so ${ref.*} resolves to the
//     on-disk .env file the hook's deploy will mount, but it leaves
//     container orchestration to that path. A future refactor can
//     collapse both modes under the handler once the hook is demoted
//     to a pure build step.
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

	// Build-mode (no image, source pushed via git) produces an image
	// tagged <app>:latest. The controller never sees the build itself —
	// the post-receive hook runs it — so we resolve the resulting image
	// here by convention. Without this default, ensureContainer's
	// `Image == ""` early return swallows every build-mode reconcile.
	if spec.Image == "" {
		spec.Image = ev.Name + ":latest"
	}

	// Network shaping. Two disjoint modes:
	//
	//   host/none mode: container shares the host's net stack (host) or
	//     gets none at all. Mutually exclusive with any bridge — no
	//     voodu0, no custom networks. Use for WebRTC/SIP/RTP/socket apps
	//     that need the host ports directly and can't live behind docker's
	//     userland NAT. Caveat: caddy ingress can't reach these by
	//     container name — the operator wires the host port into
	//     ingress.service manually.
	//
	//   bridge mode (default): voodu0 is the platform's plumbing bus —
	//     voodu-caddy and managed plugins live there — so every bridge-
	//     mode container MUST join it. `networks = [...]` opts INTO
	//     additional bridges (db, cache, whatever) but never removes
	//     voodu0. Append (not prepend) when auto-adding voodu0 so the
	//     operator's chosen primary stays the docker DNS default.
	switch spec.NetworkMode {
	case "":
		// Bridge path — fall through to networks normalization below.
	case "host", "none":
		if len(spec.Networks) > 0 || spec.Network != "" {
			return fmt.Errorf("deployment/%s: network_mode=%q is mutually exclusive with network/networks", ev.Name, spec.NetworkMode)
		}
	default:
		return fmt.Errorf("deployment/%s: network_mode=%q not supported (want \"host\" or \"none\"; omit for bridge mode)", ev.Name, spec.NetworkMode)
	}

	if spec.NetworkMode == "" {
		if len(spec.Networks) == 0 && spec.Network != "" {
			spec.Networks = []string{spec.Network}
		}

		if !slices.Contains(spec.Networks, "voodu0") {
			spec.Networks = append(spec.Networks, "voodu0")
		}
	}

	// Private-by-default port publishing. Bare container ports and
	// host:container specs get localhost-bound so they're reachable
	// from the VM (ssh tunnel, curl from inside) but invisible to the
	// internet. Caddy still reaches the container via voodu0 bridge
	// DNS — `-p` is only for host-to-container access, so this doesn't
	// affect ingress. Operators who actually want world-exposure
	// (postgres pinned to 0.0.0.0:5432, say) declare the IP explicitly.
	spec.Ports = normalizePorts(spec.Ports)

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

	if created {
		// Baseline the spec hash so the next reconcile has something to
		// compare against. Without this, the very next apply would see
		// no persisted status and treat a real drift as first-seen.
		if err := h.putDeploymentStatus(ctx, ev.Name, spec); err != nil {
			h.logf("deployment/%s status persist failed: %v", ev.Name, err)
		}

		return nil
	}

	// Spec drift trumps restart: any change in runtime-relevant fields
	// (image, ports, volumes, network, restart policy, command) means
	// the running container has the wrong shape — Recreate absorbs env
	// changes, so no follow-up restart either.
	if spec.Image != "" {
		recreated, err := h.recreateIfSpecChanged(ctx, ev.Name, spec)
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
	if envChanged {
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

// recreateIfSpecChanged compares the sha256 of the runtime-relevant
// fields of the desired spec against the hash persisted at last
// reconcile. A mismatch triggers Recreate and a fresh status write.
//
// When no status exists yet (first reconcile after a controller
// upgrade that introduced status persistence) we baseline the hash
// without recreating: the running container may well match the spec,
// and churning every pre-existing deploy on upgrade is exactly the
// kind of surprise this handler is meant to avoid.
func (h *DeploymentHandler) recreateIfSpecChanged(ctx context.Context, app string, spec deploymentSpec) (bool, error) {
	hash := deploymentSpecHash(spec)

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

	if prev.SpecHash == hash {
		// Spec text is stable, but build-mode rebuilds `<app>:latest` on
		// every git push — the tag string is identical yet the underlying
		// image ID changes. Containers freeze the image ID at create
		// time, so the running process stays on the old layers until we
		// explicitly recreate. Spec-hash can't catch this (manifest text
		// didn't move); only an image-ID comparison can.
		differ, err := h.Containers.ImageIDsDiffer(app, spec.Image)
		if err != nil {
			// Treat ID-check errors as "no drift" to avoid unnecessary
			// recreates on transient docker CLI failures. The next apply
			// will try again.
			h.logf("deployment/%s image id check failed: %v", app, err)
			return false, nil
		}

		if !differ {
			return false, nil
		}

		h.logf("deployment/%s image id drift (tag %s rebuilt under same name), recreating", app, spec.Image)
	} else {
		h.logf("deployment/%s spec drift (hash %s → %s), recreating", app, shortHash(prev.SpecHash), shortHash(hash))
	}

	envFile := ""
	if h.EnvFilePath != nil {
		envFile = h.EnvFilePath(app)
	}

	if err := h.Containers.Recreate(ContainerSpec{
		Name:        app,
		Image:       spec.Image,
		Command:     spec.Command,
		Ports:       spec.Ports,
		Volumes:     spec.Volumes,
		Networks:    spec.Networks,
		NetworkMode: spec.NetworkMode,
		Restart:     spec.Restart,
		EnvFile:     envFile,
	}); err != nil {
		return false, fmt.Errorf("recreate container: %w", err)
	}

	if err := h.writeDeploymentStatus(ctx, app, spec.Image, hash); err != nil {
		h.logf("deployment/%s status persist failed: %v", app, err)
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
		Name:        app,
		Image:       spec.Image,
		Command:     spec.Command,
		Ports:       spec.Ports,
		Volumes:     spec.Volumes,
		Networks:    spec.Networks,
		NetworkMode: spec.NetworkMode,
		Restart:     spec.Restart,
		EnvFile:     envFile,
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
}

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
