package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// pluginDispatchRequest is the wire payload for the
// `POST /plugin/{name}/{command}` endpoint. The shape is
// deliberately minimal: the CLI doesn't know what the plugin
// does, doesn't know how many refs the command takes, doesn't
// pre-fetch any state. It just hands the args slice to the
// server, which hands it to the plugin verbatim.
//
// This is the passthrough contract: CLI is dumb, server is
// dumb, plugin is autonomous. New plugin commands require zero
// CLI/server changes — author drops a binary in bin/ and the
// command is reachable via `vd <plugin>:<command> [args...]`.
type pluginDispatchRequest struct {
	Args []string `json:"args,omitempty"`
}

// pluginDispatchAction is one instruction the plugin asks the
// controller to apply on its behalf. Plugins stay transformative
// (input → output instructions) and never touch the store
// directly — every write is mediated by the controller, which
// validates the action type and applies via existing primitives.
//
// Recognised types:
//
//   - config_set       — write KV pairs to the (Scope, Name) bucket
//   - config_unset     — remove Keys from the (Scope, Name) bucket
//   - apply_manifest   — upsert a manifest via Store.Put. The
//                        embedded Manifest carries kind+scope+
//                        name+spec; action's top-level Scope/Name
//                        are the "owner context" (typically the
//                        plugin resource emitting the apply).
//   - delete_manifest  — Store.Delete(Kind, Scope, Name). The
//                        action's top-level Scope/Name + Kind
//                        identify the manifest to remove.
//   - run_job          — async Jobs.RunOnce(Scope, Name) on an
//                        already-applied job manifest. Spawns a
//                        goroutine and returns immediately with
//                        a "queued" summary. Errors during the
//                        run surface via /describe?kind=job and
//                        the per-job status, never to this
//                        dispatch response (fire-and-forget by
//                        design — plugin already exited when the
//                        run starts).
//   - exec_local       — passthrough: controller does NOT run
//                        the command server-side; it adds the
//                        command vector to a separate
//                        `exec_local` field in the response so
//                        the CLI can run it locally with TTY
//                        attached. Use for interactive shells
//                        and other TTY-dependent flows.
//
// Unknown action types are rejected (loud failure beats silent
// no-op when a plugin author typos a type).
//
// Why this exists at all: plugins are isolated processes; they
// must not touch etcd directly. Funnelling every write through
// dispatch keeps validation, audit, and restart fan-out
// centralised. The same shape will back the SDK + future Web UI
// — every "thing a plugin can do" is an action type the
// controller recognises.
type pluginDispatchAction struct {
	Type string `json:"type"`

	// Scope/Name describe the owner context for the action. For
	// config_*, they identify the bucket to write. For
	// delete_manifest, they identify the manifest to remove
	// (paired with Kind below). For apply_manifest, they're the
	// plugin's own (scope, name) — used for restart fan-out and
	// audit; the actual manifest applied is described by the
	// Manifest field.
	Scope string `json:"scope"`
	Name  string `json:"name"`

	// config_set payload.
	KV map[string]string `json:"kv,omitempty"`

	// config_unset payload.
	Keys []string `json:"keys,omitempty"`

	// SkipRestart, when true, applies the store mutation but
	// suppresses the usual restart fan-out on (Scope, Name).
	// Per-action grain because some envelopes mix
	// "restart-needed" and "store-only" writes — notably the
	// sentinel auto-failover callback in voodu-redis: the
	// data-redis action skips restart (sentinel already moved
	// roles), while consumer URL refreshes in the same envelope
	// fire restarts so apps pick up the new URL.
	//
	// Default false (omitted in JSON) preserves the historical
	// "every config_set rolls affected workloads" behaviour for
	// every existing plugin — opt-in only. Ignored for
	// apply_manifest / delete_manifest (they don't fan out
	// restarts on (action.Scope, action.Name) — the manifest
	// being applied/removed has its own reconcile path through
	// the watch loop).
	SkipRestart bool `json:"skip_restart,omitempty"`

	// apply_manifest payload — the full manifest to upsert.
	Manifest *pluginDispatchManifest `json:"manifest,omitempty"`

	// delete_manifest payload — Kind of manifest at
	// (Scope, Name) to remove. (Scope/Name come from the
	// top-level fields above so we don't duplicate them.)
	Kind string `json:"kind,omitempty"`

	// exec_local payload — a command vector the CLI runs LOCALLY
	// on the operator's host with the operator's TTY attached.
	// The controller does NOT execute this server-side; it
	// passes the command through in the dispatch response so
	// the CLI can invoke it.
	//
	// Use case: interactive shells (`vd pg:psql`, `vd redis:cli`),
	// any command that needs a TTY. Plugin-side syscall.Exec
	// cannot propagate TTY through HTTP dispatch — this gives
	// the plugin a way to delegate the local exec back to the
	// CLI while keeping authority over the command's arguments
	// (resolves container name, user, db, etc.).
	//
	// Security note: any plugin emitting exec_local can run
	// arbitrary commands on the operator's host. Plugins are
	// trusted by install model (operator chose to install them),
	// so this is consistent with the existing trust boundary.
	// The CLI logs the command before executing for audit.
	Command []string `json:"command,omitempty"`
}

