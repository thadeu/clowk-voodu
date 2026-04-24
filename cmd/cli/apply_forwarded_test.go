package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRewriteApplyToDiffJSON(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "basic apply to diff plus -o json",
			in:   []string{"apply", "-f", "-", "--format", "json"},
			want: []string{"diff", "-f", "-", "--format", "json", "-o", "json"},
		},
		{
			name: "strips -y and --auto-approve",
			in:   []string{"apply", "-y", "-f", "-", "--format", "json", "--auto-approve"},
			want: []string{"diff", "-f", "-", "--format", "json", "-o", "json"},
		},
		{
			name: "overrides user-provided -o yaml with -o json",
			in:   []string{"apply", "-o", "yaml", "-f", "-", "--format", "json"},
			want: []string{"diff", "-f", "-", "--format", "json", "-o", "json"},
		},
		{
			name: "overrides --output=text",
			in:   []string{"apply", "--output=text", "-f", "-", "--format", "json"},
			want: []string{"diff", "-f", "-", "--format", "json", "-o", "json"},
		},
		{
			name: "keeps --no-prune and --detailed-exitcode through",
			in:   []string{"apply", "--no-prune", "-f", "-", "--format", "json"},
			want: []string{"diff", "--no-prune", "-f", "-", "--format", "json", "-o", "json"},
		},
		{
			name: "strips --force (apply-only, no-op on diff)",
			in:   []string{"apply", "--force", "-f", "-", "--format", "json"},
			want: []string{"diff", "-f", "-", "--format", "json", "-o", "json"},
		},
		{
			name: "strips --force and -y together",
			in:   []string{"apply", "--force", "-y", "-f", "-", "--format", "json"},
			want: []string{"diff", "-f", "-", "--format", "json", "-o", "json"},
		},
		{
			name: "strips -v / --verbose (apply-only)",
			in:   []string{"apply", "-v", "--verbose", "-f", "-", "--format", "json"},
			want: []string{"diff", "-f", "-", "--format", "json", "-o", "json"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rewriteApplyToDiffJSON(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("rewriteApplyToDiffJSON(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestExtractApplyClientFlags(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		want     applyClientFlags
		wantRest []string
	}{
		{
			name:     "no client-only flags",
			in:       []string{"apply", "-f", "-"},
			want:     applyClientFlags{},
			wantRest: []string{"apply", "-f", "-"},
		},
		{
			name:     "-y short form",
			in:       []string{"apply", "-y", "-f", "-"},
			want:     applyClientFlags{autoApprove: true},
			wantRest: []string{"apply", "-f", "-"},
		},
		{
			name:     "--auto-approve long form",
			in:       []string{"apply", "--auto-approve", "-f", "-"},
			want:     applyClientFlags{autoApprove: true},
			wantRest: []string{"apply", "-f", "-"},
		},
		{
			name:     "--force alone",
			in:       []string{"apply", "--force", "-f", "-"},
			want:     applyClientFlags{force: true},
			wantRest: []string{"apply", "-f", "-"},
		},
		{
			name:     "--force and -y together",
			in:       []string{"apply", "-y", "--force", "-f", "-"},
			want:     applyClientFlags{autoApprove: true, force: true},
			wantRest: []string{"apply", "-f", "-"},
		},
		{
			name:     "both --auto-approve and --force (idempotent, order preserved)",
			in:       []string{"apply", "--auto-approve", "-f", "infra/dev", "--force"},
			want:     applyClientFlags{autoApprove: true, force: true},
			wantRest: []string{"apply", "-f", "infra/dev"},
		},
		{
			name:     "-v short form",
			in:       []string{"apply", "-v", "-f", "-"},
			want:     applyClientFlags{verbose: true},
			wantRest: []string{"apply", "-f", "-"},
		},
		{
			name:     "--verbose long form",
			in:       []string{"apply", "--verbose", "-f", "-"},
			want:     applyClientFlags{verbose: true},
			wantRest: []string{"apply", "-f", "-"},
		},
		{
			name:     "all three flags together",
			in:       []string{"apply", "-y", "--force", "--verbose", "-f", "infra/dev"},
			want:     applyClientFlags{autoApprove: true, force: true, verbose: true},
			wantRest: []string{"apply", "-f", "infra/dev"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rest := extractApplyClientFlags(c.in)
			if got != c.want {
				t.Errorf("flags = %+v, want %+v", got, c.want)
			}

			if !reflect.DeepEqual(rest, c.wantRest) {
				t.Errorf("rest = %v, want %v", rest, c.wantRest)
			}
		})
	}
}

func TestIsApplyCommand(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"bare apply", []string{"apply"}, true},
		{"apply with flags", []string{"apply", "-f", "stack.hcl"}, true},
		{"apply behind global flag", []string{"-o", "json", "apply", "-f", "-"}, true},
		{"diff (not apply)", []string{"diff", "-f", "stack.hcl"}, false},
		{"delete (not apply)", []string{"delete", "-f", "stack.hcl"}, false},
		{"empty argv", nil, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isApplyCommand(c.in); got != c.want {
				t.Errorf("isApplyCommand(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestEnvAutoApprove(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"bananas", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"YES", true},
		{" 1 ", true},
	}

	for _, c := range cases {
		t.Run("val="+c.val, func(t *testing.T) {
			t.Setenv("VOODU_AUTO_APPROVE", c.val)

			if got := envAutoApprove(); got != c.want {
				t.Errorf("envAutoApprove(%q) = %v, want %v", c.val, got, c.want)
			}
		})
	}
}

func TestPromptConfirm(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"YeS\n", true},
		{"\n", false},
		{"n\n", false},
		{"no\n", false},
		{"apply\n", false},
		{"", false}, // EOF with no input
	}

	for _, c := range cases {
		t.Run("input="+strings.TrimSpace(c.input), func(t *testing.T) {
			var out bytes.Buffer

			got, err := promptConfirm(strings.NewReader(c.input), &out)
			if err != nil {
				t.Fatalf("err = %v", err)
			}

			if got != c.want {
				t.Errorf("promptConfirm(%q) = %v, want %v", c.input, got, c.want)
			}

			if !strings.Contains(out.String(), "[y/N]") {
				t.Errorf("prompt missing [y/N] marker: %q", out.String())
			}
		})
	}
}

func TestPlanChangeCount(t *testing.T) {
	// No changes at all: same applied and current, no pruned.
	empty := diffResponse{}
	empty.Data.Applied = nil
	empty.Data.Current = nil
	empty.Data.Pruned = nil

	if got := planChangeCount(empty); got != 0 {
		t.Errorf("empty plan = %d, want 0", got)
	}

	// One unchanged resource — identical specs both sides.
	unchanged := diffResponse{}

	err := json.Unmarshal([]byte(`{"data":{
		"applied":[{"kind":"deployment","scope":"s","name":"n","spec":{"image":"a"}}],
		"current":[{"kind":"deployment","scope":"s","name":"n","spec":{"image":"a"}}]
	}}`), &unchanged)
	if err != nil {
		t.Fatal(err)
	}

	if got := planChangeCount(unchanged); got != 0 {
		t.Errorf("unchanged plan = %d, want 0", got)
	}

	// One new resource (current is nil for that index).
	newOne := diffResponse{}

	err = json.Unmarshal([]byte(`{"data":{
		"applied":[{"kind":"deployment","scope":"s","name":"n","spec":{"image":"a"}}],
		"current":[null]
	}}`), &newOne)
	if err != nil {
		t.Fatal(err)
	}

	if got := planChangeCount(newOne); got != 1 {
		t.Errorf("new-resource plan = %d, want 1", got)
	}

	// Modified + pruned combo.
	mixed := diffResponse{}

	err = json.Unmarshal([]byte(`{"data":{
		"applied":[{"kind":"deployment","scope":"s","name":"n","spec":{"image":"b"}}],
		"current":[{"kind":"deployment","scope":"s","name":"n","spec":{"image":"a"}}],
		"pruned":["deployment/s/old"]
	}}`), &mixed)
	if err != nil {
		t.Fatal(err)
	}

	if got := planChangeCount(mixed); got != 2 {
		t.Errorf("mixed plan = %d, want 2", got)
	}
}

// TestRunDiffEmitsJSON locks in the -o json contract: the diff command
// emits the raw plan envelope verbatim (no ANSI, no renderer), so the
// apply-forwarded orchestrator on the client can parse it. The stub
// server returns a canonical dry-run response and we assert the CLI's
// stdout round-trips cleanly into diffResponse.
func TestRunDiffEmitsJSON(t *testing.T) {
	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "deployment.hcl"), `
deployment "clowk" "lp" {
  image = "nginx:1.27"
}
`)

	reply := `{"status":"ok","data":{
		"applied":[{"kind":"deployment","scope":"clowk","name":"lp","spec":{"image":"nginx:1.27"}}],
		"current":[{"kind":"deployment","scope":"clowk","name":"lp","spec":{"image":"nginx:1.26"}}],
		"pruned":[],
		"dry_run":true
	}}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(reply))
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)
	_ = root.PersistentFlags().Set("output", "json")

	cmd, _, err := root.Find([]string{"diff"})
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	cmd.SetOut(&stdout)

	if err := runDiff(cmd, applyFlags{files: []string{dir}}); err != nil {
		t.Fatalf("runDiff: %v", err)
	}

	// Output must be pure JSON — no ANSI escapes, no "1 to modify"
	// summary line.
	out := stdout.String()

	if strings.Contains(out, "\x1b[") {
		t.Errorf("json output must not contain ANSI escapes: %q", out)
	}

	if strings.Contains(out, "to modify") {
		t.Errorf("json output must not contain human summary: %q", out)
	}

	var decoded diffResponse
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out)
	}

	if len(decoded.Data.Applied) != 1 {
		t.Errorf("applied count = %d, want 1", len(decoded.Data.Applied))
	}

	if len(decoded.Data.Current) != 1 {
		t.Errorf("current count = %d, want 1", len(decoded.Data.Current))
	}
}

// TestRunDiffJSONWithDetailedExitcode verifies JSON mode still honours
// the exit-code contract. Terraform `plan -detailed-exitcode | jq`
// users rely on this — the pipeline should both see structured data
// AND know from $? whether the plan had changes.
func TestRunDiffJSONWithDetailedExitcode(t *testing.T) {
	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "deployment.hcl"), `
