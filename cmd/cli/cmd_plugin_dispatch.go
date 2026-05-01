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

// looksLikePluginDispatch matches a `vd <plugin>:<command>
// [args...]` invocation that should route to the structured
// dispatch endpoint. The CLI is a DUMB FORWARDER — no arity
// knowledge, no per-verb hardcoded behaviour, no help intercept.
// All semantics live in the plugin itself; the CLI just packages
// the operator's args and POSTs them.
//
// Detection rule: argv has at least 2 tokens, both are plain
// alphanumeric identifiers (after rewriteColonSyntax has split
// any `:` shorthand). Everything after argv[1] is treated as
// the plugin command's args, including flags like `-h` —
// those flow through to the plugin which is responsible for
// its own help output.
//
// Returns (plugin, command, args, true) on a match.
func looksLikePluginDispatch(argv []string) (plugin, command string, args []string, ok bool) {
	if len(argv) < 2 {
		return "", "", nil, false
	}

	if !isIdent(argv[0]) || !isIdent(argv[1]) {
		return "", "", nil, false
	}

	return argv[0], argv[1], argv[2:], true
}

// pluginDispatchPayload mirrors the server-side
// pluginDispatchRequest. Body is just `{args}` — no from/to
// pre-fetch hints. Plugin parses args itself.
type pluginDispatchPayload struct {
	Args []string `json:"args,omitempty"`
}

type pluginDispatchResponse struct {
	Status string `json:"status"`
	Data   struct {
		Message string   `json:"message"`
		Applied []string `json:"applied"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// runPluginDispatch POSTs the operator's args to the plugin
// dispatch endpoint and renders the response. The CLI doesn't
// inspect the args — they're whatever the operator typed after
// `<plugin>:<command>`, including positional refs and flags.
//
// Plugin is responsible for parsing its own argv (via
// os.Args[2:] when invoked) and for emitting envelope-shaped
// stdout. Server applies any `actions` returned and surfaces
// the `message` back here.
func runPluginDispatch(root *cobra.Command, plugin, command string, args []string) error {
	body := pluginDispatchPayload{Args: args}

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
		// Plugin emitted plain text — print as-is.
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

// isIdent is defined in dispatch.go (shared with the colon
// splitter). The dispatch detector and splitCommandColon both
// gate on the same identifier rule.
