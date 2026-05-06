package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// looksLikePluginDispatch matches a `vd <plugin>:<command>
// [args...]` invocation that should route to the structured
// dispatch endpoint. The CLI is a DUMB FORWARDER — no arity
// knowledge, no per-verb hardcoded behaviour, no help intercept.
// All semantics live in the plugin itself; the CLI just packages
// the operator's args and POSTs them.
//
// Detection rule: argv has at least 2 tokens; argv[0] is a plain
// alphanumeric identifier (the plugin); argv[1] is a command
// path — one or more idents separated by colons, e.g. `info`,
// `backups:capture`, etc. Multi-segment commands let plugins
// expose nested verbs (heroku-style `pg:backups:capture`) without
// the CLI needing to know about them.
//
// Everything after argv[1] is treated as the plugin command's
// args, including flags like `-h` — those flow through to the
// plugin which is responsible for its own help output.
//
// Returns (plugin, command, args, true) on a match.
//
// Leading root flags (`-o json`, `--controller-url=...`, etc.) are
// skipped before the detection. Operators occasionally prepend
// these (`vd -o json pg:backups`); the orchestrator path also
// inserts `-o json` ahead of the plugin command when capturing the
// dispatch envelope from an SSH-forwarded invocation. Without the
// skip, leading flags push the plugin name out of position 0 and
// dispatch falls through to the legacy /plugins/exec route → 404.
func looksLikePluginDispatch(argv []string) (plugin, command string, args []string, ok bool) {
	stripped := stripLeadingRootFlags(argv)
	if len(stripped) < 2 {
		return "", "", nil, false
	}

	if !isIdent(stripped[0]) || !isCommandPath(stripped[1]) {
		return "", "", nil, false
	}

	return stripped[0], stripped[1], stripped[2:], true
}

// stripLeadingRootFlags advances past any sequence of `-flag` /
// `-flag value` pairs at the start of argv, returning the suffix.
// Uses takesValue() so a flag known to take a value consumes the
// next token; bare boolean flags (`-y`) consume only themselves.
//
// Stops at the first positional — by definition, the plugin name.
// Stops on `--` (POSIX end-of-options) so anything past it is
// preserved as positional, even if it starts with a dash.
func stripLeadingRootFlags(argv []string) []string {
	i := 0

	for i < len(argv) {
		tok := argv[i]

		if tok == "--" {
			return argv[i+1:]
		}

		if !strings.HasPrefix(tok, "-") {
			return argv[i:]
		}

		// `-flag=value` carries its own value — single token.
		if strings.Contains(tok, "=") {
			i++
			continue
		}

		// `-flag value` only when the flag is known to take a value.
		// Unknown flags are treated as bare; we stop there rather
		// than gobble the next token (which might be the plugin).
		if takesValue(tok) && i+1 < len(argv) {
			i += 2
			continue
		}

		i++
	}

	return nil
}

// isCommandPath reports whether s is a colon-separated chain of
// idents (`info`, `backups:capture`, `a:b:c`). Used to validate
// the command segment of a plugin dispatch invocation.
//
// Defined here (vs alongside isIdent in dispatch.go) so the
// dispatch test file in this package — which exercises
// looksLikePluginDispatch — sees both helpers without a circular
// import.
func isCommandPath(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}

	for _, chunk := range strings.Split(s, ":") {
		if !isIdent(chunk) {
			return false
		}
	}

	return true
}

// pluginDispatchPayload mirrors the server-side
// pluginDispatchRequest. Body is just `{args}` — no from/to
// pre-fetch hints. Plugin parses args itself.
type pluginDispatchPayload struct {
	Args []string `json:"args,omitempty"`
}

