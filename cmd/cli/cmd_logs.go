package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
)

// newLogsCmd builds `voodu logs <kind> <ref>` — the read-side counterpart
// to `voodu run job` and the recurring cronjob ticks. The CLI fans the
// request out to GET /logs and copies the streamed body straight to
// stdout so a `voodu logs -f` feels like `docker logs -f` underneath.
//
// Two-arg shape mirrors describe: <kind> and <ref>. <ref> is
// `scope/name` or bare `name` (controller resolves the scope when
// unambiguous). Kind is one of deployment / job / cronjob — exactly the
// kinds that produce containers.
//
// Default behaviour picks the most recent run/replica. --run lets the
// operator point at a specific replica id (visible in `voodu describe`
// history or `voodu get pods`).
func newLogsCmd() *cobra.Command {
	var (
		runID  string
		follow bool
		tail   int
	)

	cmd := &cobra.Command{
		Use:   "logs <kind> <ref>",
		Short: "Stream container logs for a deployment, job, or cronjob run",
		Long: `Stream stdout+stderr from a voodu-managed container.

Kinds accepted: deployment, job, cronjob. <ref> is "<scope>/<name>" or
bare "<name>" (the controller resolves the scope when unambiguous).

By default, the latest run / replica is selected — running containers
are preferred, falling back to the most recent stopped one. Use --run
to pin a specific replica id (those appear in 'voodu describe' history
and 'voodu get pods').

Job and cronjob run containers are kept around per their
successful_history_limit / failed_history_limit (defaults: 3 / 1), so
'voodu logs job <name>' replays the most recent execution without
re-running it.

Examples:
  voodu logs job api/migrate                       latest run
  voodu logs job api/migrate --run 7e2a            specific run
  voodu logs cronjob crawler1 --tail 100           last 100 lines
  voodu logs deployment web -f                     follow live`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd, args[0], args[1], runID, follow, tail)
		},
	}

	cmd.Flags().StringVar(&runID, "run", "", "specific replica id (default: latest)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new lines as they arrive")
	cmd.Flags().IntVar(&tail, "tail", 0, "limit output to the last N lines (0 = all)")

	return cmd
}

func runLogs(cmd *cobra.Command, kindArg, ref, runID string, follow bool, tail int) error {
	kindArg = strings.TrimSpace(strings.ToLower(kindArg))

	kind, err := controller.ParseKind(kindArg)
	if err != nil {
		return fmt.Errorf("kind %q: %w", kindArg, err)
	}

	if !logsKindSupported(kind) {
		return fmt.Errorf("kind %q does not produce containers (try deployment, job, or cronjob)", kind)
	}

	scope, name := splitJobRef(ref)

	if name == "" {
		return fmt.Errorf("ref %q is empty", ref)
	}

	q := url.Values{}
	q.Set("kind", string(kind))
	q.Set("name", name)

	if scope != "" {
		q.Set("scope", scope)
	}

	if runID != "" {
		q.Set("run", runID)
	}

	if follow {
		q.Set("follow", "true")
	}

	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}

	root := cmd.Root()

	// Logs are streamed for as long as the container runs (with -f) or
	// until docker hits EOF (without). The shared controllerDo helper
	// hard-codes a 30s client timeout; we issue the request directly so
	// `voodu logs -f` can stay open for hours without tripping it.
	base := strings.TrimRight(controllerURL(root), "/")
	full := base + "/logs?" + q.Encode()

	req, err := http.NewRequest(http.MethodGet, full, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", fmt.Sprintf("voodu-cli/%s", version))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("controller GET /logs: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Errors come back as JSON envelopes (resolution failures,
		// streamer not configured). Decode-and-render so the operator
		// sees the same message the controller logged. If the body
		// isn't JSON, fall through to a raw dump.
		raw, _ := io.ReadAll(resp.Body)

		var env struct {
			Error string `json:"error"`
		}

		if json.Unmarshal(raw, &env) == nil && env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return fmt.Errorf("controller returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	// X-Voodu-Container / X-Voodu-Run let the operator confirm which
	// run is being tailed when --run is omitted (especially useful for
	// cronjobs where the latest tick changes minute by minute).
	if container := resp.Header.Get("X-Voodu-Container"); container != "" {
		fmt.Fprintf(os.Stderr, "==> %s", container)

		if run := resp.Header.Get("X-Voodu-Run"); run != "" {
			fmt.Fprintf(os.Stderr, " (run %s)", run)
		}

		fmt.Fprintln(os.Stderr)
	}

	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return fmt.Errorf("stream logs: %w", err)
	}

	return nil
}

// logsKindSupported is the per-kind allowlist for `voodu logs`. The
// /logs endpoint will refuse anything else (database/ingress don't
// produce voodu-managed run containers), but failing client-side gives
// a friendlier error.
func logsKindSupported(kind controller.Kind) bool {
	switch kind {
	case controller.KindDeployment, controller.KindJob, controller.KindCronJob:
		return true
	}

	return false
}
