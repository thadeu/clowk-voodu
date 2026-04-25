package main

import (
	"github.com/spf13/cobra"
)

// newGetCmd is the umbrella for read-only listings (`voodu get pods`,
// future `voodu get apps`, `voodu get jobs`, …). Mirrors the kubectl
// shape so operators have one verb to reach for when they want to
// inspect the running state of the cluster.
func newGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Read-only listings of voodu-managed resources",
		Long: `Read-only commands that ask the controller "what's running?".

Unlike 'voodu apply' which manipulates desired state in etcd, 'voodu
get' reads runtime state directly from the host (docker labels,
plugin status). The output is meant for humans first; -o json|yaml
is honored for scripting.`,
	}

	cmd.AddCommand(newGetPodsCmd())

	return cmd
}
