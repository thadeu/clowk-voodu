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
		Short: "voodu — PaaS, declarative HCL, stateful services first-class",
		Long: `voodu is a declarative-HCL deployer and orchestrator.

Apply manifests with 'voodu apply -f file.hcl'; the controller
reconciles deployments, ingresses, jobs, cronjobs and stateful
services (Postgres, Mongo) into running containers.

Use ":" syntax as shorthand for subcommands:
  voodu config:set clowk-lp/web FOO=bar    == voodu config clowk-lp/web set FOO=bar
  voodu postgres:create main                == voodu postgres create main`,
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
		newRemoteCmd(),
		newPluginsCmd(),
	)

	return root
}
