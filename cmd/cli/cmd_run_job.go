package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"go.voodu.clowk.in/internal/controller"
)

// newRunJobCmd was the standalone subcommand `voodu run job <ref>`.
// Removed in favour of the unified `voodu run <ref>` (cmd_run.go),
// which dispatches to runRunJob automatically when the ref resolves
// to a declared job. The implementation lives below — only the
// cobra wiring went away.

// runJobResponse mirrors the /jobs/run envelope. The data field
// carries a JobRun whether the call succeeded or failed (the runner
// records the failure before returning), so the CLI can render exit
// code + duration even on error.
type runJobResponse struct {
	Status string             `json:"status"`
	Data   controller.JobRun  `json:"data"`
	Error  string             `json:"error,omitempty"`
}

func runRunJob(cmd *cobra.Command, ref string) error {
	scope, name := splitJobRef(ref)

	if name == "" {
		return fmt.Errorf("job ref %q is empty", ref)
	}

	q := url.Values{}
	q.Set("name", name)

	if scope != "" {
		q.Set("scope", scope)
	}

	root := cmd.Root()

	// Jobs can run for minutes (DB migrations, batch imports). The
	// shared controllerDo helper hard-codes a 30s client timeout —
	// fine for /apply, fatal for /jobs/run. Issue the request directly
	// with no client-side deadline; the server holds the connection
	// for the full job duration.
	base := strings.TrimRight(controllerURL(root), "/")
	full := base + "/jobs/run?" + q.Encode()

	req, err := http.NewRequest(http.MethodPost, full, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", fmt.Sprintf("voodu-cli/%s", version))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("controller POST /jobs/run: %w", err)
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var env runJobResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	switch outputFormat(root) {
	case "json":
		out := json.NewEncoder(os.Stdout)
		out.SetIndent("", "  ")
		_ = out.Encode(env)
	case "yaml":
		_ = yaml.NewEncoder(os.Stdout).Encode(env)
	default:
		renderJobRun(os.Stdout, ref, env.Data)
	}

	if env.Status == "error" || resp.StatusCode >= 400 {
		// Surface the structured error message verbatim — the runner's
		// error wraps the exit code so the operator sees what the
		// process said about itself.
		if env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return formatControllerError(resp.StatusCode, raw)
	}

	return nil
}

// splitJobRef parses "scope/name" or bare "name". The slash is the
// only separator we look for — names themselves can carry hyphens,
// dots, etc. without ambiguity.
func splitJobRef(ref string) (scope, name string) {
	ref = strings.TrimSpace(ref)

	if i := strings.Index(ref, "/"); i >= 0 {
		return ref[:i], ref[i+1:]
	}

	return "", ref
}

// splitReplicaRef parses the per-pod ref shape `<scope>/<name>.<replica>`
// — the natural extension of `<scope>/<name>` for picking ONE specific
// replica out of a fan-out. Examples:
//
//	clowk-lp/redis.0     → ("clowk-lp", "redis", "0")    (statefulset ordinal)
//	clowk-lp/web.a3f9    → ("clowk-lp", "web",   "a3f9") (deployment hex id)
//
// Returns ok=false when the ref doesn't match the shape (no slash, or
// no dot in the name part). Callers fall back to the existing
// scope/name (list all replicas) or container-name resolution paths.
//
// Why a dedicated parser:
//
//   - The on-disk container name is `<scope>-<name>.<replica>` —
//     hyphen between scope and name, dot before replica. Operators
//     don't think about that shape; they think `<scope>/<name>` for
//     the resource and `.N` for the pod.
//   - splitJobRef stops at the first `/` and returns everything after
//     verbatim — `clowk-lp/redis.0` becomes scope=clowk-lp, name=redis.0,
//     which then doesn't match anything in the /pods list because the
//     resource is named `redis`, not `redis.0`.
//   - Splitting at the LAST dot in the name part recovers the boundary:
//     statefulset names can't contain dots (HCL-validated), so any dot
//     in the name part is unambiguously the replica separator.
func splitReplicaRef(ref string) (scope, name, replica string, ok bool) {
	ref = strings.TrimSpace(ref)

	slash := strings.Index(ref, "/")
	if slash < 0 {
		return "", "", "", false
	}

	namePart := ref[slash+1:]

	dot := strings.LastIndex(namePart, ".")
	if dot <= 0 || dot == len(namePart)-1 {
		// No dot, dot at position 0 (empty basename), or dot at the
		// very end (empty replica) — none of these shapes can address
		// a pod, so treat the ref as scope/name.
		return "", "", "", false
	}

	return ref[:slash], namePart[:dot], namePart[dot+1:], true
}

// renderJobRun prints a compact human-readable summary of one job
// execution. Two lines: the headline (succeeded / failed + duration),
// then a detail line with run id and exit code so a future `voodu
// logs job/<scope>/<name>:<run_id>` query has the id ready to copy.
func renderJobRun(w io.Writer, ref string, run controller.JobRun) {
	if run.RunID == "" {
		// Server returned an empty run record — usually a 4xx before
		// the runner got involved (job not found, runner not configured).
		// Skip rendering; the caller surfaces the error message.
		return
	}

	duration := ""

	if !run.EndedAt.IsZero() {
		duration = run.EndedAt.Sub(run.StartedAt).Round(1e6).String()
	}

	switch run.Status {
	case controller.JobStatusSucceeded:
		fmt.Fprintf(w, "job %s succeeded in %s\n", ref, duration)
	case controller.JobStatusFailed:
		fmt.Fprintf(w, "job %s failed in %s (exit %d)\n", ref, duration, run.ExitCode)
	default:
		fmt.Fprintf(w, "job %s status=%s\n", ref, run.Status)
	}

	fmt.Fprintf(w, "  run_id: %s\n", run.RunID)

	if run.Error != "" {
		fmt.Fprintf(w, "  error:  %s\n", run.Error)
	}
}
