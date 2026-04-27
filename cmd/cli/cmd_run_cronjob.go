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
)

// newRunCronJobCmd builds `voodu run cronjob <scope>/<name>`. Forces
// one immediate execution of a previously-applied cronjob, bypassing
// the schedule. Useful when the operator just shipped a fix and wants
// to verify it works without waiting for the next scheduled tick —
// the scheduler's normal cadence is unaffected.
//
// Distinct from `vd exec` (M-3): `run cronjob` spawns a brand-new
// container using the cronjob spec; `exec` enters an existing
// container. Same axis as kubernetes "create a Job from a CronJob"
// vs "kubectl exec into a Pod".
func newRunCronJobCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cronjob <scope>/<name> | <name>",
		Short: "Force an immediate run of a previously-applied cronjob",
		Long: `Trigger one execution of a declared cronjob manifest, bypassing
the schedule. The cronjob spec is fetched from the controller (it
must have been applied first via 'voodu apply'), a one-shot
container is spawned with the spec's image / command / env, and the
call blocks until the container exits.

The scheduler's normal tick cadence is NOT affected — this is an
extra ad-hoc run on top of any scheduled ones. Useful for "I just
fixed the bug, run it now to confirm" flows without having to wait
for the next */15 fire.

Examples:
  voodu run cronjob ops/purge        scope=ops, name=purge
  voodu run cronjob crawler1         if 'crawler1' is unambiguous`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunCronJob(cmd, args[0])
		},
	}

	return cmd
}

func runRunCronJob(cmd *cobra.Command, ref string) error {
	scope, name := splitJobRef(ref)

	if name == "" {
		return fmt.Errorf("cronjob ref %q is empty", ref)
	}

	q := url.Values{}
	q.Set("name", name)

	if scope != "" {
		q.Set("scope", scope)
	}

	root := cmd.Root()

	// Cronjob runs can be as long as a job — same no-deadline policy.
	// /cronjobs/run holds the connection open until the spawned
	// container exits.
	base := strings.TrimRight(controllerURL(root), "/")
	full := base + "/cronjobs/run?" + q.Encode()

	req, err := http.NewRequest(http.MethodPost, full, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", fmt.Sprintf("voodu-cli/%s", version))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("controller POST /cronjobs/run: %w", err)
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
		renderJobRun(os.Stdout, "cronjob/"+ref, env.Data)
	}

	if env.Status == "error" || resp.StatusCode >= 400 {
		if env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return fmt.Errorf("controller returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	return nil
}

