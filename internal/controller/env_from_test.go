package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/paths"
)

// TestResolveEnvFromRef pins the parser surface for env_from
// entries. Three accepted shapes — bare name (same scope),
// scope/name (cross-scope), and AppID literal (escape hatch) —
// all resolve to the same env file path via paths.AppEnvFile.
func TestResolveEnvFromRef(t *testing.T) {
	cases := []struct {
		name        string
		ref         string
		callerScope string
		want        string
		wantErr     bool
	}{
		{
			name:        "bare name resolves to caller's scope",
			ref:         "web",
			callerScope: "clowk-lp",
			want:        paths.AppEnvFile("clowk-lp-web"),
		},
		{
			name:        "scope/name shape — cross-scope reference",
			ref:         "secrets/aws",
			callerScope: "clowk-lp",
			want:        paths.AppEnvFile("secrets-aws"),
		},
		{
			name:        "AppID literal — bare name treated as same-scope ref",
			ref:         "clowk-lp-web",
			callerScope: "clowk-lp",
			// Same-scope shape, so AppID(clowk-lp, clowk-lp-web).
			// Operator who actually wants the literal AppID can
			// also write `<scope>/<name>` explicitly.
			want: paths.AppEnvFile("clowk-lp-clowk-lp-web"),
		},
		{
			name:    "empty ref errors",
			ref:     "",
			wantErr: true,
		},
		{
			name:    "scope/name with empty scope errors",
			ref:     "/web",
			wantErr: true,
		},
		{
			name:    "scope/name with empty name errors",
			ref:     "scope/",
			wantErr: true,
		},
		{
			name:        "whitespace trimmed",
			ref:         "  web  ",
			callerScope: "clowk-lp",
			want:        paths.AppEnvFile("clowk-lp-web"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveEnvFromRef(tc.ref, tc.callerScope)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for ref %q, got path %q", tc.ref, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tc.want {
				t.Errorf("resolveEnvFromRef(%q, %q) = %q, want %q", tc.ref, tc.callerScope, got, tc.want)
			}
		})
	}
}

// TestResolveEnvFromList_HappyPath confirms the list-of-refs
// translation: each ref resolves, files exist on disk, the
// returned slice preserves declared order so docker last-wins
// semantics match what the operator wrote.
func TestResolveEnvFromList_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("VOODU_ROOT", tmp)

	// Pre-create the env files the refs will resolve to.
	mustTouch(t, filepath.Join(tmp, "apps", "secrets-aws", "shared", ".env"))
	mustTouch(t, filepath.Join(tmp, "apps", "clowk-lp-web", "shared", ".env"))

	got, err := resolveEnvFromList(
		[]string{"secrets/aws", "web"},
		"clowk-lp",
		nil,
	)
	if err != nil {
		t.Fatalf("resolveEnvFromList: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 paths, got %d: %v", len(got), got)
	}

	// Order preserved: env_from[0] is first in returned slice.
	if !strings.HasSuffix(got[0], "/secrets-aws/shared/.env") {
		t.Errorf("got[0] = %q, want ends with secrets-aws/.env", got[0])
	}

	if !strings.HasSuffix(got[1], "/clowk-lp-web/shared/.env") {
		t.Errorf("got[1] = %q, want ends with clowk-lp-web/.env", got[1])
	}
}

// TestResolveEnvFromList_SkipsMissingFiles: a missing file in
// the middle of the list is logged and skipped (apply order is
// not enforced — operator might apply pieces incrementally).
// The remaining files still resolve.
func TestResolveEnvFromList_SkipsMissingFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("VOODU_ROOT", tmp)

	mustTouch(t, filepath.Join(tmp, "apps", "clowk-lp-web", "shared", ".env"))
	// "secrets-aws" intentionally NOT created.

	var logged []string
	logf := func(format string, args ...any) {
		logged = append(logged, format)
	}

	got, err := resolveEnvFromList(
		[]string{"secrets/aws", "web"},
		"clowk-lp",
		logf,
	)
	if err != nil {
		t.Fatalf("resolveEnvFromList: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 path (missing skipped), got %d: %v", len(got), got)
	}

	if !strings.HasSuffix(got[0], "/clowk-lp-web/shared/.env") {
		t.Errorf("got[0] = %q, want ends with clowk-lp-web/.env", got[0])
	}

	// Log entry confirms the operator gets visibility into
	// what was skipped — silent skip would be a debug nightmare.
	foundWarning := false
	for _, msg := range logged {
		if strings.Contains(msg, "secrets/aws") || strings.Contains(msg, "skipping") {
			foundWarning = true
			break
		}
	}

	if !foundWarning {
		t.Errorf("expected log entry mentioning the skipped ref; got %v", logged)
	}
}

// TestResolveEnvFromList_AllMissingErrors: when every ref's
// file is missing, return error — there's no env to provide,
// running with zero --env-files would silently strip the
// inheritance the operator was relying on.
func TestResolveEnvFromList_AllMissingErrors(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("VOODU_ROOT", tmp)

	// No files seeded.

	_, err := resolveEnvFromList(
		[]string{"secrets/aws", "web"},
		"clowk-lp",
		nil,
	)

	if err == nil {
		t.Fatal("expected error when all env_from refs miss")
	}

	if !strings.Contains(err.Error(), "no env files resolved") {
		t.Errorf("error should mention no env files resolved, got %q", err.Error())
	}
}

// TestResolveEnvFromList_EmptyInput: nil/empty input is the
// "no env_from declared" path — return nil, no error. Caller
// uses the absence to skip the --env-file fan-out.
func TestResolveEnvFromList_EmptyInput(t *testing.T) {
	got, err := resolveEnvFromList(nil, "clowk-lp", nil)
	if err != nil {
		t.Errorf("nil input: unexpected error: %v", err)
	}

	if got != nil {
		t.Errorf("nil input should return nil, got %v", got)
	}

	got, err = resolveEnvFromList([]string{}, "clowk-lp", nil)
	if err != nil {
		t.Errorf("empty slice: unexpected error: %v", err)
	}

	if got != nil {
		t.Errorf("empty slice should return nil, got %v", got)
	}
}

// mustTouch creates an empty file at path (with parent dirs).
// Test helper for seeding env files.
func mustTouch(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("touch: %v", err)
	}
}
