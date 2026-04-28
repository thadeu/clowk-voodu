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

// newRunCronJobCmd was the standalone subcommand `voodu run cronjob
// <ref>`. Removed in favour of the unified `voodu run <ref>`
// (cmd_run.go), which dispatches to runRunCronJob when the ref
// resolves to a declared cronjob. The runRunCronJob implementation
// stays — only the cobra wiring went away.

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

