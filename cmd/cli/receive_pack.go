package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/deploy"
)

// newReceivePackCmd is the server-side entry point for commitless
// deploys. The CLI on the developer's laptop pipes a gzipped tarball of
// the build context over SSH; this command reads it from stdin and runs
// the normal build/swap/container pipeline against it.
//
// Invoked as:
//
//	voodu receive-pack <scope>/<name>     # two-label form (preferred)
//	voodu receive-pack <name>             # legacy single-label, empty
//	                                        scope — AppID collapses to name
//
// The scope shapes the on-host identity: the release dir, env file,
// image tag, and container slots are all keyed by AppID(scope, name)
// (= "<scope>-<name>"), so two different scopes can deploy the same
// logical name without fighting over disk or docker. Hidden from
// `voodu --help` because normal users never invoke it by hand — the
// CLI drives it via SSH from `voodu apply`.
func newReceivePackCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:    "receive-pack <scope>/<name>",
		Short:  "Internal: ingest a tarball build context over SSH and deploy",
		Long: `Reads a gzipped tar of the build context from stdin, extracts it to
a content-addressed release directory, and runs the build/swap/container
pipeline. This is the commitless-deploy counterpart of git's receive-pack
— the CLI pipes a tar here over SSH; no git commit required.

This command is plumbing. The supported user entry point is:

    voodu apply -f voodu.hcl

which detects build-mode deployments and streams the tarball for you.

Dedup: the build-id is the sha256 of the tarball content. A second
invocation with an identical tree skips rebuild and just repoints
'current'. Pass --force to rebuild anyway.

Build configuration (lang, go_version, dockerfile, post_deploy, …)
is pulled from the local controller at VOODU_CONTROLLER_URL — the
source of truth is whatever 'voodu apply' persisted for this
deployment. When the controller has no manifest yet (first receive
for a brand-new app) the pipeline falls back to auto-detection.`,
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope, name, err := parseScopedRef(args[0])
			if err != nil {
				return err
			}

			spec, err := deploy.FetchSpec(controllerURL(cmd.Root()), scope, name)
			if err != nil {
				// Don't fall back silently — the operator needs to see
				// that the build-config source of truth is broken.
				return fmt.Errorf("fetch deployment spec from controller: %w", err)
			}

			return deploy.RunFromTarball(controller.AppID(scope, name), os.Stdin, deploy.Options{
				LogWriter: os.Stdout,
				Force:     force,
				Spec:      spec,
			})
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "rebuild even if a release with the same content hash already exists")

	return cmd
}

// parseScopedRef splits "<scope>/<name>" into its two parts. A bare
// "<name>" is accepted with an empty scope for legacy callers. Leading
// and trailing slashes are rejected because they usually signal a bug
// in the caller (accidental absolute path, empty template variable).
func parseScopedRef(ref string) (scope, name string, err error) {
	if ref == "" {
		return "", "", fmt.Errorf("receive-pack: ref is required")
	}

	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
		return "", "", fmt.Errorf("receive-pack: malformed ref %q", ref)
	}

	parts := strings.Split(ref, "/")

	switch len(parts) {
	case 1:
		return "", parts[0], nil
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("receive-pack: malformed ref %q", ref)
		}

		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("receive-pack: ref must be <scope>/<name>, got %q", ref)
	}
}
