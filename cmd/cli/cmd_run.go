package main

import (
	"github.com/spf13/cobra"
)

// newRunCmd is the umbrella for imperative one-shot executions of
// previously-declared resources: `voodu run job <ref>`, `voodu run
// cronjob <ref>` (force-tick now). Distinct from `voodu apply`
// (sets desired state) and `voodu exec` (enters an existing
// container) — `run` SPAWNS A NEW CONTAINER from a manifest that
// was already applied.
//
// Mental model:
//
//	apply  → set desired state (durable)
//	run    → spawn one fresh container from a declared resource (transient)
//	exec   → enter a container that's already running (M-3)
//
// All three verbs are mutually exclusive — there's no overlap, and
// any new "do something to a deployed resource" capability should
// pick a clear lane.
func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Spawn a one-shot container from a declared job or cronjob",
		Long: `Imperative actions that consume a previously-applied resource and
execute it once. Unlike 'voodu apply' which manipulates desired
state in etcd, 'voodu run' produces a transient side effect — a
new container spawned from the manifest, which runs to completion
and is then garbage-collected per the resource's history limits.

Unlike 'voodu exec' which enters an existing container, 'voodu run'
ALWAYS creates a brand-new container.

Subcommands:
  job <ref>       run a declared job once
  cronjob <ref>   force a cronjob tick now, ignoring its schedule

The declared manifest must already exist on the controller (apply
it first).`,
	}

	cmd.AddCommand(newRunJobCmd())
	cmd.AddCommand(newRunCronJobCmd())

	return cmd
}
