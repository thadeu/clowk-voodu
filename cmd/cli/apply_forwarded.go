package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/remote"
)

// runApplyForwarded implements the two-phase apply flow used when the
// invocation is being forwarded to a remote voodu over SSH. The flow
// mirrors `terraform apply`:
//
//  1. SSH #1 — run `voodu diff -o json` on the server with the parsed
//     manifest as stdin. Capture the structured plan into a buffer.
//  2. Client — render the plan to the user's (real) terminal, then
//     prompt y/N unless --auto-approve / VOODU_AUTO_APPROVE is set.
//  3. SSH #2 — if approved, push build-mode tarballs (skipped on
//     phase 1, so canceled applies don't waste bandwidth) and run
//     `voodu apply` with the same manifest bytes.
//
// The prompt reads from the client's local stdin and writes to the
// client's local stderr. The server has no tty — we never allocate
// one through SSH, and wouldn't want to (see ssh.go: -tt suppressed
// when Stdin is a stream). Non-interactive callers must pass
// --auto-approve or the orchestrator refuses to proceed.
func runApplyForwarded(info *remote.Info, identity string, stream streamResult, flags applyClientFlags) (int, error) {
	// Drain stream.stdin once into memory so both SSH calls see the
	// same bytes. Manifest JSON is KB-scale; buffering is fine and
	// removes the "user edits the HCL between diff and apply" gotcha.
	body, err := io.ReadAll(stream.stdin)
	if err != nil {
		return 1, fmt.Errorf("buffer manifest: %w", err)
	}

	// Phase 0: preflight. A fresh VM with no voodu binary should
	// auto-bootstrap on the operator's first apply — they
	// configured SSH, wrote the HCL, installed the local CLI, and
	// the next thing they expect to type is `vd apply`. Forcing a
	// separate `vd remote setup` here would break that flow.
	//
	// Fast path: probe is a single SSH round-trip (~50ms warm),
	// returns immediately when the remote is healthy. Slow path:
	// installer runs over SSH, controller comes up, then the
	// usual diff/apply continues. Confirm gate matches the
	// destructive-op pattern (autoApprove / VOODU_AUTO_APPROVE).
	if err := ensureRemoteReady(info, identity, flags.autoApprove); err != nil {
		return 1, err
	}

	env := remoteEnv()

	// Phase 1: diff with -o json, capture stdout.
	diffArgs := rewriteApplyToDiffJSON(stream.args)

	// The SSH round-trip for the remote diff is the first — and
	// slowest — thing that happens after the user hits Enter: handshake
	// plus the server parsing the manifest and diffing it against live
	// controller state. On a cold connection this can read as 1–3s of
	// "is it frozen?" silence. Open a spinner up front so the terminal
	// shows immediate life, and commit it as a ✓ when the plan lands.
	checking := newProgressFilter(os.Stdout, flags.verbose)
	fmt.Fprintln(checking, "-----> Checking remote state...")

	var planBuf bytes.Buffer

	code, err := remote.Forward(info, diffArgs, remote.ForwardOptions{
		Identity: identity,
		Stdin:    bytes.NewReader(body),
		Stdout:   &planBuf,
		Env:      env,
	})
	if err != nil {
		// Dirty close clears the spinner without an inaccurate ✓ — the
		// error message prints right after on its own row.
		_ = checking.Close()
		return code, fmt.Errorf("remote diff: %w", err)
	}

	if code != 0 {
		// Same rationale: the server already wrote to stderr, committing
		// a ✓ over a failed diff would be lying.
		_ = checking.Close()
		// Server already wrote its error to stderr (we pass-through
		// stderr always). Propagate the exit code so CI sees it.
		return code, nil
	}

	checking.CommitStep()

	var plan diffResponse
	if err := json.Unmarshal(planBuf.Bytes(), &plan); err != nil {
		// Surface the raw payload so a protocol mismatch is debuggable.
		return 1, fmt.Errorf("decode remote diff: %w\n%s", err, planBuf.String())
	}

	hasSpecChanges := planChangeCount(plan) > 0
	needsSourcePush := len(stream.buildModeDeploys) > 0

	// Three cases worth distinguishing:
	//
	//  1. No spec changes AND no build-mode deploys → nothing to do,
	//     short-circuit with the terraform-ish "No changes." line.
	//
	//  2. Spec changes (regardless of build-mode) → render the diff,
	//     prompt the user, then push source + apply on approval.
	//
	//  3. No spec changes BUT build-mode deploys present → this is the
	//     "commitless re-deploy" workflow that is voodu's whole point:
	//     user iterates on source, re-applies with the same HCL, server
	//     content-addresses the new tarball, rebuilds if the tree hash
	//     differs, redeploys. Skipping this case would silently break
	//     the core UX. We proceed without prompting — there's nothing
	//     to show in a diff anyway — and print a short banner so the
	//     user sees *why* we're still doing work.
	if !hasSpecChanges && !needsSourcePush {
		fmt.Fprintln(os.Stdout, "No changes. Nothing to apply.")
		return 0, nil
	}

	if hasSpecChanges {
		// Render to the real local stdout (tty) — lipgloss picks the
		// right color profile here because the writer *is* the user's
		// terminal.
		palette := newDiffPalette(os.Stdout)
		added, modified := renderApplyPlan(os.Stdout, plan, palette)
		renderPrunePlan(os.Stdout, plan.Data.Pruned, palette)
		fmt.Fprintf(os.Stdout, "\n%s\n", diffSummary(added, modified, len(plan.Data.Pruned)))

		if !flags.autoApprove && !envAutoApprove() {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return 1, errors.New("refusing to apply in non-interactive mode without --auto-approve (or VOODU_AUTO_APPROVE=1)")
			}

			ok, err := promptConfirm(os.Stdin, os.Stderr)
			if err != nil {
				return 1, err
			}

			if !ok {
				fmt.Fprintln(os.Stderr, "Apply canceled.")
				return 0, nil
			}
		}
	} else {
		// Source-only re-apply: no spec delta, but there's code to
		// push. No prompt — nothing to confirm, and asking y/N over a
		// blank diff is annoying. The server will content-address the
		// tarball and skip the rebuild if the tree is unchanged.
		word := "deployment"
		if len(stream.buildModeDeploys) > 1 {
			word = "deployments"
		}

		// Prefix with a green ✓ so the line reads as a finished check in
		// the same visual language as the ✓ build steps below — but
		// keep the `----->` phase marker so it still signals "this is a
		// server-style banner" in the scrollback. The check conveys "we
		// looked, nothing to apply"; the banner body explains why we're
		// still doing work (re-pushing source for build-mode deploys).
		fmt.Fprintf(os.Stdout, "\x1b[32m✓\x1b[0m No spec changes. Re-pushing source for %d build-mode %s.\n",
			len(stream.buildModeDeploys), word)
	}

	// Phase 2: push source tarballs for build-mode deployments *now*,
	// not before the diff. If the user cancels at the prompt (in case
	// 2 above) we never reach this point — that's the whole win of the
	// split flow. A blank line both before and after the build block
	// separates it visually from the surrounding narrative ("No spec
	// changes…" header, "deployment/X applied" footer) so the user
	// reads three distinct sections: intent, build, result.
	if needsSourcePush {
		fmt.Fprintln(os.Stdout)

		if err := pushSourceForDeploys(info, identity, stream.buildModeDeploys, flags.force, flags.verbose); err != nil {
			return 1, err
		}

		fmt.Fprintln(os.Stdout)
	}

	// Phase 3: the actual `voodu apply` on the server. Two possible
	// wire formats:
	//
	//   - Legacy: one plain status line per manifest, e.g.
	//     "deployment/softphone/web applied". applyResultFilter
	//     styles each with a green ✓.
	//
	//   - NDJSON (ndjson/1): a hello frame first, then one result
	//     event per manifest. eventRenderer styles them identically
	//     to the legacy output but via typed decoding.
	//
	// negotiatingWriter sniffs the first server line and routes to
	// whichever filter matches. --verbose bypasses both renderers
	// (raw stream pass-through), which is what we want for debugging
	// whichever side — NDJSON frames are human-readable JSON, and
	// the legacy format is already plain text.
	legacy := newApplyResultFilter(os.Stdout, flags.verbose)
	nd := newEventRenderer(os.Stdout, flags.verbose)
	resultFilter := newNegotiatingWriter(legacy, nd)

	code, err = remote.Forward(info, stream.args, remote.ForwardOptions{
		Identity: identity,
		Stdin:    bytes.NewReader(body),
		Stdout:   resultFilter,
		Env:      env,
	})

	_ = resultFilter.Close()

	return code, err
}

