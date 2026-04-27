package main

import (
	"github.com/spf13/cobra"
)

// newGetPodCmd is the singular counterpart to `voodu get pods`,
// chosen for ergonomics: when an operator types `vd get pd` they
// usually mean one of two things, and the command resolves both
// without a second mental hop:
//
//   - `vd get pd`              → list every voodu pod (alias of
//                                'voodu get pods', filters and all)
//   - `vd get pd <ref>`        → rich detail for one (or all
//                                matching) replicas, alias of
//                                'voodu describe pod <ref>'
//
// Why one command, two modes: the muscle memory for `vd get pods`
// is plural, but the same ergonomics call for the short form `vd
// get pd` to "just work" without an arg. Forcing them into separate
// names (e.g. `get pd` only for detail, `get pds` for the list)
// would surprise more than help. argc-based routing keeps both
// shapes in one place.
//
// Filter flags (--kind/--scope/--name) are forwarded to the listing
// path; --show-env is only meaningful for the detail path. Cobra
// accepts both regardless of mode — passing --show-env without a
// ref is silently ignored, same way passing --kind with a ref is.
// The help text spells out which flag belongs to which mode.
func newGetPodCmd() *cobra.Command {
	var (
		showEnv bool
		filters getPodsFlags
	)

	cmd := &cobra.Command{
		Use:     "pod [ref]",
		Aliases: []string{"pd"},
		Short:   "List voodu pods (no ref) or show detail for one (alias of 'describe pod')",
		Long: `pod inspects voodu-managed containers, in two shapes:

  voodu get pod                        list every pod (= 'get pods')
  voodu get pod <ref>                  rich detail (= 'describe pod')

When no <ref> is given, this is exactly equivalent to 'voodu get
pods' — same output, same filter flags. When a <ref> is given,
it's exactly equivalent to 'voodu describe pod <ref>'.

<ref> accepts three shapes:

  <scope>           every container in this scope, across kinds —
                    "what's running for app X right now?".
  <scope>/<name>    every container matching the (scope, name)
                    identity — all replicas of one resource.
  <container_name>  single replica by docker container name, as it
                    appears in 'voodu get pods' (e.g. clowk-lp-web.a3f9).

Filter flags (--kind/--scope/--name) only apply to the listing
mode; --show-env only applies to the detail mode. Mixing them is
not an error — the unused one is silently ignored.

Examples:
  voodu get pod                                table of every pod
  voodu get pd  -k cronjob                     table, filtered to cronjobs
  voodu get pd  clowk-lp                       detail of every pod in scope
  voodu get pd  clowk-lp/web                   detail of all web replicas
  voodu get pd  clowk-lp-web.a3f9              detail of one replica
  voodu get pd  clowk-lp --show-env            scope sweep with env values`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return runGetPods(cmd, filters)
			}

			return runDescribePod(cmd, args[0], describeOptions{showEnv: showEnv})
		},
	}

	cmd.Flags().BoolVar(&showEnv, "show-env", false,
		"detail mode: reveal env var names and values (default: count only)")
	cmd.Flags().StringVarP(&filters.kind, "kind", "k", "",
		"list mode: filter by kind (deployment, job, cronjob)")
	cmd.Flags().StringVarP(&filters.scope, "scope", "s", "",
		"list mode: filter by scope")
	cmd.Flags().StringVarP(&filters.name, "name", "n", "",
		"list mode: filter by resource name")

	return cmd
}
