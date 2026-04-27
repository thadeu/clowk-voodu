package main

import (
	"github.com/spf13/cobra"
)

// newGetPodCmd is the singular counterpart to `voodu get pods`. It's
// a thin alias over `voodu describe pod` — operators reach for `get`
// when they want to inspect, and the singular form naturally means
// "the rich detail of one container" rather than "a one-row table".
//
// Aliases mirror the describe-side shorthand: "pd" stays consistent
// with `voodu desc pd`. Any flag describe-pod cares about (currently
// --show-env) is forwarded through unchanged.
//
// Why route to describe instead of duplicating the inspect logic:
// the wire shape (`GET /pods/{name}` → PodDetail) and the renderer
// (renderPodDetail) are already paid for. A second entry point would
// drift over time — flags would diverge, the env-hiding default
// could regress on one path and not the other. One implementation,
// two doorways.
func newGetPodCmd() *cobra.Command {
	var showEnv bool

	cmd := &cobra.Command{
		Use:     "pod <ref>",
		Aliases: []string{"pd"},
		Short:   "Show detailed state of one voodu-managed container (alias of 'describe pod')",
		Long: `pod inspects a single voodu-managed container — the rich-detail
counterpart to 'get pods'. It is exactly equivalent to 'describe pod
<ref>' / 'desc pd <ref>'; use whichever feels natural.

<ref> is the container name as it appears in 'voodu get pods'
(e.g. test-web.a3f9). Pods don't share the kind/scope/name shape
because more than one replica can match the same identity, so the
ref is always the docker container name.

Env vars are listed by count only (values and names hidden) by
default, matching 'describe pod'. Pass --show-env to reveal — useful
for debugging, risky on a screen-share or recorded session.

Examples:
  voodu get pod test-web.a3f9
  voodu get pd  test-web.a3f9
  voodu get pd  test-web.a3f9 --show-env`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDescribePod(cmd, args[0], describeOptions{showEnv: showEnv})
		},
	}

	cmd.Flags().BoolVar(&showEnv, "show-env", false,
		"reveal env var names and values (default: count only)")

	return cmd
}
