package main

import (
	"reflect"
	"testing"
)

func TestRewriteColonSyntax(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "empty",
			in:   []string{},
			want: []string{},
		},
		{
			name: "no colon passes through",
			in:   []string{"voodu", "get", "pods"},
			want: []string{"voodu", "get", "pods"},
		},
		{
			name: "config:set reorders ref before verb",
			in:   []string{"voodu", "config:set", "clowk-lp/web", "FOO=bar"},
			want: []string{"voodu", "config", "clowk-lp/web", "set", "FOO=bar"},
		},
		{
			name: "config:unset reorders ref before verb",
			in:   []string{"voodu", "config:unset", "clowk-lp", "FOO"},
			want: []string{"voodu", "config", "clowk-lp", "unset", "FOO"},
		},
		{
			name: "config:set with multiple KEY=VALUE pairs",
			in:   []string{"voodu", "config:set", "clowk-lp/web", "FOO=bar", "BAZ=qux"},
			want: []string{"voodu", "config", "clowk-lp/web", "set", "FOO=bar", "BAZ=qux"},
		},
		{
			name: "config:set with --no-restart flag in front of ref",
			in:   []string{"voodu", "config:set", "--no-restart", "clowk-lp", "FOO=bar"},
			want: []string{"voodu", "config", "clowk-lp", "set", "--no-restart", "FOO=bar"},
		},
		{
			name: "config:set with --remote flag (takes value)",
			in:   []string{"voodu", "config:set", "--remote", "staging", "clowk-lp", "FOO=bar"},
			want: []string{"voodu", "config", "clowk-lp", "set", "--remote", "staging", "FOO=bar"},
		},
		{
			name: "plugin-style colon rewrite (non-config) keeps simple split",
			in:   []string{"voodu", "postgres:create", "main"},
			want: []string{"voodu", "postgres", "create", "main"},
		},
		{
			name: "value after flag is not rewritten",
			in:   []string{"voodu", "config", "set", "-a", "api:prod"},
			want: []string{"voodu", "config", "set", "-a", "api:prod"},
		},
		{
			name: "long flag with equals is not rewritten",
			in:   []string{"voodu", "--controller-url=http://host:8080", "status"},
			want: []string{"voodu", "--controller-url=http://host:8080", "status"},
		},
		{
			name: "url-like token is not rewritten",
			in:   []string{"voodu", "plugins", "install", "registry.io:5000/caddy"},
			want: []string{"voodu", "plugins", "install", "registry.io:5000/caddy"},
		},
		{
			name: "user@host:app token (git remote) is not rewritten",
			in:   []string{"voodu", "remote", "add", "ubuntu@server.com:api"},
			want: []string{"voodu", "remote", "add", "ubuntu@server.com:api"},
		},
		{
			name: "KEY=VALUE with colon inside value is not rewritten",
			in:   []string{"voodu", "config:set", "DB_URL=postgres://a:b@c/d"},
			want: []string{"voodu", "config", "set", "DB_URL=postgres://a:b@c/d"},
		},
		{
			name: "colon without right side left alone",
			in:   []string{"voodu", "foo:"},
			want: []string{"voodu", "foo:"},
		},
		{
			name: "multi-colon plugin command (heroku-style nested)",
			in:   []string{"voodu", "pg:backups:capture", "clowk-lp/db"},
			want: []string{"voodu", "pg", "backups:capture", "clowk-lp/db"},
		},
		{
			name: "multi-colon plugin command with flag",
			in:   []string{"voodu", "pg:backups:cancel", "clowk-lp/db", "b008"},
			want: []string{"voodu", "pg", "backups:cancel", "clowk-lp/db", "b008"},
		},
		{
			name: "deep colon chain (a:b:c:d) all valid idents splits on first",
			in:   []string{"voodu", "a:b:c:d"},
			want: []string{"voodu", "a", "b:c:d"},
		},
		{
			name: "multi-colon with invalid chunk (slash inside) NOT rewritten",
			in:   []string{"voodu", "pg:backups:capture/x", "ref"},
			want: []string{"voodu", "pg:backups:capture/x", "ref"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteColonSyntax(tc.in)

			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rewriteColonSyntax(%v):\n  got  %v\n  want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsKnownCommand(t *testing.T) {
	root := newRootCmd()

	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"empty is root (known)", []string{}, true},
		{"only flags is root (known)", []string{"--help"}, true},
		{"flag value skipped", []string{"--controller-url", "http://x", "get"}, true},
		{"builtin get", []string{"get"}, true},
		{"builtin config", []string{"config", "set"}, true},
		{"builtin version", []string{"version"}, true},
		{"unknown plugin verb", []string{"postgres", "create", "main"}, false},
		{"unknown single token", []string{"nope"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isKnownCommand(root, tc.args); got != tc.want {
				t.Errorf("isKnownCommand(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestRewriteColonSyntax_DoesNotDoubleSplitOnAlreadyRewritten pins
// the bug where a forwarded SSH invocation would re-apply the colon
// rewrite on the server side, splitting `backups:capture` (already
// the result of one pass) into `backups` + `capture`. Resulting
// argv had 5 tokens instead of 4, dispatch routed to the wrong
// command (`pg backups`, with `capture` as a positional arg).
//
// The fix lives in main.go: skip the rewrite when env
// VOODU_ARGV_REWRITTEN=1 is set (client toggles it before SSH).
// This test pins what rewriteColonSyntax DOES on already-split
// argv when accidentally invoked — to catch regressions in the
// rewrite function itself, not the env-var gate.
func TestRewriteColonSyntax_AlreadySplitArgvGetsManglesByDefault(t *testing.T) {
	// Already-split argv that mimics what arrives on the server
	// after the client splits "pg:backups:capture":
	in := []string{"voodu", "pg", "backups:capture", "clowk-lp/db"}

	got := rewriteColonSyntax(in)

	// The rewrite re-splits "backups:capture" into two tokens.
	// This is intentional — the function has no way to know the
	// argv was pre-rewritten. The fix is the env var gate in
	// main.go that skips this call entirely on the server side.
	want := []string{"voodu", "pg", "backups", "capture", "clowk-lp/db"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("rewriteColonSyntax (re-applied):\n  got  %v\n  want %v\n  (this confirms the bug exists; the env-var gate in main.go is what prevents the second call)",
			got, want)
	}
}

func TestFilterFlags(t *testing.T) {
	in := []string{"postgres", "create", "--controller-url", "http://x:9", "main", "-a", "prod"}
	want := []string{"--controller-url", "http://x:9", "-a", "prod"}

	if got := filterFlags(in); !reflect.DeepEqual(got, want) {
		t.Errorf("filterFlags:\n  got  %v\n  want %v", got, want)
	}
}
