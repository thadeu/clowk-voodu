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
	"fmt"
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

// TestExtractEnvFromRefs_VooduDefaultSyntax pins the regression where
// a voodu-only `${VAR:-default}` token anywhere in the file raised an
// HCL-native template diagnostic during the light parse, causing the
// extractor to silently drop EVERY env_from ref — which then surfaced
// downstream as a bogus "undefined variable(s)" for the vars the bucket
// would have supplied. The `:-` default is resolved in
// manifest.Interpolate before the HCL parser runs, so its presence must
// not interfere with static env_from extraction.
func TestExtractEnvFromRefs_VooduDefaultSyntax(t *testing.T) {
	src := `
statefulset "fsw" "freeswitch" {
  env_from = ["fsw/freeswitch"]
  image    = "${FS_IMAGE}"
  volumes = [
    "${FS_CONFIG_DIR:-/opt/voodu/volumes/fsw/overlay}:/mnt/fs-config:ro",
  ]
}
`
	refs, err := extractEnvFromRefs("test.voodu", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	if len(refs) != 1 || refs[0] != "fsw/freeswitch" {
		t.Errorf("got %v, want [fsw/freeswitch]", refs)
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

	// Pre-populated — the fetcher shouldn't be invoked. A nil fetcher
	// would panic if the cache actually called it; the short-circuit
	// prevents that.
	got, err := c.fetch(nil, "prod/shared")
	if err != nil {
		t.Fatal(err)
	}

	if got["A"] != "1" {
		t.Errorf("cache hit returned wrong data: %v", got)
	}
}

// TestEnrichEnv_InvokesFetcher pins the core of the SSH-forward fix:
// a declared env_from ref must actually be resolved through the
// injected fetcher and layered into the interpolation context. The
// SSH-forward path regressed precisely because it passed no fetcher,
// so env_from was silently never read and `${FS_IMAGE}` surfaced as
// "undefined variable".
func TestEnrichEnv_InvokesFetcher(t *testing.T) {
	raw := []byte(`statefulset "fsw" "freeswitch" {
  env_from = ["fsw/freeswitch"]
  image    = "${FS_IMAGE}"
}`)

	var seen []string

	fetch := func(ref string) (map[string]string, error) {
		seen = append(seen, ref)

		return map[string]string{"FS_IMAGE": "registry/fsw:bookworm"}, nil
	}

	env, err := enrichEnv(fetch, "fsw.voodu", raw, map[string]string{}, newBucketCache())
	if err != nil {
		t.Fatal(err)
	}

	if len(seen) != 1 || seen[0] != "fsw/freeswitch" {
		t.Errorf("fetcher refs = %v, want [fsw/freeswitch]", seen)
	}

	if env["FS_IMAGE"] != "registry/fsw:bookworm" {
		t.Errorf("FS_IMAGE = %q, want bucket value", env["FS_IMAGE"])
	}
}

// TestExtractResourceRefs pins that every resource block's own
// (scope,name) is surfaced — these are auto-consulted as interpolation
// sources so a resource resolves ${VAR} from its own bucket without
// declaring env_from. Survives voodu's ${VAR:-default} tokens via the
// same partial-parse posture as extractEnvFromRefs.
func TestExtractResourceRefs(t *testing.T) {
	src := `
statefulset "fsw" "freeswitch" {
  image   = "${FS_IMAGE}"
  volumes = ["${FS_CONFIG_DIR:-/opt/x}:/y:ro"]
}

deployment "fsw" "api" {
  image = "nginx:1.27"
}
`
	refs := extractResourceRefs("f.voodu", []byte(src))

	want := []string{"fsw/freeswitch", "fsw/api"}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}

	for i, w := range want {
		if refs[i] != w {
			t.Errorf("refs[%d] = %q, want %q", i, refs[i], w)
		}
	}
}

// TestEnrichEnv_AutoFetchesOwnBucket is the core of the auto-resolve
// feature: with NO env_from declared, ${FS_IMAGE} resolves from the
// resource's own (scope,name) bucket.
func TestEnrichEnv_AutoFetchesOwnBucket(t *testing.T) {
	raw := []byte(`statefulset "fsw" "freeswitch" { image = "${FS_IMAGE}" }`)

	fetch := func(ref string) (map[string]string, error) {
		if ref != "fsw/freeswitch" {
			t.Fatalf("unexpected ref %q", ref)
		}

		return map[string]string{"FS_IMAGE": "registry/fsw:bookworm"}, nil
	}

	env, err := enrichEnv(fetch, "fsw.voodu", raw, map[string]string{}, newBucketCache())
	if err != nil {
		t.Fatal(err)
	}

	if env["FS_IMAGE"] != "registry/fsw:bookworm" {
		t.Errorf("FS_IMAGE = %q, want own-bucket value", env["FS_IMAGE"])
	}
}

// TestEnrichEnv_OwnBucketErrorTolerated pins that a failing own-bucket
// fetch (e.g. the resource has no dedicated bucket) is best-effort: it
// must NOT fail the apply, so auto-resolution never introduces a new
// failure mode. A genuinely needed var stays unresolved and surfaces a
// precise "undefined variable" downstream instead.
func TestEnrichEnv_OwnBucketErrorTolerated(t *testing.T) {
	raw := []byte(`deployment "p" "api" { image = "${X}" }`)

	fetch := func(string) (map[string]string, error) {
		return nil, fmt.Errorf("controller returned 404")
	}

	env, err := enrichEnv(fetch, "api.voodu", raw, map[string]string{}, newBucketCache())
	if err != nil {
		t.Fatalf("own-bucket fetch error must be tolerated, got: %v", err)
	}

	if _, ok := env["X"]; ok {
		t.Errorf("X should be unresolved, got %q", env["X"])
	}
}

// TestEnrichEnv_EnvFromErrorStaysFatal pins the asymmetry: an explicit
// env_from is a contract, so its fetch error remains fatal (unlike the
// implicit own bucket).
func TestEnrichEnv_EnvFromErrorStaysFatal(t *testing.T) {
	raw := []byte(`deployment "p" "api" { env_from = ["other/bucket"] image = "${X}" }`)

	fetch := func(ref string) (map[string]string, error) {
		if ref == "other/bucket" {
			return nil, fmt.Errorf("boom")
		}

		return map[string]string{}, nil
	}

	if _, err := enrichEnv(fetch, "api.voodu", raw, map[string]string{}, newBucketCache()); err == nil {
		t.Fatal("env_from fetch error must be fatal")
	}
}

// TestEnrichEnv_OwnBucketOverridesEnvFrom pins layer precedence: the
// resource's own bucket wins over an inherited env_from on collision,
// matching the runtime ordering in resolveAppEnv.
func TestEnrichEnv_OwnBucketOverridesEnvFrom(t *testing.T) {
	raw := []byte(`deployment "p" "api" { env_from = ["shared/x"] image = "${IMG}" }`)

	fetch := func(ref string) (map[string]string, error) {
		switch ref {
		case "shared/x":
			return map[string]string{"IMG": "from-envfrom"}, nil
		case "p/api":
			return map[string]string{"IMG": "from-own"}, nil
		}

		return map[string]string{}, nil
	}

	env, err := enrichEnv(fetch, "api.voodu", raw, map[string]string{}, newBucketCache())
	if err != nil {
		t.Fatal(err)
	}

	if env["IMG"] != "from-own" {
		t.Errorf("IMG = %q, want from-own (own bucket overrides env_from)", env["IMG"])
	}
}

// TestEnrichEnv_ShellWinsOverBucket pins the documented precedence:
// an operator's shell var overrides the bucket on collision (ad-hoc
// `FS_IMAGE=... vd apply` testing must beat the source-of-truth bucket).
func TestEnrichEnv_ShellWinsOverBucket(t *testing.T) {
	raw := []byte(`deployment "p" "api" { env_from = ["p/shared"] image = "${X}" }`)

	fetch := func(string) (map[string]string, error) {
		return map[string]string{"X": "from-bucket"}, nil
	}

	env, err := enrichEnv(fetch, "api.voodu", raw, map[string]string{"X": "from-shell"}, newBucketCache())
	if err != nil {
		t.Fatal(err)
	}

	if env["X"] != "from-shell" {
		t.Errorf("X = %q, want from-shell (shell must win)", env["X"])
	}
}

// TestBucketFetcherConstructors_NilIsOffline pins that both fetcher
// constructors degrade to nil (offline / shell-only interpolation)
// when handed no transport — the as-is forward path and offline dev
// rely on this.
func TestBucketFetcherConstructors_NilIsOffline(t *testing.T) {
	if cmdBucketFetcher(nil) != nil {
		t.Error("cmdBucketFetcher(nil) should be nil (offline)")
	}

	if sshBucketFetcher(nil, "") != nil {
		t.Error("sshBucketFetcher(nil, \"\") should be nil (offline)")
	}
}
