package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

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
	// Two shapes: file-mode (with manifests parsed locally) and
	// scope-wipe (positional <scope>, no manifests). The latter
	// hits a different server endpoint (DELETE /scope) — argv is
	// forwarded verbatim and the server's runScopeWipe handles it.
	if len(stream.manifests) == 0 {
		scope := positionalScope(stream.args)

		if scope == "" {
			// Neither manifests nor a positional scope. The CLI's
			// cobra layer should have caught this earlier ("no
			// manifests found" / "ref required"); if we're here a
			// future refactor punched a hole.
			return 1, errors.New("delete: no manifests and no scope to plan against")
		}

		return runScopeWipeForwarded(info, identity, stream, flags, scope)
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
	prune       bool // --prune: track for fail-fast on scope-wipe; passed through so server applies it too
}

// extractDeleteClientFlags walks argv once, pulling out the bool
// flags that drive client-side control flow. Returns the parsed
// flags and a copy of argv with those tokens removed. The cleaned
// argv is what eventually ships over SSH; the orchestrator re-injects
// -y after a successful prompt so the server's runDelete skips its
// own prompt path.
//
// --prune is the exception: it's both client-checked (fail-fast on
// scope-wipe missing the flag) AND server-needed (server gates
// destructive ops on its presence). So we record it in flags AND
// keep it in the cleaned argv.
func extractDeleteClientFlags(args []string) (deleteClientFlags, []string) {
	var f deleteClientFlags

	out := make([]string, 0, len(args))

	for _, tok := range args {
		switch tok {
		case "-y", "--auto-approve":
			f.autoApprove = true
		case "--dry-run":
			f.dryRun = true
		case "--prune":
			f.prune = true
			out = append(out, tok)
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

// positionalScope returns the bare positional token after `delete`
// in argv (the scope-wipe target), or "" when there isn't one.
// Skips flags + their values, so `delete --prune clowk-lp` and
// `delete clowk-lp --prune --remote prod` both resolve to "clowk-lp".
//
// Used by the forwarder to detect the scope-wipe shape without
// re-running cobra parsing client-side. The server's runScopeWipe
// re-validates the scope before doing anything destructive.
func positionalScope(args []string) string {
	idx := findPrimaryCommand(args)
	if idx < 0 || args[idx] != "delete" {
		return ""
	}

	skipNext := false

	for j := idx + 1; j < len(args); j++ {
		tok := args[j]

		if skipNext {
			skipNext = false
			continue
		}

		if strings.HasPrefix(tok, "-") {
			if !strings.Contains(tok, "=") && deleteFlagTakesValue(tok) {
				skipNext = true
			}

			continue
		}

		return tok
	}

	return ""
}

// deleteFlagTakesValue is the value-taking subset of `vd delete`
// flags. Local subset of takesValue (in dispatch.go) because we
// only care about the delete surface — kept tight so a future
// flag addition there doesn't silently break this scanner.
func deleteFlagTakesValue(flag string) bool {
	switch flag {
	case "-f", "--file", "-r", "--remote", "-o", "--output", "--format":
		return true
	}

	return false
}

// runScopeWipeForwarded is the SSH path for `vd delete <scope>
// --prune --remote X`. Renders an explicit "you are about to
// destroy scope X" preview locally so the destructive prompt
// happens on the operator's terminal (the SSH stdin is unused —
// no manifests to ship — but cobra's prompt still wouldn't reach
// the operator from the server side). On approval the argv
// forwards verbatim and the server's runScopeWipe takes over.
func runScopeWipeForwarded(info *remote.Info, identity string, stream streamResult, flags deleteClientFlags, scope string) (int, error) {
	// Fail-fast before the prompt: the server's runScopeWipe also
	// gates on --prune, but the operator should hear "you forgot
	// --prune" before being asked to confirm — not after answering
	// y to a wipe that the server then refuses.
	if !flags.prune {
		return 1, fmt.Errorf("delete <scope> requires --prune (this destroys every manifest, config, and on-disk state in the scope)")
	}

	if flags.dryRun {
		fmt.Fprintf(os.Stdout, "Would wipe scope %q (every manifest, scope config, per-app dirs).\nDry-run: no DELETE issued.\n", scope)
		return 0, nil
	}

	palette := newDiffPalette(os.Stdout)

	fmt.Fprintf(os.Stdout, "Will wipe scope %s — every manifest, scope-level config, and per-app on-disk state.\n\n", palette.Del(scope))

	if !flags.autoApprove && !envAutoApprove() {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return 1, errors.New("refusing to wipe scope in non-interactive mode without --auto-approve (or VOODU_AUTO_APPROVE=1)")
		}

		ok, err := promptDeleteConfirm(os.Stdin, os.Stderr)
		if err != nil {
			return 1, err
		}

		if !ok {
			fmt.Fprintln(os.Stderr, "Scope wipe canceled.")
			return 0, nil
		}
	}

	code, err := remote.Forward(info, stream.args, remote.ForwardOptions{
		Identity: identity,
		Stdin:    stream.stdin,
		Env:      remoteEnv(),
	})

	return code, err
}

