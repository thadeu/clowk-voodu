package main

import (
	"github.com/spf13/cobra"
)

// newRunCmd is the umbrella for imperative one-shot executions:
// `voodu run job <ref>`, future `voodu run shell`, etc. Distinct from
// `voodu apply` (which sets desired state) — running an action does
// not change the manifests; it consumes a previously-applied job and
// kicks it off once.
func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Imperative one-shot executions (jobs, shells)",
		Long: `Imperative actions that consume a previously-applied resource and
execute it once.

Unlike 'voodu apply' which manipulates desired state in etcd, 'voodu
run' produces a transient side effect — a job container that runs to
completion and disappears. The declared manifest must already exist
in the controller (apply it first).`,
	}

	cmd.AddCommand(newRunJobCmd())

	return cmd
}