// pluginDispatchExecLocal is the wire shape the controller adds
// to the dispatch response for every exec_local action it
// encountered. CLI iterates this list after the response arrives
// and runs each command with the operator's TTY attached. Order
// preserved from the plugin's emit order.
type pluginDispatchExecLocal struct {
	Command []string `json:"command"`
}

// pluginDispatchManifest is the wire shape the plugin emits when
// asking the controller to apply (upsert) a manifest. Mirrors the
// store's Manifest{} shape minus the metadata fields the
// controller fills in itself.
//
// Spec is decoded as a generic map and re-marshalled before going
// into Store.Put — same pipeline expand uses for plugin-emitted
// macro outputs, so all the validation and asset-stamping that
// applies to vd apply also applies to a plugin-dispatched apply.
type pluginDispatchManifest struct {
	Kind  string         `json:"kind"`
	Scope string         `json:"scope,omitempty"`
	Name  string         `json:"name"`
	Spec  map[string]any `json:"spec"`
}

// pluginDispatchResponse is the operator-facing data the plugin
// emits inside its envelope. `message` is the one-line success
// summary the CLI prints; `actions` is the queue the controller
// applies post-invoke.
type pluginDispatchResponse struct {
	Message string                 `json:"message,omitempty"`
	Actions []pluginDispatchAction `json:"actions,omitempty"`
}

// pluginInvocationContext is the JSON envelope the controller
// writes to the plugin's stdin. The plugin reads this to learn
// where to call back (controller_url) and to access its own
// installation directory (plugin_dir, useful for reading
// bundled assets like get-conf scripts).
//
// Plugins parse args via os.Args[2:] (standard CLI argv) — the
// stdin envelope is purely for context the OS argv can't carry.
type pluginInvocationContext struct {
	Plugin        string `json:"plugin"`
	Command       string `json:"command"`
	ControllerURL string `json:"controller_url,omitempty"`
	PluginDir     string `json:"plugin_dir,omitempty"`
	NodeName      string `json:"node_name,omitempty"`
}

