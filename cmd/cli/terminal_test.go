package main

import (
	"testing"
)

// TestSupportsTruecolor pins the COLORTERM=truecolor signal — the
// de-facto standard for terminals declaring 24-bit support. Default
// is true (truecolor-or-degrade gracefully) so old terminals that
// don't advertise still get colored output that the terminal itself
// downsamples.
func TestSupportsTruecolor(t *testing.T) {
	cases := []struct {
		name      string
		colorterm string
		noColor   string
		want      bool
	}{
		{"COLORTERM=truecolor", "truecolor", "", true},
		{"COLORTERM=24bit", "24bit", "", true},
		{"unset, default true", "", "", true},
		{"NO_COLOR overrides", "truecolor", "1", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("COLORTERM", c.colorterm)
			t.Setenv("NO_COLOR", c.noColor)

			if got := supportsTruecolor(); got != c.want {
				t.Errorf("supportsTruecolor() = %v, want %v", got, c.want)
			}
		})
	}
}