// rewriteApplyToDiffJSON turns the forwarded `apply ...` argv into its
// `diff ...` equivalent for phase 1. We always force `-o json` so the
// server emits the machine-readable plan regardless of whatever the
// user typed at top level. Apply-only flags (--auto-approve, --force,
// --verbose) are stripped — harmless on diff, but leaves the SSH
// command line clean for ssh -v / logs.
//
// `--detailed-exitcode` is intentionally passed through: if the
// server's diff decides there are no changes it'll exit 0, which is
// what we want to detect "nothing to do" before even prompting.
func rewriteApplyToDiffJSON(args []string) []string {
	out := make([]string, 0, len(args)+2)

	for _, tok := range args {
		switch tok {
		case "apply":
			out = append(out, "diff")
		case "-y", "--auto-approve", "--force", "-v", "--verbose":
			// Apply-only concern. Drop.
		default:
			out = append(out, tok)
		}
	}

	// Force JSON regardless of any -o the user passed. If they asked
	// for `apply -o yaml`, phase 1 still needs JSON for us to parse;
	// the final apply response inherits their chosen format downstream.
	out = stripOutputFlag(out)
	out = append(out, "-o", "json")

	return out
}

// stripOutputFlag removes any `-o VAL` / `--output VAL` / `-o=VAL` /
// `--output=VAL` pair from argv. Called by rewriteApplyToDiffJSON so
// our forced "-o json" isn't duplicated / overridden.
func stripOutputFlag(args []string) []string {
	out := make([]string, 0, len(args))

	i := 0
	for i < len(args) {
		tok := args[i]

		switch {
		case tok == "-o" || tok == "--output":
			// Skip the flag and its value (if present).
			if i+1 < len(args) {
				i += 2
				continue
			}

			i++

		case strings.HasPrefix(tok, "-o=") || strings.HasPrefix(tok, "--output="):
			i++

		default:
			out = append(out, tok)
			i++
		}
	}

	return out
}