// handlePluginCommand dispatches `POST /plugin/{name}/{command}`.
//
// Lifecycle of one call:
//
//  1. Parse {name, command} from URL, decode `{args}` from body.
//  2. Load the plugin from PluginsRoot. If bin/<command> doesn't
//     exist, 400 — every subcommand needs a discoverable
//     executable file (the plugin loader indexes by filename).
//  3. Run bin/<command> with the operator's args verbatim. Stdin
//     carries the invocation context (controller_url, plugin_dir)
//     so the plugin can call back into /describe, /config, etc.
//     when it needs platform state.
//  4. Parse the plugin's envelope. envelope.Data may include a
//     `message` and an `actions` list. Each action is applied
//     against the store (config_set, config_unset).
//  5. Return 200 with the plugin's `message` and a list of
//     actions actually applied.
//
// The handler doesn't pre-fetch any manifest/config — the plugin
// is autonomous. If it needs the redis manifest's spec, it does
// `GET ${controller_url}/describe?kind=statefulset&...` itself.
// Same posture as kubectl plugins: thin shell, plugin owns its
// world.
//
// Failure modes the operator sees:
//
//   - 400 plugin not installed
//   - 400 bin/<command> doesn't exist
//   - 400 plugin exited non-zero (with stderr in the error)
//   - 400 plugin returned an error envelope
//   - 400 plugin emitted a malformed action (unknown type)
//   - 500 store write failed mid-action-apply
func (a *API) handlePluginCommand(w http.ResponseWriter, r *http.Request) {
	pluginName := strings.TrimSpace(r.PathValue("name"))
	command := strings.TrimSpace(r.PathValue("command"))

	if pluginName == "" || command == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("URL must be /plugin/{name}/{command}"))
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("read body: %w", err))
		return
	}

	var req pluginDispatchRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
			return
		}
	}

	if a.PluginsRoot == "" {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("controller has no plugins root configured"))
		return
	}

	// LoadByName honours plugin.yml `aliases:` — `vd pg:psql`
	// resolves to the postgres plugin (which declares
	// aliases: [pg]) without the operator having to know
	// the canonical name.
	plug, err := plugins.LoadByName(a.PluginsRoot, pluginName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("plugin %q: %w", pluginName, err))
		return
	}

	if _, declared := plug.Commands[command]; !declared {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"plugin %q has no command %q (available: %s)",
			pluginName, command, listAvailableCommands(plug)))
		return
	}

	ctx := pluginInvocationContext{
		Plugin:        pluginName,
		Command:       command,
		ControllerURL: a.ControllerURL,
		PluginDir:     plug.Dir,
		NodeName:      a.NodeName,
	}

	stdin, err := json.Marshal(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("marshal plugin stdin: %w", err))
		return
	}

	res, err := plug.Run(r.Context(), plugins.RunOptions{
		Command: command,
		Args:    req.Args,
		Stdin:   stdin,
		Env: map[string]string{
			plugin.EnvRoot:       a.PluginsRoot,
			plugin.EnvNode:       a.NodeName,
			plugin.EnvEtcdClient: a.EtcdClient,
		},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("invoke plugin: %w", err))
		return
	}

	if res.ExitCode != 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("plugin %q %s exited %d: %s",
			pluginName, command, res.ExitCode, pluginErrorDetail(res)))
		return
	}

	if res.Envelope == nil {
		// Plugin emitted plain text (no JSON envelope) — pass it
		// through as the message. Lets simple shell plugins
		// participate in dispatch without speaking the
		// envelope protocol.
		writeJSON(w, http.StatusOK, envelope{
			Status: "ok",
			Data: map[string]any{
				"message": strings.TrimRight(string(res.Raw), "\n"),
				"applied": []string{},
			},
		})

		return
	}

	if res.Envelope.Status == "error" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("plugin %q %s: %s",
			pluginName, command, res.Envelope.Error))
		return
	}

	var resp pluginDispatchResponse

	if res.Envelope.Data != nil {
		raw, err := json.Marshal(res.Envelope.Data)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("re-marshal plugin data: %w", err))
			return
		}

		if err := json.Unmarshal(raw, &resp); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("decode plugin response: %w", err))
			return
		}
	}

	applied := make([]string, 0, len(resp.Actions))
	execLocals := make([]pluginDispatchExecLocal, 0)

	for i, action := range resp.Actions {
		// exec_local is a passthrough — controller doesn't run
		// it server-side. We collect it for the CLI to execute
		// locally with the operator's TTY. See
		// pluginDispatchAction.Command for the rationale.
		if action.Type == "exec_local" {
			if len(action.Command) == 0 {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("apply action %d (exec_local): empty command", i))
				return
			}

			execLocals = append(execLocals, pluginDispatchExecLocal{Command: action.Command})
			applied = append(applied, fmt.Sprintf("exec_local %v (deferred to CLI)", action.Command))

			continue
		}

		summary, err := a.applyPluginDispatchActionFromRequest(r, action)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("apply action %d (%s): %w", i, action.Type, err))
			return
		}

		applied = append(applied, summary)
	}

	data := map[string]any{
		"message": resp.Message,
		"applied": applied,
	}

	// Only include exec_local in the response when there's at
	// least one — keeps the JSON tidy for the 99% case where no
	// local exec is needed.
	if len(execLocals) > 0 {
		data["exec_local"] = execLocals
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   data,
	})
}

