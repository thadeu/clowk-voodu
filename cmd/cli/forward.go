package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	envControllerURL     = "VOODU_CONTROLLER_URL"
	defaultControllerURL = "http://127.0.0.1:8686"
	forwardTimeout       = 30 * time.Second
)

// controllerURL resolves the controller endpoint in priority order:
// --controller-url flag, VOODU_CONTROLLER_URL env, default localhost.
// Root flags are parsed even on the forwarding path so this can see
// --controller-url when set.
func controllerURL(root *cobra.Command) string {
	if v, _ := root.PersistentFlags().GetString("controller-url"); v != "" {
		return v
	}

	if v := os.Getenv(envControllerURL); v != "" {
		return v
	}

	return defaultControllerURL
}

// forwardRequest is the JSON body sent to the controller's /exec endpoint
// for unknown commands. Keep this stable — plugins authors will read it.
type forwardRequest struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env,omitempty"`
}

type forwardResponse struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// formatControllerError turns a non-2xx response from the controller
// into a human-readable error. The server speaks the envelope
// `{"status":"error","error":"<message>"}` for every failure path,
// so the CLI extracts the inner message and drops the JSON wrapper —
// otherwise the operator sees `controller returned 400:
// {"status":"error","error":"need at least <plugin> <command>"}`
// instead of the actual diagnostic.
//
// Falls back to the raw body when the response isn't a parseable
// envelope (network proxy 502, malformed server crash, etc.) so
// genuinely strange failures still surface their bytes.
//
// Status code is omitted on purpose: the inner message already names
// the specific problem, and the bare number ("400", "503") is noise
// most of the time. The shorter form reads as a sentence.
func formatControllerError(statusCode int, raw []byte) error {
	trimmed := strings.TrimSpace(string(raw))

	if trimmed == "" {
		return fmt.Errorf("controller returned %d (empty body)", statusCode)
	}

	if trimmed[0] == '{' {
		var env forwardResponse

		if err := json.Unmarshal([]byte(trimmed), &env); err == nil && env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}
	}

	return fmt.Errorf("controller returned %d: %s", statusCode, trimmed)
}

// forwardToController POSTs unknown-command args to the controller and
// prints whatever it returns. In M2 there is no controller yet, so this
// will fail with a clear "plugin not found or controller unreachable"
// message — which is the intended UX per the plan.
func forwardToController(root *cobra.Command, args []string) error {
	// Parse persistent flags so --controller-url takes effect even when
	// we are not going to hand off to cobra's Execute path.
	_ = root.PersistentFlags().Parse(filterFlags(args))

	url := strings.TrimRight(controllerURL(root), "/") + "/plugins/exec"

	body, err := json.Marshal(forwardRequest{Args: args})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("voodu-cli/%s", version))

	client := &http.Client{Timeout: forwardTimeout}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf(
			"unknown command %q: no matching builtin, and controller at %s is unreachable (%v)\n"+
				"hint: plugins land in M5; the controller daemon arrives in M3",
			strings.Join(args, " "), url, err,
		)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("unknown command %q: no builtin and no plugin registered", strings.Join(args, " "))
	}

	// `vd <single-token>` (e.g. `vd apps` after the apps command was
	// retired) reaches here with len(args)==1 and trips the server's
	// "need at least <plugin> <command>" guard. From the operator's
	// perspective they typed an unknown verb; surfacing the plugin-
	// system internal is misleading. Reword to match the no-plugin
	// path above so the two unknown-command outcomes read alike.
	if resp.StatusCode == http.StatusBadRequest && len(args) < 2 {
		return fmt.Errorf("unknown command %q: no builtin and no plugin registered", strings.Join(args, " "))
	}

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, raw)
	}

	return renderForwardResponse(root, raw)
}

// renderForwardResponse handles the plugin JSON protocol. Plain stdout
// (non-JSON) is printed as-is so shell plugins that don't produce JSON
// still work. When --output is json|yaml the envelope is serialised
// verbatim; otherwise the default "text" rendering hides the protocol.
func renderForwardResponse(root *cobra.Command, raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}

	if trimmed[0] != '{' {
		// Pre-envelope plugin output (shell plugins that print raw
		// text directly instead of emitting the JSON envelope).
		// Surface it untouched.
		fmt.Print(string(raw))

		if !bytes.HasSuffix(raw, []byte("\n")) {
			fmt.Println()
		}

		return nil
	}

	var r forwardResponse
	if err := json.Unmarshal(trimmed, &r); err != nil {
		fmt.Print(string(raw))
		return nil
	}

	return renderEnvelope(root, r, raw)
}

// filterFlags returns only the flag tokens from args, so we can feed them
// to PersistentFlags().Parse without tripping on positional args.
func filterFlags(args []string) []string {
	out := make([]string, 0, len(args))

	takeValue := false

	for _, tok := range args {
		if takeValue {
			out = append(out, tok)
			takeValue = false

			continue
		}

		if strings.HasPrefix(tok, "-") {
			out = append(out, tok)

			if !strings.Contains(tok, "=") && takesValue(tok) {
				takeValue = true
			}
		}
	}

	return out
}
