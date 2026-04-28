package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// newRollbackCmd is the top-level rollback verb. Mirrors
// `heroku rollback`: revert to a past release's snapshot.
//
//	vd rollback <ref>                  previous release (auto-pick)
//	vd rollback <ref> 1ksdtcj7e        exact target by id
//
// Distinct from `vd release <ref> run` (re-fires the release-phase
// command for the current spec) and `vd release <ref>` (the
// history listing). Rollback is its own verb because it changes
// desired state — it's closer to `vd apply` of a past spec than
// to anything in the release surface.
//
// The mechanics: server reads the deployment's release history,
// finds the target ID, re-Puts the manifest with that release's
// snapshot. The reconciler picks up the change and runs a normal
// recreate flow. The release-phase command does NOT re-run
// because the target's hash already has a Succeeded release
// record (idempotency).
//
// A new release record IS created — the timeline is linear, not
// circular. Rolling back from rel5 to rel3 produces a brand-new
// id (rel6, etc.) with rolled_back_from=rel3 in its history record
// so audits stay readable.
func newRollbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollback <ref> [release_id]",
		Short: "Revert a deployment to a past release",
		Long: `Re-applies a past release's spec snapshot to the deployment,
triggering a normal recreate flow. The release-phase command from
the target release does NOT re-run (its hash already has a
Succeeded record), so only the rolling restart happens.

A new release record is appended to history with a fresh id —
rollbacks are linear in the timeline, not circular. Rolling back
from rel5 to rel3 mints a new id and records rolled_back_from=rel3
on it.

Without [release_id], rolls back to the release immediately before
the current. With a release_id, exact target.

Examples:
  vd rollback clowk-lp/web              # rollback to previous release
  vd rollback clowk-lp/web 1ksdtcj7e    # rollback to a specific id

Inspect release history first to know your options:
  vd release clowk-lp/web`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]

			releaseID := ""
			if len(args) == 2 {
				releaseID = strings.TrimSpace(args[1])
			}

			return runRollback(cmd, ref, releaseID)
		},
	}

	return cmd
}

func runRollback(cmd *cobra.Command, ref, releaseID string) error {
	scope, name := splitJobRef(ref)

	if name == "" {
		return fmt.Errorf("rollback ref %q is empty", ref)
	}

	q := url.Values{}
	q.Set("name", name)

	if scope != "" {
		q.Set("scope", scope)
	}

	if releaseID != "" {
		q.Set("release_id", releaseID)
	}

	resp, err := controllerDo(cmd.Root(), http.MethodPost, "/rollback", q.Encode(), nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, raw)
	}

	var env struct {
		Data struct {
			RolledBackTo string `json:"rolled_back_to"`
			NewRelease   string `json:"new_release"`
		} `json:"data"`
	}

	_ = json.Unmarshal(raw, &env)

	if env.Data.RolledBackTo != "" {
		fmt.Printf("rolled back to %s (new release %s)\n", env.Data.RolledBackTo, env.Data.NewRelease)
	} else {
		fmt.Printf("rolled back (new release %s)\n", env.Data.NewRelease)
	}

	return nil
}
