package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
)

// newRunCmd is the unified one-shot verb. Routes intent based on
// what the operator typed:
//
//	vd run <ref>             trigger a declared job/cronjob (no extra cmd)
//	vd run <ref> <cmd...>    exec <cmd> in a running container of <ref>
//
// One verb, many shapes — operators don't need to remember whether
// the resource is a job, cronjob, deployment, or a specific container.
// Voodu inspects the ref and dispatches:
//
//   - Slash + no cmd: look up the manifest. Job → POST /jobs/run.
//     Cronjob → POST /cronjobs/run. Deployment → error (no implicit
//     command; pass one to exec a shell or specific tool).
//
//   - Any ref + cmd: defer to the exec machinery. The same scope/name
//     auto-pick (running > stopped, latest first) used by `vd exec`
//     applies; container-name refs hit the named replica directly.
//
// Replaces the old `vd run job <ref>` / `vd run cronjob <ref>`
// subcommands — those split job/cronjob/deployment by kind, which
// the operator shouldn't need to recall just to fire a one-shot.
func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <ref> [cmd...]",
		Short: "Trigger a declared job/cronjob, or exec a one-shot command in a container",
		Long: `Unified one-shot verb. Ref resolution and dispatch:

  vd run clowk-lp/migrate
      Trigger a declared job once. The job's manifest is fetched
      from the controller, a fresh container is spawned with the
      spec's image / command / env, and the call blocks until the
      container exits (run id, exit code, duration in the response).

  vd run clowk-lp/crawler1
      Force-tick a declared cronjob, bypassing the schedule. Same
      run record + history mechanics as a normal scheduled tick.

  vd run clowk-lp/web rails db:migrate
      Exec 'rails db:migrate' inside a running 'web' replica. The
      ref auto-resolves to the best replica (running > stopped,
      latest first) — same shape as 'vd exec'. Useful for one-off
      maintenance commands without declaring a separate job.

  vd run clowk-lp-web.aaaa bash
      Exec 'bash' in this specific container.

Without a command, vd run only proceeds when the ref resolves to a
declared job or cronjob — those are the only resources with a
"trigger me once" meaning. Deployments need an explicit command.

Distinct from 'vd apply' (sets desired state) and 'vd exec' (enters
an existing container directly). 'vd run' is the one-shot verb.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnified(cmd, args[0], args[1:])
		},
	}

	return cmd
}

// runUnified is the dispatch logic behind `vd run`. Two paths,
// chosen by whether the operator passed a command after the ref.
func runUnified(cmd *cobra.Command, ref string, command []string) error {
	ref = strings.TrimSpace(ref)

	if ref == "" {
		return fmt.Errorf("run ref is empty")
	}

	if len(command) > 0 {
		// Operator gave a command → always exec into a live
		// container. Reuse the existing exec machinery so ref
		// resolution, TTY auto-detect, raw mode, env-forwarding,
		// and replica auto-pick all stay in one place. Passing
		// false for tty/stdin lets runExec's own auto-default
		// inspect the local terminal (the run command doesn't
		// expose --tty / --stdin flags).
		return runExec(cmd, ref, command,
			"" /* container override */, false /* tty */, false /* stdinFlag */,
			"" /* workdir */, "" /* user */, nil /* envs */)
	}

	// No command: this only makes sense for jobs and cronjobs.
	scope, name := splitJobRef(ref)

	if name == "" {
		return fmt.Errorf("run ref %q is empty or invalid", ref)
	}

	// Probe the manifest store for a matching job or cronjob.
	// Bare-name refs (no slash) auto-resolve scope server-side via
	// fetchRemote's existing logic.
	if m, err := fetchRemote(cmd.Root(), controller.KindJob, scope, name); err == nil && m != nil {
		return runRunJob(cmd, ref)
	}

	if m, err := fetchRemote(cmd.Root(), controller.KindCronJob, scope, name); err == nil && m != nil {
		return runRunCronJob(cmd, ref)
	}

	return fmt.Errorf(
		"ref %q does not name a declared job or cronjob;\n"+
			"  to run a one-shot in an existing container, append the command:\n"+
			"      vd run %s <CMD ARGS...>",
		ref, ref,
	)
}