type pluginDispatchResponse struct {
	Status string `json:"status"`
	Data   struct {
		Message   string                       `json:"message"`
		Applied   []string                     `json:"applied"`
		ExecLocal []pluginDispatchExecLocalCmd `json:"exec_local,omitempty"`
		FetchFile []pluginDispatchFetchFileCmd `json:"fetch_file,omitempty"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

// pluginDispatchExecLocalCmd is one command the plugin asked the
// CLI to run locally on the operator's host with TTY attached.
// Mirrors the controller's pluginDispatchExecLocal — kept in
// lockstep so dispatch JSON deserialises cleanly both ways.
//
// CLI executes each entry sequentially in the order returned by
// the controller (preserving plugin emit order). Exit codes from
// the local commands propagate as the CLI's exit code.
type pluginDispatchExecLocalCmd struct {
	Command []string `json:"command"`
}

// pluginDispatchFetchFileCmd carries metadata for a host→operator
// file transfer. The CLI picks the actual mechanism — scp when
// auto-forwarded over SSH, cp when running on the controller —
// and never reads bytes through the dispatch envelope.
//
// Mirrors the controller's pluginDispatchFetchFile. See
// runFetchFile for the operator-side handling.
type pluginDispatchFetchFileCmd struct {
	RemotePath string `json:"remote_path"`
	DestPath   string `json:"dest_path"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
}

// runPluginDispatch POSTs the operator's args to the plugin
// dispatch endpoint and renders the response. The CLI doesn't
// inspect the args — they're whatever the operator typed after
// `<plugin>:<command>`, including positional refs and flags.
//
// Plugin is responsible for parsing its own argv (via
// os.Args[2:] when invoked) and for emitting envelope-shaped
// stdout. Server applies any `actions` returned and surfaces
// the `message` back here.
func runPluginDispatch(root *cobra.Command, plugin, command string, args []string) error {
	// Cobra never reaches Execute() on the dispatch path, so root's
	// persistent flags ($-o, --controller-url, etc.) carry their
	// defaults unless we parse them here. The forwardToController
	// path does the same dance — we mirror it so `-o json` and
	// `--controller-url=...` work uniformly across forwarder and
	// dispatcher. Pass os.Args[1:] (filtered for flags) so leading
	// flags ahead of the plugin name get applied; positional args
	// stay in `args` for the plugin payload.
	_ = root.PersistentFlags().Parse(filterFlags(os.Args[1:]))

	body := pluginDispatchPayload{Args: args}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}

	url := strings.TrimRight(controllerURL(root), "/") + "/plugin/" + plugin + "/" + command

	// VOODU_DEBUG=1 prints the dispatch URL + args to stderr.
	// Useful for "is the multi-colon split landing right?" kind
	// of debugging when an operator suspects a CLI staleness or
	// routing issue. No-op without the env var so normal output
	// stays clean.
	if os.Getenv("VOODU_DEBUG") == "1" {
		fmt.Fprintf(os.Stderr, "voodu-debug: dispatch URL=%s args=%v\n", url, args)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("voodu-cli/%s", version))

	client := &http.Client{Timeout: forwardTimeout}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch %s:%s: controller at %s unreachable (%v)", plugin, command, url, err)
	}
	defer resp.Body.Close()

	body2, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, body2)
	}

	// `-o json`: dump the server's envelope verbatim. The Mac-side
	// orchestrator (runDownloadForwarded) relies on this to parse
	// fetch_file / exec_local actions out of an SSH-forwarded
	// dispatch — the pretty-text path below would lose the
	// structured action data. Operators can also pipe to jq for
	// programmatic access to applied summaries / messages.
	if outputFormat(root) == "json" {
		// We already verified the body parses (via the unmarshal
		// below for the text path). Pass it through as-is so jq
		// gets the canonical wire shape, not a re-marshaled copy.
		fmt.Print(string(body2))

		if !bytes.HasSuffix(body2, []byte("\n")) {
			fmt.Println()
		}

		return nil
	}

	var out pluginDispatchResponse
	if err := json.Unmarshal(body2, &out); err != nil {
		// Plugin emitted plain text — print as-is.
		fmt.Print(string(body2))

		if !bytes.HasSuffix(body2, []byte("\n")) {
			fmt.Println()
		}

		return nil
	}

	if out.Data.Message != "" {
		fmt.Println(out.Data.Message)
	}

	for _, a := range out.Data.Applied {
		fmt.Printf("  ✓ %s\n", a)
	}

	// exec_local: plugin asked us to invoke commands locally
	// (typically interactive shells). Each command runs with the
	// operator's stdin/stdout/stderr attached so TTY-dependent
	// flows work — psql, redis-cli, etc.
	//
	// Sequential execution: we run them in plugin emit order and
	// stop on the first non-zero exit. The exit code propagates
	// up so shell pipelines / scripts see the underlying
	// command's status, not just the dispatch wrapper's.
	for _, ex := range out.Data.ExecLocal {
		if err := runExecLocal(ex.Command); err != nil {
			return err
		}
	}

	// fetch_file: plugin asked us to transfer a file from the
	// controller host to the operator's filesystem. Local-mode
	// only here — auto-forwarded :download is handled earlier
	// by runDownloadForwarded, which uses scp with progress
	// instead of cp (and never hits this dispatch path). The
	// guard catches accidental fetch_file emissions on remote
	// invocations (e.g. a future plugin) so we don't silently
	// no-op or, worse, copy on the wrong machine.
	for _, ff := range out.Data.FetchFile {
		if err := runFetchFileLocal(ff); err != nil {
			return err
		}
	}

	return nil
}

