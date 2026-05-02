package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
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
// Today only `config_set` and `config_unset` are recognised.
// Unknown action types are rejected (loud failure beats silent
// no-op when a plugin author typos a type).
type pluginDispatchAction struct {
	Type  string            `json:"type"`
	Scope string            `json:"scope"`
	Name  string            `json:"name"`
	KV    map[string]string `json:"kv,omitempty"`
	Keys  []string          `json:"keys,omitempty"`

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
	// every existing plugin — opt-in only.
	SkipRestart bool `json:"skip_restart,omitempty"`
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

	plug, err := plugins.LoadFromDir(filepath.Join(a.PluginsRoot, pluginName))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("plugin %q: %w", pluginName, err))
		return
	}

	if _, declared := plug.Commands[command]; !declared {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"plugin %q does not have an executable named %q under bin/ (each subcommand needs a shim file in bin/<name> that re-execs the plugin binary)",
			pluginName, command))
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

	for i, action := range resp.Actions {
		summary, err := a.applyPluginDispatchActionFromRequest(r, action)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("apply action %d (%s): %w", i, action.Type, err))
			return
		}

		applied = append(applied, summary)
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data: map[string]any{
			"message": resp.Message,
			"applied": applied,
		},
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

	default:
		return "", fmt.Errorf("unknown action type %q (supported: config_set, config_unset)", action.Type)
	}
}

func keysSorted(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
