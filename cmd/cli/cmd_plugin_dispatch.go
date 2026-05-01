package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// pluginDispatchCommands names the plugin commands that route
// through the structured dispatch endpoint (POST
// /plugin/{name}/{command}) instead of the generic forward path
// (/plugins/exec). The map value is the number of positional
// refs the verb expects:
//
//   - 2 refs: `<provider> <consumer>`. Provider lands as
//     payload.from (server pre-fetches its spec + config),
//     consumer lands as payload.to. Used by link/unlink.
//   - 1 ref: `<target>`. Target lands as payload.from. Used by
//     unary verbs that operate on a single manifest, like
//     new-password (rotate) — there's no consumer side.
//
// Adding a new structured verb: pick its arity (1 or 2) and
// extend the map. The dispatch detector reads the arity to
// decide how many positionals to slurp. Plugin commands outside
// this set fall through to the generic forward path
// (fire-and-forget RPC, no pre-fetch, no action applier).
var pluginDispatchCommands = map[string]int{
	"link":         2,
	"unlink":       2,
	"new-password": 1,
}

// IsPluginDispatchCommand reports whether the operator-typed
// `<plugin>:<command>` should be routed through the structured
// dispatch endpoint.
func IsPluginDispatchCommand(command string) bool {
	_, ok := pluginDispatchCommands[command]

	return ok
}

// looksLikePluginDispatch matches `vd <plugin>:<command>
// <ref...>` shape: argv[0] is the plugin (after rewriteColonSyntax
// split the colon), argv[1] is the command, and remaining
// positionals are scope/name refs. Used by dispatch() to peel
// these off the unknown-command path and route to the structured
// endpoint.
//
// argv at this point is the post-rewrite shape, e.g.:
//
//	["redis", "link", "clowk-lp/redis", "clowk-lp/web"]
//	["redis", "new-password", "clowk-lp/redis"]
//
// The number of refs slurped depends on the command's declared
// arity in pluginDispatchCommands. Returns the slurped refs as
// `refs[]` so the caller can route them to from/to as the
// command's semantics dictate.
func looksLikePluginDispatch(argv []string) (plugin, command string, refs []string, ok bool) {
	if len(argv) < 2 {
		return "", "", nil, false
	}

	arity, isDispatch := pluginDispatchCommands[argv[1]]
	if !isDispatch {
		return "", "", nil, false
	}

	// Skip flag tokens to find the positional refs.
	positionals := make([]string, 0, len(argv))

	skipNext := false

	for _, tok := range argv[2:] {
		if skipNext {
			skipNext = false
			continue
		}

		if strings.HasPrefix(tok, "-") {
			if !strings.Contains(tok, "=") && takesValue(tok) {
				skipNext = true
			}

			continue
		}

		positionals = append(positionals, tok)
	}

	if len(positionals) < arity {
		return "", "", nil, false
	}

	return argv[0], argv[1], positionals[:arity], true
}

// pluginDispatchPayload mirrors the server-side
// pluginDispatchRequest shape. Defined locally so the CLI doesn't
// have to import the controller package.
type pluginDispatchPayload struct {
	From *pluginDispatchRef `json:"from,omitempty"`
	To   *pluginDispatchRef `json:"to,omitempty"`
}

type pluginDispatchRef struct {
	Kind  string `json:"kind,omitempty"`
	Scope string `json:"scope,omitempty"`
	Name  string `json:"name"`
}

// pluginDispatchResponse is the operator-facing summary the
// server returns after running the plugin and applying any
// actions. CLI prints `message` (operator's success line) and
// optionally each action under it.
type pluginDispatchResponse struct {
	Status string `json:"status"`
	Data   struct {
		Message string   `json:"message"`
		Applied []string `json:"applied"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// runPluginDispatch resolves the refs to {scope, name}, infers
// the from kind from the plugin's emit convention (today: every
// dispatch-capable plugin emits a statefulset — postgres, redis,
// mysql, mongo all fit), POSTs to /plugin/{name}/{command}, and
// renders the server's response.
//
// `refs` length matches the command's declared arity:
//
//   - 1 ref: refs[0] is the target → goes into `from`. No `to`.
//   - 2 refs: refs[0] → from (provider), refs[1] → to (consumer).
//
// Future arities (e.g. 3-ref `migrate <from> <via> <to>`) extend
// the routing here without disturbing the dispatch payload shape.
func runPluginDispatch(root *cobra.Command, plugin, command string, refs []string) error {
	if len(refs) == 0 {
		return fmt.Errorf("usage: vd %s:%s <ref>", plugin, command)
	}

	body := pluginDispatchPayload{}

	fromScope, fromName := splitRefScopeName(refs[0])
	if fromName == "" {
		return fmt.Errorf("invalid ref %q (expected scope/name)", refs[0])
	}

	body.From = &pluginDispatchRef{
		Kind:  pluginDispatchKindFor(plugin),
		Scope: fromScope,
		Name:  fromName,
	}

	if len(refs) >= 2 {
		toScope, toName := splitRefScopeName(refs[1])
		if toName == "" {
			return fmt.Errorf("invalid consumer ref %q (expected scope/name)", refs[1])
		}

		body.To = &pluginDispatchRef{
			Scope: toScope,
			Name:  toName,
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}

	url := strings.TrimRight(controllerURL(root), "/") + "/plugin/" + plugin + "/" + command

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("voodu-cli/%s", version))

	client := &http.Client{Timeout: forwardTimeout}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch %s:%s: controller at %s unreachable (%v)", plugin, command, url, err)
	}
	defer resp.Body.Close()

	body2, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, body2)
	}

	var out pluginDispatchResponse
	if err := json.Unmarshal(body2, &out); err != nil {
		// Pre-envelope shape — print verbatim.
		fmt.Print(string(body2))

		if !bytes.HasSuffix(body2, []byte("\n")) {
			fmt.Println()
		}

		return nil
	}

	if out.Data.Message != "" {
		fmt.Println(out.Data.Message)
	}

	for _, a := range out.Data.Applied {
		fmt.Printf("  ✓ %s\n", a)
	}

	return nil
}

// splitRefScopeName parses "scope/name" or just "name". Mirrors
// splitJobRef but kept independent so the dispatch code stays
// self-contained and the helper's contract is dispatch-local.
func splitRefScopeName(ref string) (scope, name string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", ""
	}

	if i := strings.Index(ref, "/"); i >= 0 {
		return ref[:i], ref[i+1:]
	}

	return "", ref
}

// pluginDispatchKindFor maps a plugin name to the kind its
// `expand` command emits, which is also the kind the dispatch
// endpoint should pre-fetch the spec from. Today every plugin
// in the fleet emits a statefulset — postgres, redis, mysql,
// mongo, clickhouse are all stateful workloads with stable
// per-pod identity. When a plugin emits a different kind, this
// map gains an entry; until then a single default keeps the
// CLI dependency-free of plugin internals.
//
// The downside if we get this wrong: server attaches no spec
// to the plugin's stdin (config still attaches), and the plugin
// has to operate on config alone. Tolerable, and the plugin
// can detect+error if it really needed the spec.
func pluginDispatchKindFor(plugin string) string {
	switch plugin {
	default:
		return "statefulset"
	}
}
