package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestRunDeleteFiresDeleteRequests is the happy path: every manifest
// fires a DELETE /apply with the right kind/scope/name. The plan is
// NOT rendered server-side — that lives in the orchestrator, mirroring
// runApply's no-preview shape. runDelete just executes.
func TestRunDeleteFiresDeleteRequests(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "deployment.hcl"), `
deployment "clowk-lp" "web" {
  image = "x:1"
}
`)

	mustWrite(t, filepath.Join(dir, "cronjob.hcl"), `
cronjob "clowk-lp" "crawler1" {
  schedule = "*/5 * * * *"
  image    = "y:1"
}
`)

	var (
		mu      sync.Mutex
		deleted []string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apply" || r.Method != http.MethodDelete {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "boom", http.StatusBadRequest)
			return
		}

		mu.Lock()
		deleted = append(deleted, r.URL.Query().Get("kind")+"/"+
			r.URL.Query().Get("scope")+"/"+r.URL.Query().Get("name"))
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	out := captureStdout(t, func() {
		f := applyFlags{files: []string{dir}}

		if err := runDelete(root, f); err != nil {
			t.Fatalf("runDelete: %v", err)
		}
	})

	// runDelete must NOT render the plan in normal mode — the
	// orchestrator already showed it client-side. Printing it again
	// here would duplicate output through the SSH pipe.
	if strings.Contains(out, "Will delete") {
		t.Errorf("runDelete should not render the plan outside --dry-run; got:\n%s", out)
	}

	// Per-line confirmations are kept — same shape runApply uses.
	for _, want := range []string{
		"deployment/web deleted",
		"cronjob/crawler1 deleted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing per-line confirmation %q in output:\n%s", want, out)
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if len(deleted) != 2 {
		t.Errorf("expected 2 DELETEs, got %d: %v", len(deleted), deleted)
	}
}

// TestRunDeleteDryRunSkipsServerCalls locks in --dry-run: the plan
// renders but no DELETE ever reaches the controller. Without this
// test someone could refactor the dry-run check below the request
// loop and silently break the contract.
func TestRunDeleteDryRunSkipsServerCalls(t *testing.T) {
	t.Setenv("VOODU_AUTO_APPROVE", "")
	t.Setenv("NO_COLOR", "1")

	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "deployment.hcl"), `
deployment "test" "api" {
  image = "x:1"
}
`)

	var hits int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	root := newRootCmd()
	_ = root.PersistentFlags().Set("controller-url", ts.URL)

	out := captureStdout(t, func() {
		f := applyFlags{
			files:  []string{dir},
			dryRun: true,
		}

		if err := runDelete(root, f); err != nil {
			t.Fatalf("runDelete: %v", err)
		}
	})

	if !strings.Contains(out, "Will delete 1 resource") {
		t.Errorf("plan missing in dry-run output:\n%s", out)
	}

	if !strings.Contains(out, "Dry-run") {
		t.Errorf("dry-run banner missing:\n%s", out)
	}

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("dry-run should not contact controller, got %d hits", got)
	}
}

// TestPromptDeleteConfirm exercises the y/N parser. Anything that's
// not a clear "y" / "yes" must be treated as no — the destructive
// default is "don't proceed".
func TestPromptDeleteConfirm(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"y lower", "y\n", true},
		{"Y upper", "Y\n", true},
		{"yes lower", "yes\n", true},
		{"YES upper", "YES\n", true},
		{"yes with surrounding ws", "  yes  \n", true},
		{"empty (just enter)", "\n", false},
		{"n", "n\n", false},
		{"no", "no\n", false},
		{"random word", "maybe\n", false},
		{"EOF (no newline)", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer

			got, err := promptDeleteConfirm(strings.NewReader(c.input), &out)
			if err != nil {
				t.Fatalf("err: %v", err)
			}

			if got != c.want {
				t.Errorf("input %q → got %v, want %v", c.input, got, c.want)
			}

			if !strings.Contains(out.String(), "Delete these resources?") {
				t.Errorf("prompt text missing in output: %q", out.String())
			}
		})
	}
}

// TestExtractDeleteClientFlags locks in the orchestrator's flag
// extraction: -y / --auto-approve / --dry-run are stripped from
// argv (so the forwarded SSH command is clean) and the booleans land
// in the returned flags struct.
func TestExtractDeleteClientFlags(t *testing.T) {
	cases := []struct {
		name      string
		in        []string
		wantFlags deleteClientFlags
		wantArgs  []string
	}{
		{
			name:      "neither flag",
			in:        []string{"delete", "-f", "-", "--format", "json"},
			wantFlags: deleteClientFlags{},
			wantArgs:  []string{"delete", "-f", "-", "--format", "json"},
		},
		{
			name:      "short -y",
			in:        []string{"delete", "-y", "-f", "-"},
			wantFlags: deleteClientFlags{autoApprove: true},
			wantArgs:  []string{"delete", "-f", "-"},
		},
		{
			name:      "long --auto-approve",
			in:        []string{"delete", "--auto-approve", "-f", "-"},
			wantFlags: deleteClientFlags{autoApprove: true},
			wantArgs:  []string{"delete", "-f", "-"},
		},
		{
			name:      "--dry-run",
			in:        []string{"delete", "--dry-run", "-f", "-"},
			wantFlags: deleteClientFlags{dryRun: true},
			wantArgs:  []string{"delete", "-f", "-"},
		},
		{
			name:      "both",
			in:        []string{"delete", "-y", "--dry-run", "-f", "-"},
			wantFlags: deleteClientFlags{autoApprove: true, dryRun: true},
			wantArgs:  []string{"delete", "-f", "-"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotFlags, gotArgs := extractDeleteClientFlags(c.in)

			if gotFlags != c.wantFlags {
				t.Errorf("flags: got %+v, want %+v", gotFlags, c.wantFlags)
			}

			if strings.Join(gotArgs, " ") != strings.Join(c.wantArgs, " ") {
				t.Errorf("args: got %v, want %v", gotArgs, c.wantArgs)
			}
		})
	}
}

// TestIsDeleteCommand mirrors the existing isApplyCommand test: the
// classifier must look past leading flags so `voodu -o json delete`
// still routes to the delete orchestrator.
func TestIsDeleteCommand(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"bare delete", []string{"delete", "-f", "-"}, true},
		{"flag before delete", []string{"-o", "json", "delete", "-f", "-"}, true},
		{"apply does not match", []string{"apply", "-f", "-"}, false},
		{"empty", []string{}, false},
		{"only flags", []string{"-v"}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDeleteCommand(c.in); got != c.want {
				t.Errorf("isDeleteCommand(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
