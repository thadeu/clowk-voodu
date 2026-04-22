package main

import (
	"strings"

	"github.com/spf13/cobra"
)

// rewriteColonSyntax rewrites `cmd:sub` tokens to separate `cmd sub`
// tokens so Cobra can route them. We preserve flag values untouched —
// only the first colon in a non-flag token is split, and only when both
// sides are alphanumeric (so URLs and paths like `user@host:app` or
// `registry.io:5000/x` are not mangled).
//
// Called at process entry to rewrite os.Args in place; tested in
// isolation against the same rule set.
func rewriteColonSyntax(argv []string) []string {
	if len(argv) == 0 {
		return argv
	}

	out := make([]string, 0, len(argv)+4)
	out = append(out, argv[0])

	// skipFlagValue is true when the previous token was a flag like
	// `--app` that takes a value, so the next token is the value and
	// must not be split.
	skipFlagValue := false

	for i := 1; i < len(argv); i++ {
		tok := argv[i]

		if skipFlagValue {
			out = append(out, tok)
			skipFlagValue = false

			continue
		}

		if strings.HasPrefix(tok, "-") {
			out = append(out, tok)

			if !strings.Contains(tok, "=") && takesValue(tok) {
				skipFlagValue = true
			}

			continue
		}

		if left, right, ok := splitCommandColon(tok); ok {
			out = append(out, left, right)
			continue
		}

		out = append(out, tok)
	}

	return out
}

// splitCommandColon returns (left, right, true) when `tok` looks like a
// command:subcommand form (e.g. `config:set`, `postgres:create`). A
// token qualifies when it contains exactly one ':' and both sides are
// non-empty identifiers (letters, digits, hyphens, underscores).
func splitCommandColon(tok string) (string, string, bool) {
	idx := strings.Index(tok, ":")
	if idx <= 0 || idx == len(tok)-1 {
		return "", "", false
	}

	left := tok[:idx]
	right := tok[idx+1:]

	if strings.ContainsAny(right, ":/@") {
		return "", "", false
	}

	if !isIdent(left) || !isIdent(right) {
		return "", "", false
	}

	return left, right, true
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		if !(r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')) {
			return false
		}
	}

	return true
}

// takesValue returns true for known CLI flags that accept a separate
// argument. Listed explicitly so unknown `--foo bar` pairs don't
// accidentally suppress colon-rewriting of `bar`.
func takesValue(flag string) bool {
	known := map[string]bool{
		"-a": true, "--app": true,
		"-f": true, "--file": true,
		"-r": true, "--replicas": true,
		"-o": true, "--output": true,
		"--controller-url": true,
	}

	if strings.Contains(flag, "=") {
		return false
	}

	return known[flag]
}

// dispatch runs the cobra tree. If the first positional token does not
// resolve to a known command, the arguments are forwarded to the
// controller over HTTP instead of producing an "unknown command" error.
func dispatch(root *cobra.Command, args []string) error {
	if isKnownCommand(root, args) {
		root.SetArgs(args)
		return root.Execute()
	}

	return forwardToController(root, args)
}

// isKnownCommand walks the command tree to see whether the first
// positional argument maps to a registered command. Flags and their
// values are skipped. Empty input resolves to the root, which is
// "known" (cobra will print help).
func isKnownCommand(root *cobra.Command, args []string) bool {
	skipFlagValue := false

	for _, tok := range args {
		if skipFlagValue {
			skipFlagValue = false
			continue
		}

		if strings.HasPrefix(tok, "-") {
			if !strings.Contains(tok, "=") && takesValue(tok) {
				skipFlagValue = true
			}

			continue
		}

		for _, c := range root.Commands() {
			if c.Name() == tok {
				return true
			}

			for _, alias := range c.Aliases {
				if alias == tok {
					return true
				}
			}
		}

		return false
	}

	return true
}
