package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/git"
	"go.voodu.clowk.in/internal/remote"
	"go.voodu.clowk.in/internal/tarball"
)

// localOnlyCommands never forward over SSH — they manage client-side
// state (git remotes, local setup) or are purely informational.
var localOnlyCommands = map[string]bool{
	"version":    true,
	"help":       true,
	"--help":     true,
	"-h":         true,
	"--version":  true,
	"setup":      true,
	"remote":     true,
	"completion": true,
}

// maybeForwardRemote is the M5.5 dispatch hook. In client mode, if the
// invocation resolves to a configured remote, we SSH the argv to the
// server and exit with the remote's exit code. Otherwise we return and
// let Cobra handle the command locally.
//
// Returns (exitCode, forwarded). When forwarded==true the caller must
// os.Exit(code); Cobra should not run.
func maybeForwardRemote(root *cobra.Command, args []string) (int, bool) {
	if remote.IsServerMode() {
		return 0, false
	}

	if isLocalOnly(args) {
		return 0, false
	}

	remoteFlag, appFlag, forwardArgs := remote.ExtractFlags(args)

	// No remote-targeting signals at all → the user probably just
	// wants local execution (common for help flows, apply --dry-run,
	// offline development). We do not auto-forward in that case; only
	// an explicit --remote, an -a flag, or a configured default
	// "voodu" remote triggers the SSH path.
	if remoteFlag == "" && appFlag == "" && !hasDefaultRemote() {
		return 0, false
	}

	info, err := remote.Resolve(remoteFlag, appFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}

	if info == nil {
		return 0, false
	}

	identity := os.Getenv("VOODU_SSH_IDENTITY")

	// Manifest commands (apply/diff/delete) consume local files. We
	// parse them here — on the dev machine where ${VAR} can resolve —
	// and forward the result as JSON on stdin.
	stream, err := rewriteForStdinStream(forwardArgs)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}

	// Build-mode deployments need their source on the server before the
	// controller can reconcile. Default transport is a per-deployment
	// gzipped tar piped to `voodu receive-pack` over SSH — commitless,
	// respects per-deployment `path` (monorepo-friendly). Legacy `git
	// push` flow stays available via $VOODU_PUSH_MODE=git for ops who
	// want a trail in the server-side bare repo.
	if len(stream.buildModeDeploys) > 0 {
		if err := pushSourceForDeploys(info, identity, stream.buildModeDeploys); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
	}

	code, err := remote.Forward(info, stream.args, remote.ForwardOptions{
		Identity: identity,
		Stdin:    stream.stdin,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		if code == 0 {
			code = 1
		}
	}

	return code, true
}

// isLocalOnly returns true when the first positional token is a
// command that must run on the dev machine. Flags and their values
// are skipped so `-o json config set ...` still classifies as a
// forwardable "config" command.
func isLocalOnly(args []string) bool {
	skipValue := false

	for _, tok := range args {
		if skipValue {
			skipValue = false
			continue
		}

		if strings.HasPrefix(tok, "-") {
			if localOnlyCommands[tok] {
				return true
			}

			if !strings.Contains(tok, "=") && takesValue(tok) {
				skipValue = true
			}

			continue
		}

		return localOnlyCommands[tok]
	}

	// Bare `voodu` with no args: let Cobra show help locally.
	return true
}

// hasDefaultRemote says whether a git remote named "voodu" exists in
// the current repo. Used so CLI invocations inside a repo that was
// `voodu remote add`-ed auto-forward without needing -a / --remote.
func hasDefaultRemote() bool {
	_, err := remote.Lookup(remote.DefaultRemote)
	return err == nil
}

// sourcePushBranch picks the ref name to push to on the bare repo.
// Override via $VOODU_DEPLOY_BRANCH; defaults to "main" to match the
// bare repo bootstrap (`git init --bare --initial-branch=main`).
func sourcePushBranch() string {
	if b := os.Getenv("VOODU_DEPLOY_BRANCH"); b != "" {
		return b
	}

	return "main"
}

// pushSourceForDeploys transports each build-mode deployment's source
// to the server. Mode is picked once for the whole apply:
//
//   - $VOODU_PUSH_MODE=git → single `git push` of the current HEAD, one
//     bare repo update regardless of how many deployments are in the
//     manifest. Kept for ops who want audit trail.
//
//   - anything else (default) → one gzipped tar per deployment, piped
//     into `voodu receive-pack <scope>/<name>` over SSH. No git commit
//     required, respects each deployment's `path` as the build context.
func pushSourceForDeploys(info *remote.Info, identity string, deploys []buildModeDep) error {
	if os.Getenv("VOODU_PUSH_MODE") == "git" {
		return pushSourceViaGit(info)
	}

	for _, d := range deploys {
		if err := pushSourceViaTarball(info, identity, d); err != nil {
			return fmt.Errorf("receive-pack %s/%s: %w", d.Scope, d.Name, err)
		}
	}

	return nil
}

func pushSourceViaGit(info *remote.Info) error {
	branch := sourcePushBranch()

	fmt.Fprintf(os.Stderr, "-----> Pushing source via git to %s (branch: %s)\n", info.RemoteName, branch)

	return git.PushHead(context.Background(), info.RemoteName, branch)
}

// pushSourceViaTarball streams `path`'s contents as a gzipped tar into
// `voodu receive-pack <scope>/<name>` on the server. Uses an os.Pipe so
// the tar is produced lazily while SSH drains it — no temp file on the
// client, no full-archive buffered in memory.
func pushSourceViaTarball(info *remote.Info, identity string, d buildModeDep) error {
	fmt.Fprintf(os.Stderr, "-----> Shipping %s (scope: %s, context: %s)\n", d.Name, d.Scope, d.Path)

	pr, pw := io.Pipe()

	// Goroutine drives tar production; any error flows to the reader
	// side via CloseWithError and surfaces when SSH hits EOF.
	go func() {
		_, err := tarball.Stream(pw, d.Path, tarball.Options{
			MaxSize:  buildContextMaxSize(),
			Progress: os.Stderr,
		})

		// CloseWithError(nil) behaves like Close — no error propagates
		// on the happy path.
		_ = pw.CloseWithError(err)
	}()

	ref := d.Name
	if d.Scope != "" {
		ref = d.Scope + "/" + d.Name
	}

	args := []string{"receive-pack", ref}
	if os.Getenv("VOODU_FORCE_REBUILD") == "1" {
		args = append(args, "--force")
	}

	code, err := remote.Forward(info, args, remote.ForwardOptions{
		Identity: identity,
		Stdin:    pr,
	})
	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("remote exited %d", code)
	}

	return nil
}

// buildContextMaxSize returns the byte cap for an individual
// deployment's tarball. Default is 500 MB — generous enough for a
// typical monorepo subtree, tight enough to catch a missing
// .dockerignore before the upload saturates a home uplink. Overridable
// via $VOODU_BUILD_MAX_SIZE (bytes).
func buildContextMaxSize() int64 {
	if v := os.Getenv("VOODU_BUILD_MAX_SIZE"); v != "" {
		var n int64

		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}

	return 500 * 1024 * 1024
}
