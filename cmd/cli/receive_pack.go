package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

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
//	voodu receive-pack <name>             # legacy single-label, same
//	                                        semantics while we migrate
//
// The scope is accepted in the ref for forward compatibility and to
// mirror what the controller stores, but only the name is used to
// resolve filesystem paths today — per-scope app dirs would be a
// separate migration. Hidden from `voodu --help` because normal users
// never invoke it by hand (same posture as `voodu deploy`, the git
// post-receive plumbing).
func newReceivePackCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:    "receive-pack <scope>/<name>",
		Short:  "Internal: ingest a tarball build context over SSH and deploy",
		Long: `Reads a gzipped tar of the build context from stdin, extracts it to
a content-addressed release directory, and runs the build/swap/container
pipeline. This is the commitless-deploy counterpart of git's receive-pack
— the CLI pipes a tar to this command instead of firing git push.

This command is plumbing. The supported user entry point is:

    voodu apply -f voodu.hcl

which detects build-mode deployments and streams the tarball for you.

Dedup: the build-id is the sha256 of the tarball content. A second
invocation with an identical tree skips rebuild and just repoints
'current'. Pass --force to rebuild anyway.`,
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, name, err := parseScopedRef(args[0])
			if err != nil {
				return err
			}

			return deploy.RunFromTarball(name, os.Stdin, deploy.Options{
				LogWriter: os.Stdout,
				Force:     force,
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
