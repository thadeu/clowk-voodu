package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/term"

	"go.voodu.clowk.in/internal/remote"
)

// runDeleteForwarded is the client-side orchestrator for `voodu
// delete` over SSH. It mirrors runApplyForwarded's "show plan,
// prompt locally, then forward" pattern but is much simpler — no
// diff round-trip is needed because the plan IS the loaded manifests
// list, and there's no source push to schedule.
//
// Why a client-side orchestrator at all: when forwarding, we use the
// SSH stdin to ship the manifest JSON to the server. That stream is
// fully consumed by the time the server's runDelete reaches its
// confirmation prompt — there's no PTY-side input left to read y/N
// from. So the prompt has to happen here, before SSH starts, on the
// real local terminal.
//
// On approval we inject -y into the forwarded argv so the server's
// runDelete skips its own prompt (which would deadlock on an empty
// stdin). On cancel we exit 0 without ever reaching SSH — same
// "no destructive action" guarantee runApplyForwarded provides.
func runDeleteForwarded(info *remote.Info, identity string, stream streamResult, flags deleteClientFlags) (int, error) {
	if len(stream.manifests) == 0 {
		// Should not happen — rewriteForStdinStream returns the as-is
		// path when there are no -f files, and that path skips this
		// orchestrator. Guard anyway so a future refactor doesn't make
		// us silently delete based on argv we didn't parse.
		return 1, errors.New("delete: no manifests to plan against")
	}

	if flags.dryRun {
		palette := newDiffPalette(os.Stdout)

		renderDeletePlan(os.Stdout, stream.manifests, palette)

		fmt.Fprintln(os.Stdout, "\nDry-run: no DELETE issued.")

		return 0, nil
	}

	palette := newDiffPalette(os.Stdout)

	renderDeletePlan(os.Stdout, stream.manifests, palette)

	if !flags.autoApprove && !envAutoApprove() {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return 1, errors.New("refusing to delete in non-interactive mode without --auto-approve (or VOODU_AUTO_APPROVE=1)")
		}

		ok, err := promptDeleteConfirm(os.Stdin, os.Stderr)
		if err != nil {
			return 1, err
		}

		if !ok {
			fmt.Fprintln(os.Stderr, "Delete canceled.")
			return 0, nil
		}
	}

	// Symmetric with runApplyForwarded: forward stream.args verbatim.
	// The server-side runDelete is a no-prompt path (its --auto-approve
	// is a cobra surface no-op, same shape as runApply ignores its own
	// --auto-approve), so we don't need to inject any signal — pre-
	// approval is implicit in "we got here at all".
	code, err := remote.Forward(info, stream.args, remote.ForwardOptions{
		Identity: identity,
		Stdin:    stream.stdin,
		Env:      remoteEnv(),
	})

	return code, err
}

// deleteClientFlags is the small bag of delete-only flags the client
// orchestrator consumes before forwarding. Same role as
// applyClientFlags — kept separate because the meaningful bools are
// different (delete has --dry-run, apply has --force/--verbose) and
// merging them would conflate the two surfaces.
type deleteClientFlags struct {
	autoApprove bool // -y / --auto-approve: skip the y/N prompt
	dryRun      bool // --dry-run: render plan and exit, no DELETE
}

// extractDeleteClientFlags walks argv once, pulling out the bool
// flags that drive client-side control flow. Returns the parsed
// flags and a copy of argv with those tokens removed. The cleaned
// argv is what eventually ships over SSH; the orchestrator re-injects
// -y after a successful prompt so the server's runDelete skips its
// own prompt path.
func extractDeleteClientFlags(args []string) (deleteClientFlags, []string) {
	var f deleteClientFlags

	out := make([]string, 0, len(args))

	for _, tok := range args {
		switch tok {
		case "-y", "--auto-approve":
			f.autoApprove = true
		case "--dry-run":
			f.dryRun = true
		default:
			out = append(out, tok)
		}
	}

	return f, out
}

// isDeleteCommand returns true when the first positional token in
// argv is "delete". Mirrors isApplyCommand so the forwarder router
// can pick the orchestrator without a second walker.
func isDeleteCommand(args []string) bool {
	idx := findPrimaryCommand(args)
	if idx < 0 {
		return false
	}

	return args[idx] == "delete"
}

