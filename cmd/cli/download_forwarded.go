// Mac-side orchestrator for `:download` plugin commands when the
// invocation is being auto-forwarded over SSH.
//
// The problem: if we just SSH-forward `vd pg:backups:download` like
// any other command, the plugin (and any in-process file copy)
// runs on the controller host. The dump file lands on the VM, not
// on the operator's machine. Earlier base64-in-dispatch attempt
// capped at ~256 MiB and burned 3x RAM through the pipeline.
//
// The fix: Mac CLI orchestrates the transfer:
//
//  1. SSH-forward a metadata-only dispatch (the plugin's
//     `:download` command) with stdout captured. Plugin emits a
//     fetch_file action carrying remote_path + dest_path.
//  2. Mac CLI parses the JSON envelope, extracts the action.
//  3. Mac CLI runs `scp -i <identity> <user@host>:<remote_path>
//     <dest_path>` on the operator's machine. scp shows native
//     progress and has no size limit.
//
// Plugins opt in by emitting fetch_file. The match below is
// intentionally narrow (`pg:backups:download` only) until we
// generalise the contract — the plugin manifest could declare
// "download" verbs in a future iteration so this hardcoding goes
// away.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"go.voodu.clowk.in/internal/remote"
)

// isPluginDownloadCommand returns true for `<plugin>:backups:download`
// invocations. We match suffix only — the orchestrator behaviour is
// the same regardless of which plugin emitted it (today only
// voodu-postgres; tomorrow voodu-redis or voodu-mysql can ride along
// without CLI changes by emitting the same fetch_file action).
//
// Skips flag-only invocations and bare command tokens. The matcher
// runs after the colon-rewrite, so the canonical shape we look for
// is `<plugin>` then `backups:download` as the second positional —
// that's what splitCommandColon produces from `pg:backups:download`.
func isPluginDownloadCommand(args []string) bool {
	if len(args) < 2 {
		return false
	}

	// First positional is the plugin name (after rewriteColonSyntax).
	// Second is the command path. `pg:backups:download` becomes
	// ["pg", "backups:download"].
	plugin := args[0]
	cmd := args[1]

	if plugin == "" || strings.HasPrefix(plugin, "-") {
		return false
	}

	return cmd == "backups:download"
}

// runDownloadForwarded orchestrates a `:download` invocation when
// the caller's CLI is auto-forwarding over SSH:
//
//  1. SSH-forwards the dispatch with stdout captured into a buffer.
//     Plugin runs, returns its envelope (message + actions).
//  2. Parses the JSON envelope, extracts fetch_file actions.
//  3. For each action, runs scp from the operator's machine,
//     letting scp show its native byte-rate progress.
//
// Returns the SSH dispatch's exit code on metadata failure, or 1
// for any orchestrator-side failure (parse, scp). Successful
// downloads return 0.
func runDownloadForwarded(info *remote.Info, identity string, args []string) (int, error) {
	if info == nil {
		return 1, fmt.Errorf("no remote configured")
	}

	// Phase 1: SSH-forward the dispatch with stdout captured. The
	// plugin's job is to look up the file path on the controller
	// host and emit a fetch_file action — no bytes through the
	// pipe. We pass `-o json` so the dispatch CLI on the remote
	// emits the raw envelope instead of pretty-text, which we can
	// then parse here.
	dispatchArgs := append([]string{"-o", "json"}, args...)

	var stdout bytes.Buffer

	code, err := remote.Forward(info, dispatchArgs, remote.ForwardOptions{
		Identity: identity,
		Stdout:   &stdout,
		Env:      remoteEnv(),
	})
	if err != nil {
		return code, fmt.Errorf("remote dispatch: %w", err)
	}

	if code != 0 {
		// Server already wrote its error to stderr (passes through
		// always). Propagate the exit code so CI sees it.
		return code, nil
	}

	// Phase 2: parse the dispatch envelope from captured stdout.
	// The wire shape mirrors pluginDispatchResponse — same struct
	// the local-mode runPluginDispatch decodes into.
	var resp pluginDispatchResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return 1, fmt.Errorf("decode dispatch envelope: %w\n%s", err, stdout.String())
	}

	if resp.Status == "error" {
		return 1, fmt.Errorf("%s", resp.Error)
	}

	if resp.Data.Message != "" {
		fmt.Println(resp.Data.Message)
	}

	for _, applied := range resp.Data.Applied {
		fmt.Printf("  ✓ %s\n", applied)
	}

	if len(resp.Data.FetchFile) == 0 {
		// Plugin didn't ask for a transfer (probably an error or
		// help path). The message above already explained things;
		// no scp to run.
		return 0, nil
	}

	// Phase 3: run scp for every fetch_file action. Sequential —
	// matching the order the plugin emitted, same as exec_local.
	for _, ff := range resp.Data.FetchFile {
		if err := runScpFetch(info, identity, ff); err != nil {
			return 1, err
		}
	}

	return 0, nil
}

// runScpFetch invokes `scp -i <identity> <host>:<remote> <dest>`
// with stdin/stdout/stderr forwarded so scp's native progress
// renders directly on the operator's terminal. Refuses to clobber
// an existing destination — operators routinely re-run downloads
// and the default dest is the source filename, so a stale local
// copy could be silently overwritten otherwise.
func runScpFetch(info *remote.Info, identity string, ff pluginDispatchFetchFileCmd) error {
	if ff.RemotePath == "" || ff.DestPath == "" {
		return fmt.Errorf("fetch_file: remote_path and dest_path are required")
	}

	if _, err := os.Stat(ff.DestPath); err == nil {
		return fmt.Errorf("fetch_file %s: file already exists (move/rename it, or pass --to <other>)", ff.DestPath)
	}

	scpArgs := []string{}

	if identity != "" {
		scpArgs = append(scpArgs, "-i", identity)
	}

	// -O forces the legacy SCP protocol on systems where openssh
	// has switched to SFTP-default (Ventura+, modern Linux distros).
	// Both work for our purpose; SCP-mode prints a familiar progress
	// bar everywhere we've shipped this. SFTP-mode would print no
	// progress on some implementations.
	scpArgs = append(scpArgs, "-O")

	src := info.Host + ":" + shellEscapeForScp(ff.RemotePath)
	scpArgs = append(scpArgs, src, ff.DestPath)

	cmd := exec.Command("scp", scpArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scp %s → %s: %w", src, ff.DestPath, err)
	}

	return nil
}

// shellEscapeForScp wraps the path in single quotes if it contains
// spaces or shell metacharacters. scp executes the remote half
// through the operator's login shell, so unquoted spaces split the
// path into multiple arguments. The host-bind-mount layout we use
// today (/opt/voodu/backups/<scope>/<name>/<file>) doesn't have
// spaces, but the helper keeps us safe if a future scope or name
// adopts a less restrictive convention.
func shellEscapeForScp(path string) string {
	if !strings.ContainsAny(path, " \t'\"\\$`") {
		return path
	}

	// Single-quote-wrap and escape any embedded single quotes via
	// the standard shell trick: ' → '\''. POSIX sh, bash, zsh, and
	// dash all interpret it the same way.
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
