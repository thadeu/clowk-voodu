package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestParseConfigOutputFormat pins the closed enum of accepted
// -o values plus the case/whitespace tolerance an operator typing
// from muscle memory expects (`-o YAML`, ` -o text `, etc.).
// Unknown values must error — silent fallback to text would mask
// a typo and produce confusing output.
func TestParseConfigOutputFormat(t *testing.T) {
	cases := []struct {
		in      string
		want    configOutputFormat
		wantErr bool
	}{
		{"", configOutputText, false},
		{"text", configOutputText, false},
		{"TEXT", configOutputText, false},
		{"  text  ", configOutputText, false},
		{"json", configOutputJSON, false},
		{"hcl", configOutputHCL, false},
		// yaml was dropped — output was redundant with text
		// (KEY: VALUE vs KEY=VALUE both flat single-line).
		{"yaml", "", true},
		{"yml", "", true},
		{"unknown", "", true},
		{"xml", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseConfigOutputFormat(tc.in)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got %q", tc.in, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tc.want {
				t.Errorf("parseConfigOutputFormat(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRenderConfigVars_TextDefault keeps the legacy KEY=VALUE
// shape — it's the format every operator already knows from
// .env files and the format scripts have been parsing forever.
// Sorted by key for diff-stability.
func TestRenderConfigVars_TextDefault(t *testing.T) {
	vars := map[string]string{
		"REDIS_URL":    "redis://localhost",
		"DATABASE_URL": "postgres://localhost",
		"LOG_LEVEL":    "info",
	}

	var buf bytes.Buffer
	if err := renderConfigVars(&buf, vars, configOutputText); err != nil {
		t.Fatalf("render: %v", err)
	}

	want := "DATABASE_URL=postgres://localhost\n" +
		"LOG_LEVEL=info\n" +
		"REDIS_URL=redis://localhost\n"

	if buf.String() != want {
		t.Errorf("text output:\n--- got:\n%s\n--- want:\n%s", buf.String(), want)
	}
}

// TestRenderConfigVars_JSON checks the JSON output is parseable
// by jq and round-trips through encoding/json. The MarshalIndent
// makes terminal output readable; consumers tolerating compact
// JSON also work.
func TestRenderConfigVars_JSON(t *testing.T) {
	vars := map[string]string{
		"KEY1": "value1",
		"KEY2": "value with spaces",
	}

	var buf bytes.Buffer
	if err := renderConfigVars(&buf, vars, configOutputJSON); err != nil {
		t.Fatalf("render: %v", err)
	}

	// Round-trip through json — guarantees the output is valid
	// JSON, not just text that happens to look right.
	var parsed map[string]string
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("parse own output: %v\noutput:\n%s", err, buf.String())
	}

	if len(parsed) != 2 || parsed["KEY1"] != "value1" || parsed["KEY2"] != "value with spaces" {
		t.Errorf("round-trip mismatch: %+v", parsed)
	}
}

// TestRenderConfigVars_HCL pins the `env = { KEY = "value" }`
// shape. Direct paste into a manifest's spec.env block is the
// primary use case, so we verify the wrapper + structure.
func TestRenderConfigVars_HCL(t *testing.T) {
	vars := map[string]string{
		"REDIS_URL":    "redis://localhost",
		"DATABASE_URL": "postgres://localhost",
	}

	var buf bytes.Buffer
	if err := renderConfigVars(&buf, vars, configOutputHCL); err != nil {
		t.Fatalf("render: %v", err)
	}

	got := buf.String()

	if !strings.HasPrefix(got, "env = {\n") {
		t.Errorf("output should start with env block opener; got:\n%s", got)
	}

	if !strings.HasSuffix(got, "}\n") {
		t.Errorf("output should end with closing brace; got:\n%s", got)
	}

	// Sorted keys — DATABASE_URL appears before REDIS_URL.
	dbIdx := strings.Index(got, "DATABASE_URL")
	redisIdx := strings.Index(got, "REDIS_URL")

	if dbIdx < 0 || redisIdx < 0 || dbIdx > redisIdx {
		t.Errorf("HCL output not sorted by key; got:\n%s", got)
	}

	// Values are quoted (HCL string literal).
	if !strings.Contains(got, `REDIS_URL = "redis://localhost"`) {
		t.Errorf("REDIS_URL value not properly quoted; got:\n%s", got)
	}
}

// TestRenderConfigVars_HCL_EscapesQuotes: env values containing
// double quotes or backslashes must be escaped per HCL2's string
// literal grammar — operators paste this directly into manifests,
// and a raw `"` in the value would break parsing.
func TestRenderConfigVars_HCL_EscapesQuotes(t *testing.T) {
	vars := map[string]string{
		"WITH_QUOTE":    `value with "embedded" quote`,
		"WITH_BACKSLASH": `path\to\file`,
		"MULTILINE":      "line1\nline2",
	}

	var buf bytes.Buffer
	if err := renderConfigVars(&buf, vars, configOutputHCL); err != nil {
		t.Fatalf("render: %v", err)
	}

	got := buf.String()

	// Embedded quote escaped as \"
	if !strings.Contains(got, `WITH_QUOTE = "value with \"embedded\" quote"`) {
		t.Errorf("embedded quote not escaped; got:\n%s", got)
	}

	// Backslash doubled
	if !strings.Contains(got, `WITH_BACKSLASH = "path\\to\\file"`) {
		t.Errorf("backslash not escaped; got:\n%s", got)
	}

	// Newline becomes \n literal
	if !strings.Contains(got, `MULTILINE = "line1\nline2"`) {
		t.Errorf("newline not escaped; got:\n%s", got)
	}
}

// TestRenderConfigVars_EmptyMap: each format handles "no vars
// set" sensibly. Text emits nothing (empty list of KEY=VAL is
// still a valid env file — empty); JSON emits {}; HCL emits an
// empty env block.
func TestRenderConfigVars_EmptyMap(t *testing.T) {
	cases := []struct {
		format configOutputFormat
		want   string
	}{
		{configOutputText, ""},
		{configOutputJSON, "{}\n"},
		{configOutputHCL, "env = {\n}\n"},
	}

	for _, tc := range cases {
		t.Run(string(tc.format), func(t *testing.T) {
			var buf bytes.Buffer
			if err := renderConfigVars(&buf, map[string]string{}, tc.format); err != nil {
				t.Fatalf("render: %v", err)
			}

			if buf.String() != tc.want {
				t.Errorf("empty %s output: got %q, want %q", tc.format, buf.String(), tc.want)
			}
		})
	}
}