// applyPluginDispatchActionFromRequest is the HTTP-handler path
// — same logic as ApplyPluginDispatchAction but it has access
// to *http.Request so it can honour `?restart=false` semantics
// via maybeRestartAffected. Used by handlePluginCommand.
func (a *API) applyPluginDispatchActionFromRequest(r *http.Request, action pluginDispatchAction) (string, error) {
	summary, err := a.applyPluginDispatchAction(r.Context(), action)
	if err != nil {
		return "", err
	}

	// SkipRestart=true short-circuits the fan-out for THIS action
	// only. The use case is sentinel auto-failover: the data-redis
	// action records the new master ordinal in the store but
	// skips the rolling restart (sentinel has already moved roles
	// inside Redis; rolling pods would drop active connections
	// AND risk a ping-pong with sentinel re-electing during the
	// reboot window). Other actions in the same envelope (consumer
	// URL refreshes) keep SkipRestart=false so apps still restart
	// to pick up the new URL.
	if action.SkipRestart {
		return summary, nil
	}

	// apply_manifest / delete_manifest / run_job have their OWN
	// reconcile path through the watch loop or the JobRunner —
	// restarting (action.Scope, action.Name) here would roll the
	// owner resource (e.g. the postgres a backup job belongs to)
	// on every plugin-emitted action, which is wrong. The watched
	// kind's handler picks up WatchPut/WatchDelete; run_job's
	// goroutine drives container lifecycle on its own.
	if action.Type == "apply_manifest" || action.Type == "delete_manifest" || action.Type == "run_job" {
		return summary, nil
	}

	// Trigger the same restart fan-out `vd config set` does so
	// consumer pods pick up the new env file without a manual
	// nudge. Always-on for plugin dispatch unless the action
	// explicitly opted out: the operator invoking `vd <plugin>:link`
	// expects the link to be live.
	a.maybeRestartAffected(r, action.Scope, action.Name)

	return summary, nil
}

// applyPluginDispatchAction translates one plugin-emitted action
// into a store mutation. Mirrors `vd config set` / `vd config
// unset` semantics so a plugin's link/expand actions produce
// the same observable outcome as the operator running those
// CLI verbs by hand.
//
// Free-function shape (takes ctx + store via the receiver) so
// non-HTTP callers — notably runExpand, where actions can flow
// out of expand alongside manifests — can apply actions without
// fabricating a request. Restart fan-out is the caller's
// responsibility (it differs by call site: HTTP path honours
// ?restart=false, expand path always-on).
func (a *API) applyPluginDispatchAction(ctx context.Context, action pluginDispatchAction) (string, error) {
	if action.Scope == "" || action.Name == "" {
		return "", fmt.Errorf("scope and name are required for action %q", action.Type)
	}

	switch action.Type {
	case "config_set":
		if len(action.KV) == 0 {
			return "", fmt.Errorf("config_set requires non-empty kv")
		}

		if err := a.Store.PatchConfig(ctx, action.Scope, action.Name, action.KV); err != nil {
			return "", err
		}

		keys := keysSorted(action.KV)

		return fmt.Sprintf("config_set %s/%s: %s", action.Scope, action.Name, strings.Join(keys, ",")), nil

	case "config_unset":
		if len(action.Keys) == 0 {
			return "", fmt.Errorf("config_unset requires non-empty keys")
		}

		// PatchConfig with empty value unsets the key — see
		// memstore_test.go's PatchConfig implementation and
		// the etcd counterpart in store.go.
		unsets := make(map[string]string, len(action.Keys))
		for _, k := range action.Keys {
			unsets[k] = ""
		}

		if err := a.Store.PatchConfig(ctx, action.Scope, action.Name, unsets); err != nil {
			return "", err
		}

		sorted := append([]string(nil), action.Keys...)
		sort.Strings(sorted)

		return fmt.Sprintf("config_unset %s/%s: %s", action.Scope, action.Name, strings.Join(sorted, ",")), nil

	case "apply_manifest":
		return a.applyDispatchApplyManifest(ctx, action)

	case "delete_manifest":
		return a.applyDispatchDeleteManifest(ctx, action)

	case "run_job":
		return a.applyDispatchRunJob(ctx, action)

	default:
		return "", fmt.Errorf("unknown action type %q (supported: config_set, config_unset, apply_manifest, delete_manifest, run_job)", action.Type)
	}
}