// runExecLocal invokes one exec_local command with the operator's
// TTY attached. Stdin/stdout/stderr forward as-is so interactive
// programs (psql, redis-cli, etc.) feel like the operator ran
// them directly. Non-zero exit from the child surfaces as the
// CLI's exit (via os.Exit on the parent's error path).
func runExecLocal(command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("exec_local: empty command")
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		// ExitError carries the child's exit code; surface it
		// verbatim so `vd pg:psql` followed by `;` exits with
		// the same code psql would have.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}

		return fmt.Errorf("exec_local %s: %w", command[0], err)
	}

	return nil
}

// runFetchFileLocal copies RemotePath → DestPath on the same
// machine. Used when the plugin and the operator are on the same
// host (no auto-forward) — `cp` is enough; no SSH needed.
//
// The auto-forward case takes a different code path
// (runDownloadForwarded) which uses scp from the operator to the
// controller machine before the dispatch ever happens, so this
// handler never runs in that mode.
//
// Refuses to clobber an existing destination — operators
// commonly re-run `:download` and the default dest is the
// in-pod filename, so accidental overwrites from a stale local
// copy would be easy to make.
func runFetchFileLocal(ff pluginDispatchFetchFileCmd) error {
	if ff.RemotePath == "" || ff.DestPath == "" {
		return fmt.Errorf("fetch_file: remote_path and dest_path are required")
	}

	if _, err := os.Stat(ff.DestPath); err == nil {
		return fmt.Errorf("fetch_file %s: file already exists (move/rename it, or pass --to <other>)", ff.DestPath)
	}

	src, err := os.Open(ff.RemotePath)
	if err != nil {
		return fmt.Errorf("fetch_file: open %s: %w", ff.RemotePath, err)
	}

	defer src.Close()

	dst, err := os.Create(ff.DestPath)
	if err != nil {
		return fmt.Errorf("fetch_file: create %s: %w", ff.DestPath, err)
	}

	defer dst.Close()

	n, err := io.Copy(dst, src)
	if err != nil {
		return fmt.Errorf("fetch_file: copy %s → %s: %w", ff.RemotePath, ff.DestPath, err)
	}

	fmt.Printf("  ✓ wrote %s (%d bytes)\n", ff.DestPath, n)

	return nil
}

// isIdent is defined in dispatch.go (shared with the colon
// splitter). The dispatch detector and splitCommandColon both
// gate on the same identifier rule.
