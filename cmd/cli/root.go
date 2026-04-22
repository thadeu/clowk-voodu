package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newRootCmd builds the top-level `voodu` command with all builtins
// attached. The returned tree is mutated by dispatch() to add forwarding
// behavior for unknown subcommands.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "voodu",
		Short: "voodu — PaaS git-push style, stateful services first-class",
		Long: `voodu is a git-push deployer and orchestrator.

It keeps what Gokku did well (git-push deploy, blue/green, config:set)
and adds stateful services (Postgres, Mongo) as first-class citizens.

Use ":" syntax as shorthand for subcommands:
  voodu config:set FOO=bar -a api    == voodu config set FOO=bar -a api
  voodu postgres:create main          == voodu postgres create main`,
		Version:       fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, date),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("voodu {{.Version}}\n")

	root.PersistentFlags().String("controller-url", "", "controller HTTP endpoint (env: VOODU_CONTROLLER_URL)")
	root.PersistentFlags().String("output", "text", "output format: text|json")

	root.AddCommand(
		newVersionCmd(),
		newSetupCmd(),
		newAppsCmd(),
		newDeployCmd(),
		newConfigCmd(),
		newApplyCmd(),
		newStatusCmd(),
		newLogsCmd(),
		newExecCmd(),
		newScaleCmd(),
		newServerCmd(),
		newPluginsCmd(),
	)

	return root
}
