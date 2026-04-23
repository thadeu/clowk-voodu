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
		{"setup", []string{"setup"}, true},
		{"remote subtree", []string{"remote", "add", "api", "u@h:api"}, true},

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
