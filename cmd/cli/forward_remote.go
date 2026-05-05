package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"go.voodu.clowk.in/internal/progress"
	"go.voodu.clowk.in/internal/remote"
	"go.voodu.clowk.in/internal/tarball"
)

// localOnlyCommands never forward over SSH — they manage client-side
// state (git remotes, --version) or are purely informational.
var localOnlyCommands = map[string]bool{
	"version":   true,
	"help":      true,
	"--help":    true,
	"-h":        true,
	"--version": true,
	"remote":    true,
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

	remoteFlag, forwardArgs := remote.ExtractFlags(args)

	// No remote-targeting signal at all → the user probably just wants
	// local execution (common for help flows, apply --dry-run, offline
	// development). We do not auto-forward in that case; only an
	// explicit --remote or a configured default "voodu" remote triggers
	// the SSH path.
	if remoteFlag == "" && !hasDefaultRemote() {
		return 0, false
	}

	info, err := remote.Resolve(remoteFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}

	if info == nil {
		return 0, false
	}

	// Identity precedence:
	//   1. VOODU_SSH_IDENTITY env (explicit override, highest priority)
	//   2. Identity embedded in the remote URL (user@host:/path/to/key.pem)
	//   3. ssh's defaults (~/.ssh/config, agent, ~/.ssh/id_rsa, ...)
	// EC2-style .pem keys live in (2) by far most often — a pem
	// per VPS that the operator set in `voodu remote add`.
	identity := os.Getenv("VOODU_SSH_IDENTITY")
	if identity == "" {
		identity = info.Identity
	}

	// Manifest commands (apply/diff/delete) consume local files. We
	// parse them here — on the dev machine where ${VAR} can resolve —
	// and forward the result as JSON on stdin.
	stream, err := rewriteForStdinStream(forwardArgs)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1, true
	}

	// `voodu apply` takes the two-phase orchestrated flow: diff, prompt,
	// apply. The orchestrator handles its own tarball push *after* the
	// prompt so a canceled apply doesn't upload source for nothing.
	// See runApplyForwarded for the full dance.
	if isApplyCommand(stream.args) {
		flags, cleanedArgs := extractApplyClientFlags(stream.args)
		stream.args = cleanedArgs

		code, err := runApplyForwarded(info, identity, stream, flags)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)

			if code == 0 {
				code = 1
			}
		}

		return code, true
	}

	// `voodu delete` is destructive — render the plan locally and
	// prompt y/N on the user's actual terminal before any SSH. The
	// orchestrator injects -y into the forwarded argv on approval so
	// the server's runDelete doesn't try to prompt over a stdin that
	// is already carrying the manifest stream.
	if isDeleteCommand(stream.args) {
		flags, cleanedArgs := extractDeleteClientFlags(stream.args)
		stream.args = cleanedArgs

		code, err := runDeleteForwarded(info, identity, stream, flags)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)

			if code == 0 {
				code = 1
			}
		}

		return code, true
	}

	// Build-mode deployments need their source on the server before
	// the controller can reconcile. We stream a gzipped tar per
	// deployment into `voodu receive-pack <scope>/<name>` over SSH —
	// commitless, per-deployment `path` as the build context, content-
	// addressable so identical trees skip the rebuild. Force rebuild
	// only reachable here via the env var — the --force flag lives on
	// `apply` and is routed through runApplyForwarded above.
	if len(stream.buildModeDeploys) > 0 {
		if err := pushSourceForDeploys(info, identity, stream.buildModeDeploys, false, false); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
	}

	code, err := remote.Forward(info, stream.args, remote.ForwardOptions{
		Identity: identity,
		Stdin:    stream.stdin,
		Env:      remoteEnv(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		if code == 0 {
			code = 1
		}
	}

	return code, true
}

// isApplyCommand returns true when the first positional token in argv
// is "apply". Shared by the forwarder router to pick the two-phase
// orchestrator. Uses findPrimaryCommand (forward_stdin.go) so flag
// values like `-o json apply` classify correctly.
func isApplyCommand(args []string) bool {
	idx := findPrimaryCommand(args)
	if idx < 0 {
		return false
	}

	return args[idx] == "apply"
}

// applyClientFlags is the small bag of apply-only flags that the
// client *consumes* before forwarding. The server ignores all of them
// — they drive client-side control flow (the y/N prompt, the force
// bit on the receive-pack push, the spinner/verbose presentation
// mode) — so we pull them out once here and pass the parsed result
// to the orchestrator, keeping the forwarded SSH argv tidy.
type applyClientFlags struct {
	autoApprove bool // -y / --auto-approve: skip the interactive prompt
	force       bool // --force: rebuild build-mode deploys on hash hit
	verbose     bool // -v / --verbose: disable spinner, passthrough raw build output
}

// extractApplyClientFlags walks argv once, pulling out the bool flags
// that are meaningful only to the client-side orchestrator. Returns
// the parsed flags struct and a copy of argv with those tokens
// removed. Cosmetic for the server (its apply would ignore them
// anyway), but keeps logs and `ssh -v` output readable. Any future
// client-only apply flag should join this function, not spawn a
// second walker.
func extractApplyClientFlags(args []string) (applyClientFlags, []string) {
	var f applyClientFlags

	out := make([]string, 0, len(args))

	for _, tok := range args {
		switch tok {
		case "-y", "--auto-approve":
			f.autoApprove = true
		case "--force":
			f.force = true
		case "-v", "--verbose":
			f.verbose = true
		default:
			out = append(out, tok)
		}
	}

	return f, out
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

// pushSourceForDeploys transports each build-mode deployment's source
// to the server as a gzipped tar piped into `voodu receive-pack
// <scope>/<name>` over SSH. One stream per deployment so each respects
// its own `path` (the build context). Content-addressable on the far
// side: identical trees skip the rebuild entirely.
//
// `force` requests a rebuild even when the content hash matches an
// existing release. Useful for non-deterministic build caches or when
// validating CI image changes. VOODU_FORCE_REBUILD=1 is still honoured
// as an env-var escape hatch; either path lights the same --force
// token on the remote receive-pack invocation.
//
// `verbose` disables the spinner that collapses docker buildx output
// in a TTY. Off by default — docker buildx dumps 80% chatter 20%
// signal, and the user already sees `-----> Building release...` →
// `✓ Built X in Ns`. Pass -v on apply to debug a failed build.
func pushSourceForDeploys(info *remote.Info, identity string, deploys []buildModeDep, force, verbose bool) error {
	for _, d := range deploys {
		if err := pushSourceViaTarball(info, identity, d, force, verbose); err != nil {
			return fmt.Errorf("receive-pack %s/%s: %w", d.Scope, d.Name, err)
		}
	}

	return nil
}

// pushSourceViaTarball streams `path`'s contents as a gzipped tar into
// `voodu receive-pack <scope>/<name>` on the server. Uses an os.Pipe so
// the tar is produced lazily while SSH drains it — no temp file on the
// client, no full-archive buffered in memory.
//
// `force` (or VOODU_FORCE_REBUILD=1 in the env) appends --force to the
// remote receive-pack argv, asking the server to rebuild the image
// even when the content-addressed release already exists.
//
// `verbose` bypasses the progressFilter so the raw docker buildx
// stream reaches the user's terminal untouched. Default (verbose
// false) collapses the noisy middle of the build into a spinner.
func pushSourceViaTarball(info *remote.Info, identity string, d buildModeDep, force, verbose bool) error {
	// Record the deploy name so both renderers (legacy progressFilter
	// and NDJSON eventRenderer) produce the right `✓ Built <tag> in Ns`
	// summary regardless of which path the negotiation picks.
	rememberShippedTag(d.Name)

	// The Shipping banner is a client-only note (the server has not
	// even been contacted yet). Print it straight to stdout, outside
	// any filter — otherwise it would race with the server's first
	// line for the negotiator's "line one" peek.
	//
	// We commit it as a green ✓ right away instead of opening a spinner:
	// from the client's point of view Shipping is a one-shot act ("we
	// kicked off the tar stream"), and visually it now aligns with the
	// ✓ cascade that follows (Receiving, Creating, Building, Built).
	// Printing `----->` here would leave an orphan banner without the
	// leading checkmark, which reads as "this line didn't finish."
	fmt.Fprintf(os.Stdout, "\x1b[32m✓\x1b[0m Shipping %s (scope: %s, context: %s)\n", d.Name, d.Scope, d.Path)

	// Two renderers are pre-built and handed to a negotiatingWriter
	// that picks between them based on the server's first stdout line.
	// NDJSON-speaking servers emit a hello frame and eventRenderer
	// takes over; legacy servers emit `-----> Receiving ...` banners
	// and progressFilter handles them. The client is therefore
	// version-agnostic: it speaks the latest protocol, falls back
	// gracefully otherwise.
	legacy := newProgressFilter(os.Stdout, verbose)
	nd := newEventRenderer(os.Stdout, verbose)
	filter := newNegotiatingWriter(legacy, nd)

	pr, pw := io.Pipe()

	// The tar builder's own progress log ("tarball: using .dockerignore",
	// "tarball: N files, X KB") is useful when debugging a slow or
	// oversized upload, but it bypasses the progressFilter (it goes
	// direct to stderr, not through the SSH pipe), so leaving it on in
	// non-verbose mode would clutter the curated output. Attach it only
	// when the user asked for --verbose; the spinner covers the user-
	// facing feedback otherwise.
	var tarProgress io.Writer
	if verbose {
		tarProgress = os.Stderr
	}

	// Goroutine drives tar production; any error flows to the reader
	// side via CloseWithError and surfaces when SSH hits EOF.
	go func() {
		_, err := tarball.Stream(pw, d.Path, tarball.Options{
			MaxSize:  buildContextMaxSize(),
			Progress: tarProgress,
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
	if force || os.Getenv("VOODU_FORCE_REBUILD") == "1" {
		args = append(args, "--force")
	}

	// Both streams feed the filter. docker buildx writes its progress
	// stream to stderr; ignoring stderr here would let the #N noise
	// bypass the spinner entirely (proven during first live test).
	// The filter's mutex keeps concurrent writes from interleaving
	// mid-line.
	code, err := remote.Forward(info, args, remote.ForwardOptions{
		Identity: identity,
		Stdin:    pr,
		Stdout:   filter,
		Stderr:   filter,
	})

	// Always flush the filter, even on error — otherwise a build that
	// crashed mid-stream would leave a dangling spinner on the user's
	// terminal. Close is a no-op when verbose/non-TTY.
	_ = filter.Close()

	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("remote exited %d", code)
	}

	return nil
}

// remoteEnv builds the env map inlined into the SSH command so the
// remote voodu can emit colorized output and speak the NDJSON
// progress protocol. Why this exists: the server process writes to a
// pipe (sshd), so its lipgloss renderer would otherwise pick the
// no-color profile. The user's *local* stdout is the real tty — we
// detect here, propagate FORCE_COLOR=1 across the wire, and let the
// bytes stream back to the actual terminal intact.
//
// Precedence mirrors no-color.org for color flags:
//   - NO_COLOR (non-empty local) → forwarded as-is, disables everything
//   - FORCE_COLOR (non-empty local) → forwarded as-is, user's override wins
//   - Else if local stdout is a tty → synthesize FORCE_COLOR=1
//   - Else → no color flag set, remote stays plain (pipes, CI, redirects)
//
// VOODU_PROTOCOL is always set: the client speaks the latest wire
// version, and servers that don't know it will silently ignore the
// env var and fall back to their legacy text format. The client's
// negotiatingWriter sniffs the server's first stdout line to decide
// which renderer to use — if no hello arrives, the legacy
// progressFilter takes over transparently.
func remoteEnv() map[string]string {
	env := map[string]string{
		progress.EnvProtocol: progress.ProtocolVersion,
		// Tell the server-side voodu that the client already
		// applied rewriteColonSyntax — re-applying on the server
		// would mangle multi-segment plugin commands like
		// `pg:backups:capture`. The client splits to
		// `["pg", "backups:capture"]`; without this marker the
		// server would re-split `backups:capture` into
		// `["backups", "capture"]`, dropping the third segment.
		envRewriteAlreadyApplied: "1",
	}

	if v := os.Getenv("NO_COLOR"); v != "" {
		env["NO_COLOR"] = v
		return env
	}

	if v := os.Getenv("FORCE_COLOR"); v != "" {
		env["FORCE_COLOR"] = v
		return env
	}

	if term.IsTerminal(int(os.Stdout.Fd())) {
		env["FORCE_COLOR"] = "1"
	}

	return env
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