// planChangeCount replays the "is there anything to apply?" decision
// the text renderer makes, so we can short-circuit to "No changes."
// before rendering. Kept local so the orchestrator doesn't depend on
// the renderer's side-effect counts.
func planChangeCount(plan diffResponse) int {
	changes := len(plan.Data.Pruned)

	for i, desired := range plan.Data.Applied {
		if desired == nil {
			continue
		}

		var current *controller.Manifest

		if i < len(plan.Data.Current) {
			current = plan.Data.Current[i]
		}

		if current == nil || len(diffSpec(desired.Spec, current.Spec)) > 0 {
			changes++
		}
	}

	return changes
}

// envAutoApprove is the CI escape hatch. Accept "1", "true", "yes"
// (case-insensitive) — same permissive set Docker / Kubernetes tools
// use. Anything else (including unset / empty) = require the prompt.
func envAutoApprove() bool {
	v := strings.TrimSpace(os.Getenv("VOODU_AUTO_APPROVE"))

	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	}

	return false
}

// promptConfirm reads a single line from `in`, writes the prompt to
// `out`, and returns true iff the user typed y / yes (case
// insensitive). Anything else — including empty input (just Enter) —
// is treated as No. The prompt is intentionally on stderr so stdout
// stays piping-clean for the diff content printed just above.
//
// The leading `\n` before the prompt and the trailing `\n` after the
// user's input bracket the question with blank rows so it doesn't
// read as glued to the diff above or to the build spinner / "Apply
// canceled." line that runs immediately after. Symmetric to the blank
// lines surrounding the build block in runApplyForwarded.
func promptConfirm(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprint(out, "\nApply these changes? [y/N]: ")

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}

	fmt.Fprintln(out)

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}

	return false, nil
}
