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

	// Cobra auto-registers a `completion` subcommand by default. We don't
	// ship shell completions yet, and leaving it in pollutes `voodu --help`
	// with a command that advertises functionality the CLI doesn't provide.
	// Re-enable by removing this line when completions become a real thing.
	root.CompletionOptions.DisableDefaultCmd = true

	root.PersistentFlags().String("controller-url", "", "controller HTTP endpoint (env: VOODU_CONTROLLER_URL)")
	root.PersistentFlags().StringP("output", "o", "text", "output format: text|json|yaml")
	root.PersistentFlags().StringP("remote", "r", "", "voodu remote name to forward to (client mode only; defaults to the 'voodu' git remote)")

	root.AddCommand(
		newVersionCmd(),
		newSetupCmd(),
		newReceivePackCmd(),
		newConfigCmd(),
		newApplyCmd(),
		newDiffCmd(),
		newDeleteCmd(),
		newGetCmd(),
		newRunCmd(),
		newRestartCmd(),
		newReleaseCmd(),
		newRollbackCmd(),
		newDescribeCmd(),
		newLogsCmd(),
		newExecCmd(),
		newServerCmd(),
		newRemoteCmd(),
		newPluginsCmd(),
	)

	return root
}
