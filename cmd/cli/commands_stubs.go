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

func newStatusCmd() *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the current state of apps and services",
		RunE:  stubRunE("M3", "Status reads from the controller, which arrives in M3."),
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "filter by app name")

	return cmd
}

func newLogsCmd() *cobra.Command {
	var app string

	var follow bool

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Tail logs for an app",
		RunE:  stubRunE("M3", "Log streaming goes through the controller (M3)."),
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new lines")

	return cmd
}

func newExecCmd() *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "exec -- CMD [ARGS...]",
		Short: "Run a one-off command inside the app's container",
		RunE:  stubRunE("M3", "One-off exec runs via the controller (M3)."),
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")

	return cmd
}

func newScaleCmd() *cobra.Command {
	var app string

	var replicas int

	cmd := &cobra.Command{
		Use:   "scale",
		Short: "Set the number of replicas for an app",
		RunE:  stubRunE("M3", "Scaling is declarative via `voodu apply` (M4) or directly via the controller (M3)."),
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")
	cmd.Flags().IntVarP(&replicas, "replicas", "r", 0, "desired replica count")

	return cmd
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

