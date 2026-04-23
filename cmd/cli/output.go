package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// outputFormat reads --output from the root persistent flags. Returns
// "text" when unset or unrecognised — plugins shouldn't crash because
// of a bad flag.
func outputFormat(root *cobra.Command) string {
	v, _ := root.PersistentFlags().GetString("output")

	switch strings.ToLower(strings.TrimSpace(v)) {
	case "json":
		return "json"
	case "yaml", "yml":
		return "yaml"
	default:
		return "text"
	}
}

// renderEnvelope prints a forwardResponse (already decoded) according to
// the caller's requested -o format. For "text" it mimics the legacy
// renderer: errors surface, Data is pretty-printed, plain stdout is
// passed through verbatim. For "json" / "yaml" the full envelope is
// serialised as a single document so tooling can pipe it.
func renderEnvelope(root *cobra.Command, r forwardResponse, raw []byte) error {
	switch outputFormat(root) {
	case "json":
		return writeJSONEnvelope(os.Stdout, r)

	case "yaml":
		return writeYAMLEnvelope(os.Stdout, r)
	}

	return writeTextEnvelope(os.Stdout, r, raw)
}

func writeTextEnvelope(w io.Writer, r forwardResponse, raw []byte) error {
	if r.Status == "error" {
		return fmt.Errorf("%s", r.Error)
	}

	if stdout := extractStdout(r.Data); stdout != "" {
		fmt.Fprint(w, stdout)

		if !strings.HasSuffix(stdout, "\n") {
			fmt.Fprintln(w)
		}

		return nil
	}

	if len(r.Data) > 0 {
		var pretty bytes.Buffer

		if err := json.Indent(&pretty, r.Data, "", "  "); err == nil {
			fmt.Fprintln(w, pretty.String())
			return nil
		}

		fmt.Fprintln(w, string(r.Data))
		return nil
	}

	fmt.Fprint(w, string(raw))

	return nil
}

func writeJSONEnvelope(w io.Writer, r forwardResponse) error {
	out := map[string]any{"status": r.Status}

	if len(r.Data) > 0 {
		var data any
		if err := json.Unmarshal(r.Data, &data); err == nil {
			out["data"] = data
		} else {
			out["data"] = string(r.Data)
		}
	}

	if r.Error != "" {
		out["error"] = r.Error
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

func writeYAMLEnvelope(w io.Writer, r forwardResponse) error {
	out := map[string]any{"status": r.Status}

	if len(r.Data) > 0 {
		var data any
		if err := json.Unmarshal(r.Data, &data); err == nil {
			out["data"] = data
		} else {
			out["data"] = string(r.Data)
		}
	}

	if r.Error != "" {
		out["error"] = r.Error
	}

	return yaml.NewEncoder(w).Encode(out)
}

// extractStdout recognises the controller's plain-text passthrough
// shape: {"status":"ok","data":{"stdout":"..."}}. Returns empty string
// when Data doesn't match, so the caller falls back to pretty-printing.
func extractStdout(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}

	var obj struct {
		Stdout string `json:"stdout"`
	}

	if err := json.Unmarshal(data, &obj); err != nil {
		return ""
	}

	return obj.Stdout
}
