package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/git"
	"go.voodu.clowk.in/internal/remote"
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
	// controller can reconcile. Fire `git push` synchronously — the
	// post-receive hook on the bare repo is what turns the push into a
	// built image. Push output streams live so the user sees hook logs.
	if stream.needsSourcePush {
		branch := sourcePushBranch()

		fmt.Fprintf(os.Stderr, "-----> Pushing source to %s (branch: %s)\n", info.RemoteName, branch)

		if err := git.PushHead(context.Background(), info.RemoteName, branch); err != nil {
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
