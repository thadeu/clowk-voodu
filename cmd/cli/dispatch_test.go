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
			name: "simple colon rewrite",
			in:   []string{"voodu", "config:set", "FOO=bar"},
			want: []string{"voodu", "config", "set", "FOO=bar"},
		},
		{
			name: "plugin-style colon rewrite",
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

func TestFilterFlags(t *testing.T) {
	in := []string{"postgres", "create", "--controller-url", "http://x:9", "main", "-a", "prod"}
	want := []string{"--controller-url", "http://x:9", "-a", "prod"}

	if got := filterFlags(in); !reflect.DeepEqual(got, want) {
		t.Errorf("filterFlags:\n  got  %v\n  want %v", got, want)
	}
}
