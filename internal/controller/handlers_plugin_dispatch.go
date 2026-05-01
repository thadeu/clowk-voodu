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
// `POST /plugin/{name}/{command}` endpoint. It's the operator-side
// CLI handing the controller two manifest references (the
// "provider" and the "consumer" in linking parlance) plus opaque
// extra args that go straight to the plugin's stdin.
//
// The shape is deliberately generic so a plugin command isn't
// pinned to the link/unlink use case — `from` and `to` are
// optional, `args` and `extra` carry whatever the plugin command
// needs. A future `vd <plugin>:<other-command>` can reuse the
// same envelope without API churn.
type pluginDispatchRequest struct {
	From  *pluginManifestRef `json:"from,omitempty"`
	To    *pluginManifestRef `json:"to,omitempty"`
	Args  []string           `json:"args,omitempty"`
	Extra map[string]any     `json:"extra,omitempty"`
}

// pluginManifestRef points at a single manifest the plugin wants
// to know about. Kind is optional — when empty the controller
// skips the spec pre-fetch and only attaches the config bucket
// (useful for the `to` side of a link, which is just a target,
// not yet associated with any specific kind).
type pluginManifestRef struct {
	Kind  string `json:"kind,omitempty"`
	Scope string `json:"scope,omitempty"`
	Name  string `json:"name"`
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
}

// pluginDispatchResponse mirrors what plugins must emit on stdout
// inside a JSON envelope (envelope.Data is unmarshalled into this
// shape). `message` is an operator-facing one-liner; `actions`
// is the queue the controller applies post-invoke.
type pluginDispatchResponse struct {
	Message string                 `json:"message,omitempty"`
	Actions []pluginDispatchAction `json:"actions,omitempty"`
}

// handlePluginCommand dispatches `POST /plugin/{name}/{command}`.
//
// Lifecycle of one call:
//
//  1. Parse {name, command} from URL, decode body.
//  2. Load the plugin from PluginsRoot. If the command isn't
//     declared in plugin.yml, 400 — never invoke an
//     undocumented command (matches the contract every other
//     plugin call follows).
//  3. Pre-fetch the `from` manifest's spec + merged config and
//     the `to` config (when scope/name supplied). Plugin gets
//     this as part of its stdin so it doesn't need a callback
//     channel back into the controller.
//  4. Run the plugin via the same code path as `expand`
//     (LoadFromDir + Run). Stdin is JSON; stdout is parsed as a
//     plugin envelope and the Data block is decoded into
//     pluginDispatchResponse.
//  5. Apply each action in order. Any action error aborts the
//     remaining ones and surfaces 5xx — partial-apply semantics
//     would be confusing for operators.
//  6. Return 200 with the plugin's `message` and a list of the
//     actions actually applied (operator can render this as the
//     command's success output).
//
// Failure modes the operator sees:
//
//   - 400 plugin not installed
//   - 400 command not declared in plugin.yml
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
		// Plugin commands are discovered by FILE in `bin/` (or the
		// legacy `commands/` dir), not by the plugin.yml `commands:`
		// list. Common mistake: declaring the command in plugin.yml
		// without dropping a matching shim script in bin/ that
		// re-execs the actual binary.
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"plugin %q does not have an executable named %q under bin/ (each subcommand needs a shim file in bin/<name> that re-execs the plugin binary)",
			pluginName, command))
		return
	}

	// Pre-fetch context for both refs. Failures are non-fatal —
	// the plugin can still decide to operate with whatever
	// context arrived (maybe it only needs `from.spec`, maybe
	// it doesn't care about config). We log the failure into
	// the stdin payload so the plugin can detect partial state.
	stdinPayload := map[string]any{
		"plugin":  pluginName,
		"command": command,
	}

	if req.From != nil {
		stdinPayload["from"] = a.fetchPluginCtxForRef(r.Context(), *req.From)
	}

	if req.To != nil {
		stdinPayload["to"] = a.fetchPluginCtxForRef(r.Context(), *req.To)
	}

	if len(req.Args) > 0 {
		stdinPayload["args"] = req.Args
	}

	if len(req.Extra) > 0 {
		stdinPayload["extra"] = req.Extra
	}

	stdin, err := json.Marshal(stdinPayload)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("marshal plugin stdin: %w", err))
		return
	}

	res, err := plug.Run(r.Context(), plugins.RunOptions{
		Command: command,
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
		writeErr(w, http.StatusBadRequest, fmt.Errorf("plugin %q %s emitted no JSON envelope (got: %s)",
			pluginName, command, truncateForLog(res.Raw, 200)))
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

// fetchPluginCtxForRef gathers the context a plugin command
// typically needs about one manifest: kind/scope/name (verbatim
// from the request) plus optionally the spec (when a kind is
// supplied) and the merged config (always). Errors during the
// store reads are swallowed — the plugin gets best-effort context
// and decides whether the missing piece is fatal for its logic.
func (a *API) fetchPluginCtxForRef(ctx context.Context, ref pluginManifestRef) map[string]any {
	out := map[string]any{
		"kind":  ref.Kind,
		"scope": ref.Scope,
		"name":  ref.Name,
	}

	if ref.Kind != "" && a.Store != nil {
		if kind, err := ParseKind(ref.Kind); err == nil {
			if m, err := a.Store.Get(ctx, kind, ref.Scope, ref.Name); err == nil && m != nil {
				if len(m.Spec) > 0 {
					var spec map[string]any
					if err := json.Unmarshal(m.Spec, &spec); err == nil {
						out["spec"] = spec
					}
				}
			}
		}
	}

	if a.Store != nil {
		if cfg, err := a.Store.ResolveConfig(ctx, ref.Scope, ref.Name); err == nil && len(cfg) > 0 {
			out["config"] = cfg
		}
	}

	return out
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

	// Trigger the same restart fan-out `vd config set` does so
	// consumer pods pick up the new env file without a manual
	// nudge. Always-on for plugin dispatch: the operator
	// invoking `vd <plugin>:link` expects the link to be live.
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
