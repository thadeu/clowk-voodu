package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// stubRunE is used for commands whose real implementation lands in a
// later milestone. They are registered so `voodu --help` shows the
// shape of the CLI, and so forwarding doesn't accidentally try to
// contact the controller for things we know are coming.
func stubRunE(milestone, hint string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("%s: not yet implemented (lands in %s). %s", cmd.CommandPath(), milestone, hint)
	}
}

func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Manage Voodu server nodes",
	}

	cmd.AddCommand(
		serverInitCmd(),
		&cobra.Command{
			Use:   "add HOST",
			Short: "Install the controller on a remote host over SSH",
			Args:  cobra.ExactArgs(1),
			RunE:  stubRunE("M3+", "Remote bootstrap over SSH is a M3 follow-up. For now run `voodu server init` on the target host."),
		},
		&cobra.Command{
			Use:   "list",
			Short: "List registered server nodes",
			RunE:  stubRunE("M3+", "Node registry lives in etcd and is populated by the controller (M3+)."),
		},
	)

	return cmd
}

