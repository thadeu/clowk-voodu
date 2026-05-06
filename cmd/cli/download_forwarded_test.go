package main

import (
	"strings"
	"testing"
)

// TestIsPluginDownloadCommand_Matches pins the matcher contract:
// after rewriteColonSyntax has split on the first colon, a download
// invocation looks like ["pg", "backups:download", ...]. Anything
// else (other plugin commands, bare flags, empty argv) must NOT
// match — the orchestrator runs scp on the operator's machine, so
// false positives would scp things the user didn't ask for.
func TestIsPluginDownloadCommand_Matches(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		// Canonical shape after rewriteColonSyntax.
		{[]string{"pg", "backups:download", "clowk-lp/db", "b013"}, true},
		{[]string{"pg", "backups:download", "clowk-lp/db", "b013", "--to", "./x.dump"}, true},
		{[]string{"postgres", "backups:download", "clowk-lp/db", "b013"}, true},

		// Other plugin commands — must not match.
		{[]string{"pg", "backups", "clowk-lp/db"}, false},
		{[]string{"pg", "backups:capture", "clowk-lp/db"}, false},
		{[]string{"pg", "backups:logs", "clowk-lp/db", "b013"}, false},
		{[]string{"pg", "psql", "clowk-lp/db"}, false},

		// Edge cases — empty / leading-flag / single-arg.
		{nil, false},
		{[]string{}, false},
		{[]string{"pg"}, false},
		{[]string{"--help"}, false},
		{[]string{"-o", "json", "pg", "backups:download"}, false}, // flags before plugin name
	}

	for _, tc := range cases {
		got := isPluginDownloadCommand(tc.args)
		if got != tc.want {
			t.Errorf("isPluginDownloadCommand(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestShellEscapeForScp pins the quoting rules: simple paths pass
// through untouched (the common case — host bind-mount paths under
// /opt/voodu/backups don't have spaces), but anything containing
// shell metacharacters gets single-quoted to survive scp's
// remote-shell expansion.
func TestShellEscapeForScp(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// No metacharacters — passthrough.
		{"/opt/voodu/backups/clowk-lp/db/b001-20260505T020000Z.dump",
			"/opt/voodu/backups/clowk-lp/db/b001-20260505T020000Z.dump"},
		{"/srv/backups/db.dump", "/srv/backups/db.dump"},

		// Spaces — single-quote wrap.
		{"/srv/with space/db.dump", "'/srv/with space/db.dump'"},

		// Embedded single quote — POSIX escape trick.
		{"/srv/o'reilly/db.dump", `'/srv/o'\''reilly/db.dump'`},

		// Dollar sign / backtick — must quote so the remote shell
		// doesn't try to expand a variable / run a subshell.
		{"/srv/$danger/db.dump", "'/srv/$danger/db.dump'"},
		{"/srv/`evil`/db.dump", "'/srv/`evil`/db.dump'"},
	}

	for _, tc := range cases {
		got := shellEscapeForScp(tc.in)
		if got != tc.want {
			t.Errorf("shellEscapeForScp(%q):\n  got:  %q\n  want: %q", tc.in, got, tc.want)
		}
	}
}

// TestShellEscapeForScp_NoLeakedExpansion runs through the
// concerning shell metas and confirms NONE of them survive
// unquoted. Defense-in-depth — a future regression where someone
// tightens the "no metachars" predicate would still produce safe
// output through this test.
func TestShellEscapeForScp_NoLeakedExpansion(t *testing.T) {
	dangerous := []string{
		"/path with space/file",
		"/path/$VAR/file",
		"/path/`cmd`/file",
		"/path/\"file\"",
		"/path/\\file",
		"/path/'with quote'/file",
	}

	for _, p := range dangerous {
		got := shellEscapeForScp(p)
		if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
			t.Errorf("dangerous path %q produced unquoted output %q", p, got)
		}
	}
}
