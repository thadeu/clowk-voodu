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

	// consumed tracks indices already emitted out-of-order by the
	// `config:set <ref>` reorder so the outer loop doesn't re-emit
	// them when iteration reaches their natural position.
	var consumed []int

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
			// Heroku-style `config:set <ref> KEY=VAL` shorthand. The
			// underlying command surface is ref-first
			// (`config <ref> set KEY=VAL`), so a naive split would
			// land as `config set <ref> KEY=VAL` and the ref-parser
			// would treat "set" as the scope. Swap the next non-flag
			// token (the ref) into position so the muscle-memory
			// shape works without growing a new subcommand.
			//
			// Limited to config on purpose: other umbrella commands
			// either keep their verb-first shape (`plugins:install`)
			// or never gained a positional ref. When more verbs
			// adopt the ref-first pattern this list grows.
			if left == "config" && isRefFirstConfigVerb(right) {
				if next, idx, found := nextPositional(argv, i+1); found && !looksLikeKeyValue(next) {
					out = append(out, left, next, right)
					// Mark the ref-token as already consumed so the
					// outer loop doesn't re-emit it. We can't simply
					// `i = idx` because the loop's `i++` would skip
					// one too many on the next iteration; instead we
					// stash it in a small skip-set.
					consumeIndex(&consumed, idx)
					continue
				}
			}

			out = append(out, left, right)
			continue
		}

		if isConsumed(consumed, i) {
			continue
		}

		out = append(out, tok)
	}

	return out
}

// isRefFirstConfigVerb names the config verbs that accept the
// ref-first shape via colon shorthand. Kept tiny on purpose — only
// commands whose canonical surface is `config <ref> <verb>` should
// trigger the reorder. `list` and `get` are excluded (the bareword
// `vd config <ref>` already covers list, and `get` without a ref
// has no useful colon form to support).
func isRefFirstConfigVerb(verb string) bool {
	return verb == "set" || verb == "unset"
}

// looksLikeKeyValue reports whether tok looks like a `KEY=VALUE`
// arg rather than a ref. Env var names can't contain `=` (it's the
// separator), and scope/name refs are conventionally lowercase
// identifiers without `=` either — so the `=` is a reliable
// "definitely not a ref" signal. Used by the colon-rewrite so
// `config:set FOO=bar` (operator forgot the ref) doesn't get
// silently mis-parsed as ref="FOO=bar".
func looksLikeKeyValue(tok string) bool {
	return strings.Contains(tok, "=")
}

// nextPositional finds the next non-flag token in argv starting at
// `start`. Returns (token, index, true) when found and (_, _, false)
// otherwise. Skips flag values for known value-taking flags so
// `config:set --remote prod <ref>` still detects <ref> correctly.
func nextPositional(argv []string, start int) (string, int, bool) {
	skipNext := false

	for j := start; j < len(argv); j++ {
		tok := argv[j]

		if skipNext {
			skipNext = false
			continue
		}

		if strings.HasPrefix(tok, "-") {
			if !strings.Contains(tok, "=") && takesValue(tok) {
				skipNext = true
			}

			continue
		}

		return tok, j, true
	}

	return "", -1, false
}

func consumeIndex(set *[]int, i int) {
	*set = append(*set, i)
}

func isConsumed(set []int, i int) bool {
	for _, x := range set {
		if x == i {
			return true
		}
	}

	return false
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

	// Reject paths/URLs masquerading as commands. URLs typically
	// contain `/` (s3://bucket/key) or `@` (user@host:path); plain
	// `host:port` strings would accidentally split, but operators
	// don't pass those as positional command names.
	if strings.ContainsAny(right, "/@") {
		return "", "", false
	}

	if !isIdent(left) {
		return "", "", false
	}

	// Right side may itself contain colons for chained subcommands
	// (heroku-style: `pg:backups:capture` → plugin=pg,
	// command=`backups:capture`). Each colon-separated chunk must
	// be a plain identifier so we don't mangle URLs/paths sneaking
	// past the slash check above.
	for _, chunk := range strings.Split(right, ":") {
		if !isIdent(chunk) {
			return "", "", false
		}
	}

	return left, right, true
}

// isIdent reports whether s is a plain alphanumeric identifier
// (with hyphens / underscores allowed inside). Tokens starting
// with `-` are rejected so the dispatch detector doesn't mistake
// a flag like `-h` for a plugin or command name.
func isIdent(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
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
		"-r": true, "--remote": true,
		"-o": true, "--output": true,
		"--controller-url": true,
		"--replicas":       true,
		"--format":         true,
	}

	if strings.Contains(flag, "=") {
		return false
	}

	return known[flag]
}

// dispatch runs the cobra tree. If the first positional token does not
// resolve to a known command, the arguments are routed to the plugin
// dispatch endpoint (passthrough — args go to the plugin verbatim).
//
// The CLI is intentionally dumb about plugin commands:
//
//   - No arity knowledge. Plugin owns its argv parsing.
//   - No hardcoded help. `-h`/`--help` flows through as args;
//     the plugin emits its own usage.
//   - No per-verb special cases. Every `<plugin>:<command>` is
//     packaged identically and POSTed to /plugin/{name}/{command}.
//
// Server discovers the plugin (loads from PluginsRoot), invokes
// bin/<command> with the args, applies any actions the plugin
// returns. If the plugin or bin/<command> doesn't exist, the
// server responds with a clear error and dispatch surfaces it.
func dispatch(root *cobra.Command, args []string) error {
	if isKnownCommand(root, args) {
		root.SetArgs(args)
		return root.Execute()
	}

	// `vd <plugin> -h` and `vd <plugin> --help` (no verb)
	// synthesize a call to the plugin's `help` command. The
	// plugin author drops a bin/help shim and decides what
	// the overview looks like — CLI doesn't render anything
	// itself. Without this, `-h` would fall through to the
	// generic forwarder and the operator would see "unknown
	// command" instead of plugin help.
	if plugin, isHelp := looksLikePluginOverviewHelp(args); isHelp {
		return runPluginDispatch(root, plugin, "help", nil)
	}

	if plugin, command, pluginArgs, ok := looksLikePluginDispatch(args); ok {
		return runPluginDispatch(root, plugin, command, pluginArgs)
	}

	return forwardToController(root, args)
}

// looksLikePluginOverviewHelp matches `vd <plugin> -h` /
// `vd <plugin> --help` shapes — operator asking for plugin-level
// overview. Returns (plugin, true) when matched.
//
// Verb-specific help (`vd <plugin>:<verb> -h`) is handled by
// looksLikePluginDispatch as a normal arg passthrough — the
// plugin's verb command sees `-h` in its argv and decides what
// to do.
func looksLikePluginOverviewHelp(args []string) (plugin string, ok bool) {
	if len(args) != 2 {
		return "", false
	}

	if !isIdent(args[0]) {
		return "", false
	}

	if args[1] != "-h" && args[1] != "--help" {
		return "", false
	}

	return args[0], true
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