deployment "clowk" "lp" {
  image = "nginx:1"
}
`)

	var replyJSON string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(replyJSON))
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)
	_ = root.PersistentFlags().Set("output", "json")

	cmd, _, _ := root.Find([]string{"diff"})

	// Case: changes pending — JSON body emitted AND errExitWithChanges
	// returned (main() maps to exit 2).
	replyJSON = `{"status":"ok","data":{
		"applied":[{"kind":"deployment","scope":"clowk","name":"lp","spec":{"image":"nginx:1"}}],
		"current":[{"kind":"deployment","scope":"clowk","name":"lp","spec":{"image":"nginx:0"}}],
		"pruned":[]
	}}`

	var out bytes.Buffer
	cmd.SetOut(&out)

	err := runDiff(cmd, applyFlags{files: []string{dir}, detailedExitcode: true})
	if err == nil || err.Error() != errExitWithChanges.Error() {
		t.Errorf("expected errExitWithChanges, got %v", err)
	}

	if !strings.Contains(out.String(), `"applied"`) {
		t.Errorf("JSON body missing from stdout: %q", out.String())
	}
}

// TestRunDiffYAMLEmitsMachineReadable is the symmetric YAML case —
// same plan, different encoder. Looser assertion: just ensure YAML
// shape (keys visible as `key: value`) and no ANSI.
func TestRunDiffYAMLEmitsMachineReadable(t *testing.T) {
	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "d.hcl"), `deployment "s" "n" { image = "x" }`+"\n")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","data":{
			"applied":[{"kind":"deployment","scope":"s","name":"n","spec":{"image":"x"}}],
			"current":[null],
			"pruned":[]
		}}`))
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)
	_ = root.PersistentFlags().Set("output", "yaml")

	cmd, _, _ := root.Find([]string{"diff"})

	var out bytes.Buffer
	cmd.SetOut(&out)

	if err := runDiff(cmd, applyFlags{files: []string{dir}}); err != nil {
		t.Fatalf("runDiff: %v", err)
	}

	s := out.String()

	if strings.Contains(s, "\x1b[") {
		t.Errorf("yaml output must not contain ANSI escapes: %q", s)
	}

	for _, want := range []string{"applied:", "current:", "kind: deployment"} {
		if !strings.Contains(s, want) {
			t.Errorf("yaml output missing %q:\n%s", want, s)
		}
	}
}

