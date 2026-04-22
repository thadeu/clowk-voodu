package remote

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

// ForwardOptions tweaks how the SSH invocation is shaped. The zero
// value is the normal case (TTY auto-detected, identity from SSH
// config, `voodu` as the remote binary name).
type ForwardOptions struct {
	// SSHBin lets tests inject a fake `ssh` on PATH. Empty = real ssh.
	SSHBin string

	// Identity is the -i argument. Empty = SSH picks from config.
	Identity string

	// RemoteBinary is the command name on the server. Defaults to "voodu".
	RemoteBinary string

	// ForceTTY overrides TTY auto-detection. nil = detect from stdin.
	ForceTTY *bool

	// Stdin, when non-nil, replaces os.Stdin as the input to the remote
	// process. Setting this also disables TTY allocation (a piped reader
	// and a TTY don't mix — ssh -tt would eat raw bytes with CR/LF fun).
	Stdin io.Reader
}

// Forward runs `ssh [opts] HOST voodu <args...>` with stdio wired to
// the current process and returns the remote exit code. This is what
// makes `voodu logs -a api` transparent: the caller sees the server's
// output streaming in real time, and $? on exit matches the server.
func Forward(info *Info, args []string, opts ForwardOptions) (int, error) {
	if info == nil {
		return 1, fmt.Errorf("no remote configured")
	}

	bin := opts.SSHBin
	if bin == "" {
		bin = "ssh"
	}

	remoteBin := opts.RemoteBinary
	if remoteBin == "" {
		remoteBin = "voodu"
	}

	sshArgs := []string{}

	if opts.Identity != "" {
		sshArgs = append(sshArgs, "-i", opts.Identity)
	}

	// A custom stdin means we're streaming bytes (manifest JSON, a tar,
	// etc.) — TTY mode would mangle that, so skip -tt unconditionally.
	if opts.Stdin == nil && wantTTY(opts.ForceTTY) {
		// -tt forces allocation even when the local stdin isn't a TTY
		// — needed for `logs -f` over a non-interactive caller, and
		// harmless when we do have a TTY.
		sshArgs = append(sshArgs, "-tt")
	}

	sshArgs = append(sshArgs, info.Host, buildRemoteCommand(remoteBin, args))

	cmd := exec.Command(bin, sshArgs...)

	if opts.Stdin != nil {
		cmd.Stdin = opts.Stdin
	} else {
		cmd.Stdin = os.Stdin
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err == nil {
		return 0, nil
	}

	// exec.ExitError carries the real exit code from the remote ssh
	// chain; anything else is a local failure (couldn't find ssh, etc).
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), nil
	}

	return 1, fmt.Errorf("ssh %s: %w", info.Host, err)
}

// buildRemoteCommand shell-escapes each argv entry and joins them so
// the remote shell reconstructs the argv exactly. Without escaping,
// `config:set FOO="bar baz" -a api` would land on the server as three
// tokens plus garbage — classic Gokku bug.
func buildRemoteCommand(bin string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellEscape(bin))

	for _, a := range args {
		parts = append(parts, shellEscape(a))
	}

	return strings.Join(parts, " ")
}

// shellEscape wraps s in single quotes, escaping any embedded single
// quotes using the standard close-quote/backslash-quote/open-quote
// sequence. Bulletproof against spaces, $, `, &, newlines, etc.
func shellEscape(s string) string {
	if s == "" {
		return "''"
	}

	// Fast path: identifier-only strings pass through unchanged, which
	// keeps the transmitted command readable in ssh -v output.
	if isShellSafe(s) {
		return s
	}

	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func isShellSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '/', r == ':',
			r == '=', r == '@', r == '+', r == ',':
			continue
		default:
			return false
		}
	}

	return true
}

// wantTTY picks a TTY policy. Explicit override wins; otherwise we
// allocate a TTY iff stdin is a terminal — programmatic callers (pipes,
// scripts) don't want the CR/LF mess of a forced TTY.
func wantTTY(force *bool) bool {
	if force != nil {
		return *force
	}

	return term.IsTerminal(int(os.Stdin.Fd()))
}
