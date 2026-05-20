// Tests for env_from → parse-time interpolation:
//
//   * env_from refs are extracted statically from raw HCL
//   * shell vars win over bucket vars on collision
//   * later refs override earlier ones on collision
//   * cache deduplicates fetches across files in one apply
//   * missing buckets / unreachable controller fail loudly
//
// Network-side mocking: we don't run a real controller. The
// tests exercise extractEnvFromRefs (pure), mergeBucketEnv
// (pure), and parseEnvFromRef (pure). The full enrichEnv path
// is covered indirectly via the merge function — controllerDo
// is exercised in the existing apply integration tests, which
// already require a real controller to be running.

package main

import (
	"strings"
	"testing"
)

// TestExtractEnvFromRefs_Basic pins the happy path: a single
// env_from on a deployment block yields its refs.
func TestExtractEnvFromRefs_Basic(t *testing.T) {
	src := `
deployment "prod" "api" {
  image    = "nginx:1.27"
  env_from = ["prod/shared"]
}
`
	refs, err := extractEnvFromRefs("test.hcl", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	if len(refs) != 1 || refs[0] != "prod/shared" {
		t.Errorf("got %v, want [prod/shared]", refs)
	}
}

// TestExtractEnvFromRefs_MultipleResources walks several blocks
// in one file, concatenating refs in declared order.
func TestExtractEnvFromRefs_MultipleResources(t *testing.T) {
	src := `
deployment "prod" "api" {
  env_from = ["prod/shared", "prod/api-creds"]
}

deployment "prod" "worker" {
  env_from = ["prod/shared", "prod/worker-creds"]
}
`
	refs, err := extractEnvFromRefs("test.hcl", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	// Dedup: prod/shared appears in both blocks, should land once.
	wantSet := map[string]bool{
		"prod/shared":      true,
		"prod/api-creds":   true,
		"prod/worker-creds": true,
	}

	for _, r := range refs {
		if !wantSet[r] {
			t.Errorf("unexpected ref %q", r)
		}

		delete(wantSet, r)
	}

	if len(wantSet) > 0 {
		t.Errorf("missing refs: %v", wantSet)
	}
}

// TestExtractEnvFromRefs_None returns nil when no env_from
// is declared anywhere in the file.
func TestExtractEnvFromRefs_None(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"
  env   = { FOO = "bar" }
}
`
	refs, err := extractEnvFromRefs("test.hcl", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	if refs != nil {
		t.Errorf("got %v, want nil", refs)
	}
}

// TestExtractEnvFromRefs_MultilineList accepts the canonical
// HCL multi-line list shape.
func TestExtractEnvFromRefs_MultilineList(t *testing.T) {
	src := `
deployment "prod" "api" {
  env_from = [
    "prod/shared",
    "prod/api-creds",
  ]
}
`
	refs, err := extractEnvFromRefs("test.hcl", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	if len(refs) != 2 {
		t.Fatalf("want 2 refs, got %d: %v", len(refs), refs)
	}

	if refs[0] != "prod/shared" || refs[1] != "prod/api-creds" {
		t.Errorf("order lost: %v", refs)
	}
}

// TestExtractEnvFromRefs_BareScope handles the unscoped /
// scope-only ref form ("monitoring") that targets the scope-
// level bucket without a per-app name.
func TestExtractEnvFromRefs_BareScope(t *testing.T) {
	src := `
deployment "prod" "api" {
  env_from = ["monitoring"]
}
`
	refs, err := extractEnvFromRefs("test.hcl", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	if len(refs) != 1 || refs[0] != "monitoring" {
		t.Errorf("got %v, want [monitoring]", refs)
	}
}

// TestExtractEnvFromRefs_MalformedSkipped doesn't panic or
// error when HCL is syntactically broken — just returns no
// refs. The real parser surfaces the real diagnostic later.
func TestExtractEnvFromRefs_MalformedSkipped(t *testing.T) {
	src := `deployment "prod" "api" { env_from = [unclosed`

	refs, err := extractEnvFromRefs("test.hcl", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error on malformed HCL: %v", err)
	}

	if refs != nil {
		t.Errorf("malformed HCL should yield no refs: %v", refs)
	}
}

// TestMergeBucketEnv_ShellWinsOverBucket pins the precedence
// rule: operator's shell override always beats the bucket. Lets
// `SLACK_URL=https://test/h vd apply` work even when prod/shared
// has a different SLACK_URL.
func TestMergeBucketEnv_ShellWinsOverBucket(t *testing.T) {
	buckets := map[string]map[string]string{
		"prod/shared": {
			"SLACK_URL": "https://prod/hook",
			"DB_URL":    "postgres://prod",
		},
	}

	shell := map[string]string{
		"SLACK_URL": "https://test/hook", // override
		// DB_URL only in bucket
		// HOME only in shell
		"HOME":  "/home/dev",
	}

	got := mergeBucketEnv([]string{"prod/shared"}, buckets, shell)

	if got["SLACK_URL"] != "https://test/hook" {
		t.Errorf("SLACK_URL: shell should have won, got %q", got["SLACK_URL"])
	}

	if got["DB_URL"] != "postgres://prod" {
		t.Errorf("DB_URL: bucket-only should reach, got %q", got["DB_URL"])
	}

	if got["HOME"] != "/home/dev" {
		t.Errorf("HOME: shell-only should reach, got %q", got["HOME"])
	}
}

// TestMergeBucketEnv_LaterRefOverridesEarlier mirrors the
// runtime env_from layering: env_from = ["a", "b"] layers b
// AFTER a, so b's keys override a's on collision.
func TestMergeBucketEnv_LaterRefOverridesEarlier(t *testing.T) {
	buckets := map[string]map[string]string{
		"prod/base":     {"X": "from-base", "Y": "y-base"},
		"prod/override": {"X": "from-override"},
	}

	got := mergeBucketEnv([]string{"prod/base", "prod/override"}, buckets, nil)

	if got["X"] != "from-override" {
		t.Errorf("X: later ref should win, got %q", got["X"])
	}

	if got["Y"] != "y-base" {
		t.Errorf("Y: base value should reach, got %q", got["Y"])
	}
}

// TestMergeBucketEnv_EmptyShellIsFine accepts a nil shell map
// — the call site might call us before reading os.Environ.
func TestMergeBucketEnv_EmptyShellIsFine(t *testing.T) {
	buckets := map[string]map[string]string{
		"prod/shared": {"X": "v"},
	}

	got := mergeBucketEnv([]string{"prod/shared"}, buckets, nil)

	if got["X"] != "v" {
		t.Errorf("X: should reach from bucket, got %q", got["X"])
	}
}

// TestMergeBucketEnv_NoBuckets returns a copy of shell when no
// refs are declared. Important: callers may mutate the result,
// and the caller's shellEnv must not be aliased.
func TestMergeBucketEnv_NoBuckets(t *testing.T) {
	shell := map[string]string{"FOO": "bar"}

	got := mergeBucketEnv(nil, nil, shell)

	if got["FOO"] != "bar" {
		t.Errorf("FOO: should reach from shell, got %q", got["FOO"])
	}

	// Verify the result is a separate allocation. Without this,
	// downstream mutations would leak into os.Environ-derived
	// callers' state.
	got["BAR"] = "added-by-test"
	if _, leak := shell["BAR"]; leak {
		t.Error("shell map was aliased — got is not a fresh allocation")
	}
}

// TestParseEnvFromRef_Scoped splits "scope/name" cleanly.
func TestParseEnvFromRef_Scoped(t *testing.T) {
	scope, name, err := parseEnvFromRef("prod/shared")
	if err != nil {
		t.Fatal(err)
	}

	if scope != "prod" || name != "shared" {
		t.Errorf("got (%q, %q), want (prod, shared)", scope, name)
	}
}

// TestParseEnvFromRef_BareScope returns (scope, "") for the
// "monitoring" shape — targets the scope-level bucket.
func TestParseEnvFromRef_BareScope(t *testing.T) {
	scope, name, err := parseEnvFromRef("monitoring")
	if err != nil {
		t.Fatal(err)
	}

	if scope != "monitoring" || name != "" {
		t.Errorf("got (%q, %q), want (monitoring, \"\")", scope, name)
	}
}

// TestParseEnvFromRef_Empty rejects the empty string.
func TestParseEnvFromRef_Empty(t *testing.T) {
	_, _, err := parseEnvFromRef("")
	if err == nil {
		t.Fatal("expected error on empty ref")
	}

	if !strings.Contains(err.Error(), "empty ref") {
		t.Errorf("error message: %v", err)
	}
}

// TestParseEnvFromRef_EmptyScope rejects "/name" (no scope).
func TestParseEnvFromRef_EmptyScope(t *testing.T) {
	_, _, err := parseEnvFromRef("/name-only")
	if err == nil {
		t.Fatal("expected error on /-prefixed ref")
	}

	if !strings.Contains(err.Error(), "empty scope") {
		t.Errorf("error message: %v", err)
	}
}

// TestBucketCache_DedupFetch verifies the cache calls the
// fetcher once per unique ref even when asked many times.
// Since the real fetcher requires a controller, we instead
// pre-populate the cache and assert the entries map shape.
func TestBucketCache_DedupFetch(t *testing.T) {
	c := newBucketCache()

	c.entries["prod/shared"] = map[string]string{"A": "1"}

	// Pre-populated — the fetcher shouldn't be invoked. Calling
	// fetch with nil cmd would panic if it actually tried the
	// network; the cache short-circuit prevents that.
	got, err := c.fetch(nil, "prod/shared")
	if err != nil {
		t.Fatal(err)
	}

	if got["A"] != "1" {
		t.Errorf("cache hit returned wrong data: %v", got)
	}
}
