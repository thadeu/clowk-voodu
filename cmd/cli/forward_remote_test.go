package main

import (
	"testing"
)

func TestIsLocalOnly(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"empty", nil, true},
		{"version", []string{"version"}, true},
		{"help flag", []string{"--help"}, true},
		{"remote subtree", []string{"remote", "add", "api", "u@h:api"}, true},
		// self-update orchestrates SSH internally; the root dispatch
		// itself MUST stay local — otherwise the server's old binary
		// (which is what self-update is supposed to fix!) fails with
		// "unknown command".
		{"self-update", []string{"self-update"}, true},
		{"self-update with flags", []string{"self-update", "--yes"}, true},
		{"self-update with version pin", []string{"self-update", "--version=v0.10.0"}, true},

		{"apply forwards", []string{"apply", "-f", "stack.hcl"}, false},
		{"diff forwards", []string{"diff", "-f", "stack.hcl"}, false},
		{"delete forwards", []string{"delete", "-f", "stack.hcl"}, false},
		{"config set", []string{"config", "set", "FOO=bar", "-a", "api"}, false},
		{"logs", []string{"logs", "-a", "api"}, false},
		{"plugin wildcard", []string{"postgres", "backup", "-a", "pg-0"}, false},

		{"flag before command", []string{"-o", "json", "status"}, false},
		{"output equals", []string{"--output=json", "config", "list"}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isLocalOnly(c.in); got != c.want {
				t.Errorf("isLocalOnly(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
