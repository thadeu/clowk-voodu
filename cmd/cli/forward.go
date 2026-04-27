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

	if resp.StatusCode >= 400 {
		return fmt.Errorf("controller returned %d: %s", resp.StatusCode, string(raw))
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
		// Pre-envelope plugin output (legacy Gokku shell plugins that
		// print raw text directly). Surface it untouched.
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