// applyDispatchApplyManifest persists a plugin-emitted manifest
// via Store.Put. The same write path `vd apply` uses, so all
// downstream invariants (manifest validation, scoped-kind
// enforcement, asset-digest stamping when applicable, watch loop
// fan-out to handlers) hold automatically.
//
// Why this exists: imperative plugin commands (capture a backup,
// trigger a release, schedule a one-shot) need to materialise
// runtime resources that the operator didn't author. Plugins can
// emit those as manifests through this action instead of touching
// docker / etcd directly. The watch loop then dispatches to the
// kind's reconcile handler (e.g. JobHandler.apply for kind=job),
// keeping the plugin uninvolved in container lifecycle.
//
// Note on triggering job runs: applying a job manifest registers
// it in the store and fires the watch loop's `apply` path, which
// only writes baseline status — it does NOT execute the job.
// Plugins that want immediate execution emit a follow-up call to
// /jobs/run (or, in a future iteration, a `run_job` dispatch
// action that wraps it).
func (a *API) applyDispatchApplyManifest(ctx context.Context, action pluginDispatchAction) (string, error) {
	if action.Manifest == nil {
		return "", fmt.Errorf("apply_manifest requires a manifest payload")
	}

	m := action.Manifest

	if m.Kind == "" || m.Name == "" {
		return "", fmt.Errorf("apply_manifest manifest requires kind and name")
	}

	specRaw, err := json.Marshal(m.Spec)
	if err != nil {
		return "", fmt.Errorf("apply_manifest: marshal spec: %w", err)
	}

	manifest := &Manifest{
		Kind:  Kind(m.Kind),
		Scope: m.Scope,
		Name:  m.Name,
		Spec:  specRaw,
	}

	// Store.Put runs Validate() — kind enforcement, scoped-kind
	// enforcement (e.g. job MUST carry a scope), name non-empty.
	// Errors here are surfaced verbatim so plugin authors get
	// loud feedback when their generated manifest is malformed.
	if _, err := a.Store.Put(ctx, manifest); err != nil {
		return "", fmt.Errorf("apply_manifest: %w", err)
	}

	if m.Scope == "" {
		return fmt.Sprintf("apply_manifest %s/%s", m.Kind, m.Name), nil
	}

	return fmt.Sprintf("apply_manifest %s/%s/%s", m.Kind, m.Scope, m.Name), nil
}

// applyDispatchDeleteManifest removes a manifest from the store
// via Store.Delete. The watch loop fires the kind's reconcile
// handler with WatchDelete, which in turn tears down any docker
// containers / asset files / etc. associated with the manifest.
//
// Same idempotent semantics as `vd delete`: missing manifests are
// not an error (returns "delete_manifest <kind>/<name> (no-op)"
// so the caller can compose action queues without pre-checking
// existence).
//
// The action's top-level Scope/Name + Kind identify the target.
// No payload field — Kind reuses the existing top-level slot to
// keep delete shape minimal.
func (a *API) applyDispatchDeleteManifest(ctx context.Context, action pluginDispatchAction) (string, error) {
	if action.Kind == "" {
		return "", fmt.Errorf("delete_manifest requires kind")
	}

	kind, err := ParseKind(action.Kind)
	if err != nil {
		return "", fmt.Errorf("delete_manifest: %w", err)
	}

	deleted, err := a.Store.Delete(ctx, kind, action.Scope, action.Name)
	if err != nil {
		return "", fmt.Errorf("delete_manifest: %w", err)
	}

	suffix := ""
	if !deleted {
		suffix = " (no-op)"
	}

	if action.Scope == "" {
		return fmt.Sprintf("delete_manifest %s/%s%s", action.Kind, action.Name, suffix), nil
	}

	return fmt.Sprintf("delete_manifest %s/%s/%s%s", action.Kind, action.Scope, action.Name, suffix), nil
}

// applyDispatchRunJob spawns Jobs.RunOnce in a goroutine and
// returns immediately. The dispatch handler is fire-and-forget by
// design — plugin authors who need the run's outcome query it
// after the fact via /describe?kind=job&scope=&name= or by
// listing containers labelled kind=job.
//
// Why goroutine + background context: RunOnce blocks until the
// container exits (could be seconds to hours for a backup
// capture). Holding the dispatch HTTP connection open for the
// duration would tie the operator's `vd` invocation to the run,
// defeating the detached-default UX. The action returns "queued"
// the instant the goroutine is scheduled.
//
// Errors surface to logs (plugin author can grep for the queue
// summary) but NOT to the operator's dispatch response — by the
// time the run actually fails, the operator has already gotten
// the "queued" success. Plugins compose this with apply_manifest:
//
//	Actions: [
//	  { type: "apply_manifest", manifest: {kind: "job", ...} },
//	  { type: "run_job", scope: "...", name: "..." },
//	]
//
// Both actions run in order on the controller side. apply_manifest
// persists the manifest (so RunOnce can read it via Store.Get);
// run_job kicks off the actual execution.
func (a *API) applyDispatchRunJob(ctx context.Context, action pluginDispatchAction) (string, error) {
	if a.Jobs == nil {
		return "", fmt.Errorf("run_job: no job runner configured on this controller")
	}

	scope := action.Scope
	name := action.Name

	// Background ctx because the goroutine outlives the HTTP
	// request. Using r.Context() (via the dispatch path) would
	// cancel the run when the operator's HTTP client disconnects
	// — wrong shape for fire-and-forget.
	go func() {
		bgCtx := context.Background()

		run, err := a.Jobs.RunOnce(bgCtx, scope, name)
		if err != nil {
			// Surface to controller logs only. Operators query
			// /describe?kind=job to see status of failed runs.
			fmt.Fprintf(os.Stderr, "run_job %s/%s failed: %v\n", scope, name, err)
			return
		}

		fmt.Fprintf(os.Stderr, "run_job %s/%s completed: run_id=%s status=%s\n",
			scope, name, run.RunID, run.Status)
	}()

	return fmt.Sprintf("run_job %s/%s queued", scope, name), nil
}

// listAvailableCommands renders the plugin's declared command
// names as a comma-separated, sorted list. Used in the
// "no such command" error so operators see what they CAN type
// without having to read plugin.yml. Capped at 20 entries with
// an ellipsis so the error message stays reasonable for
// large plugins.
func listAvailableCommands(plug *plugins.LoadedPlugin) string {
	names := make([]string, 0, len(plug.Commands))
	for name := range plug.Commands {
		names = append(names, name)
	}

	sort.Strings(names)

	const cap = 20
	if len(names) > cap {
		return strings.Join(names[:cap], ", ") + ", ..."
	}

	return strings.Join(names, ", ")
}

func keysSorted(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
