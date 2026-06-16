// apply_envfrom.go bridges runtime `env_from` config buckets into
// the CLI's parse-time `${VAR}` interpolation context.
//
// Why this exists:
//
//   Operators store secrets in scope buckets (`vd config set -s
//   prod -n shared SLACK_URL=...`) so multiple resources can
//   share them via `env_from = ["prod/shared"]`. That works
//   beautifully for CONTAINER runtime env — the controller
//   materialises the bucket into an env file the container reads
//   at boot.
//
//   But the manifest's `${VAR}` interpolation is CLIENT-side at
//   parse time, against the operator's shell only. Pre-this
//   feature, an operator wanting `${SLACK_URL}` in their HCL
//   had to export the var locally too, which means every dev
//   on the team needed their own `.envrc` carrying the same
//   secret — a copy-of-the-source-of-truth problem.
//
//   This file closes the gap: when the CLI sees `env_from = [...]`
//   in a manifest, it fetches those buckets from the controller
//   BEFORE doing `${VAR}` interpolation. The bucket vars layer
//   into the same interpolation context the shell already feeds.
//   One source of truth (the bucket), and rotation via
//   `vd config set` propagates to every dev's next `vd apply`
//   automatically.
//
// Precedence (later wins, like the runtime env_from path):
//
//   1. env_from'd bucket vars, in declared order (later refs
//      override earlier ones on the same key — matches how the
//      controller layers --env-file flags at container start).
//   2. operator's shell env (always wins — allows ad-hoc
//      override for testing without touching the bucket).

package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/manifest"
)

// envFromScanner walks raw HCL bytes and extracts every
// `env_from = [...]` attribute it finds at the top level of any
// block. Returns the refs in declared order (across the file),
// deduplicated by string equality.
//
// Implementation note: we use hclsyntax.ParseConfig — the
// LIGHT parse path — which builds a syntax tree without
// evaluating expressions. env_from values are normally pure
// literals (`"prod/shared"`), so reading the values via
// hcl.ExprAsKeyword / attr.Expr static-analysis works without
// requiring interpolation to have happened first.
//
// Pre-`${VAR}` interpolation? Yes intentionally. The whole
// point is to FEED the interpolation context with bucket
// data; if we waited until after interpolation we'd already
// have crashed on unresolved shell-env refs in body fields.
//
// Limitation: env_from values that themselves contain `${VAR}`
// (a weird but legal HCL shape) won't be statically extractable
// here. Document that env_from refs must be literal — same
// posture the existing runtime path already takes.
func extractEnvFromRefs(filename string, raw []byte) ([]string, error) {
	file, _ := hclsyntax.ParseConfig(raw, filename, hcl.Pos{Line: 1, Column: 1})

	// Proceed even when diags has errors. ParseConfig still returns a
	// PARTIAL syntax tree, and we deliberately walk it: voodu's own
	// `${VAR}` / `${VAR:-default}` tokens are NOT valid HCL-native
	// template expressions — the `:-` default is voodu syntax resolved
	// in manifest.Interpolate BEFORE the HCL parser ever runs — so a
	// perfectly valid voodu manifest raises diagnostics here (e.g. a
	// `${FS_CONFIG_DIR:-/default}` in a volumes string). Bailing on the
	// first such diag silently dropped every env_from ref in the file,
	// which then surfaced downstream as a bogus "undefined variable"
	// for vars the env_from bucket would have supplied. env_from values
	// are pure string literals and parse cleanly regardless, so the
	// partial tree still exposes them. A genuinely broken file is
	// reported with a proper diagnostic later by manifest.ParseFile.
	if file == nil {
		return nil, nil
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, nil
	}

	seen := make(map[string]struct{})

	var refs []string

	scanBody := func(b *hclsyntax.Body) {
		for _, attr := range b.Attributes {
			if attr.Name != "env_from" {
				continue
			}

			tuple, ok := attr.Expr.(*hclsyntax.TupleConsExpr)
			if !ok {
				// env_from = something_other_than_a_list — let the
				// real parser report this. We just don't extract.
				continue
			}

			for _, expr := range tuple.Exprs {
				lit, ok := expr.(*hclsyntax.TemplateExpr)
				if !ok {
					continue
				}

				// Pure-literal templates have a single LiteralValueExpr
				// part. Anything with embedded ${...} fails this and we
				// skip — operator gets the runtime "ref not found" error
				// later, which names the offending ref.
				if len(lit.Parts) != 1 {
					continue
				}

				litVal, ok := lit.Parts[0].(*hclsyntax.LiteralValueExpr)
				if !ok {
					continue
				}

				val := litVal.Val
				if val.Type().FriendlyName() != "string" {
					continue
				}

				s := val.AsString()
				if s == "" {
					continue
				}

				if _, dup := seen[s]; dup {
					continue
				}

				seen[s] = struct{}{}

				refs = append(refs, s)
			}
		}
	}

	// Walk top-level blocks; each is a resource (deployment,
	// statefulset, app, etc.). env_from is always a block-level
	// attribute, so a one-level descent covers every case.
	for _, blk := range body.Blocks {
		if blk.Body != nil {
			scanBody(blk.Body)
		}
	}

	return refs, nil
}

// bucketCache memoises configFetch results across a single
// `vd apply` invocation. Applying a 20-file directory where
// every manifest does `env_from = ["prod/shared"]` should
// fetch the bucket once, not 20 times.
//
// Keyed by the raw ref string ("prod/shared", "monitoring",
// etc.); value is the bucket's KV map.
type bucketCache struct {
	entries map[string]map[string]string
}

func newBucketCache() *bucketCache {
	return &bucketCache{entries: map[string]map[string]string{}}
}

// fetch reads one bucket from the controller, using the cache
// on subsequent hits. Empty bucket (key not yet set) returns an
// empty map without error — that matches the runtime semantics
// (env_from to an empty bucket layers nothing).
//
// Errors here are network / 5xx — the operator's manifest
// references config the CLI can't reach, and we'd rather fail
// the apply than silently interpolate against incomplete data.
func (c *bucketCache) fetch(cmd *cobra.Command, ref string) (map[string]string, error) {
	if cached, ok := c.entries[ref]; ok {
		return cached, nil
	}

	scope, name, err := parseEnvFromRef(ref)
	if err != nil {
		return nil, err
	}

	// configFetch wants name="" for the scope-level bucket; we
	// pass the parsed name verbatim (which is "" when the ref is
	// scope-only like "monitoring", or the bucket name like
	// "shared" for "prod/shared").
	vars, err := configFetch(cmd, scope, name, "")
	if err != nil {
		return nil, fmt.Errorf("env_from %q: %w", ref, err)
	}

	c.entries[ref] = vars

	return vars, nil
}

// mergeEnv layers the operator's bucket-sourced vars into the
// shell env map. Shell wins on collision (later layered, last
// write wins — same posture as the runtime env_from path where
// spec.env wins over env_from buckets).
//
// `refs` are honoured in declared order: refs[1] overrides
// refs[0] on shared keys, refs[2] overrides refs[1], etc.
// Matches runtime env_from layering exactly.
//
// The returned map is a fresh allocation — the caller's shellEnv
// map stays untouched (important for multi-file applies where
// each file's enrichment must be independent).
func mergeBucketEnv(refs []string, buckets map[string]map[string]string, shellEnv map[string]string) map[string]string {
	out := make(map[string]string, len(shellEnv)+16)

	for _, ref := range refs {
		for k, v := range buckets[ref] {
			out[k] = v
		}
	}

	// Shell wins. Operator override via `MY_VAR=test vd apply`
	// always beats the bucket. Same shape as runtime: spec.env
	// (the operator's authored override) wins over env_from.
	for k, v := range shellEnv {
		out[k] = v
	}

	return out
}

// enrichEnvFor is the loadOne-callable wrapper. Reads env_from
// refs from raw, fetches via cache, merges with shellEnv,
// returns the merged map ready to feed manifest.ParseFile.
//
// cmd == nil short-circuits to shellEnv unchanged — used by
// tests that exercise pure-shell interpolation.
func enrichEnvFor(cmd *cobra.Command, filename string, raw []byte, shellEnv map[string]string, cache *bucketCache) (map[string]string, error) {
	if cmd == nil {
		return shellEnv, nil
	}

	return enrichEnv(cmd, filename, raw, shellEnv, cache)
}

// enrichEnv combines bucket-sourced and shell-sourced
// interpolation vars for one manifest source. The full pipeline:
//
//   1. extractEnvFromRefs scans the raw bytes for env_from refs
//      (statically, pre-interpolation).
//   2. cache.fetch reads each bucket from the controller, with
//      memoisation across the apply session.
//   3. mergeBucketEnv layers buckets + shell into one map.
//
// Returns the merged map. When no env_from is declared, returns
// shellEnv unchanged (cheap path; no controller round-trip).
//
// Network errors propagate — the operator needs to know when
// the source-of-truth bucket can't be reached, rather than
// silently apply with `${VAR}` resolved to its shell-only value
// (or worse, fail later with "undefined variable").
func enrichEnv(cmd *cobra.Command, filename string, raw []byte, shellEnv map[string]string, cache *bucketCache) (map[string]string, error) {
	if cache == nil {
		cache = newBucketCache()
	}

	refs, err := extractEnvFromRefs(filename, raw)
	if err != nil {
		return nil, err
	}

	if len(refs) == 0 {
		return shellEnv, nil
	}

	buckets := make(map[string]map[string]string, len(refs))

	for _, ref := range refs {
		vars, ferr := cache.fetch(cmd, ref)
		if ferr != nil {
			return nil, ferr
		}

		buckets[ref] = vars
	}

	return mergeBucketEnv(refs, buckets, shellEnv), nil
}

// loadDir walks `root` collecting every manifest-shaped file
// (.hcl/.voodu/.vdu/.vd/.yml/.yaml) and parses each one with its
// own env_from enrichment. The cache passed in is shared across
// the whole apply session — applying a 20-file directory where
// every manifest does `env_from = ["prod/shared"]` fetches that
// bucket once.
//
// Mirrors manifest.ParseDir's walk shape but doesn't reach for
// that function directly — we need to interpose enrichEnvFor
// per file, which ParseDir's signature can't accept.
func loadDir(cmd *cobra.Command, root string, shellEnv map[string]string, cache *bucketCache) ([]controller.Manifest, error) {
	var files []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if _, ferr := manifest.FormatFromExt(path); ferr != nil {
			return nil
		}

		files = append(files, path)

		return nil
	})
	if err != nil {
		return nil, err
	}

	var out []controller.Manifest

	for _, f := range files {
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", f, rerr)
		}

		env, eerr := enrichEnvFor(cmd, f, raw, shellEnv, cache)
		if eerr != nil {
			return nil, eerr
		}

		mans, perr := manifest.ParseFile(f, env)
		if perr != nil {
			return nil, perr
		}

		out = append(out, mans...)
	}

	return out, nil
}

// parseEnvFromRef splits a `scope/name` or bare `scope` ref into
// (scope, name). Mirrors the controller-side parser so the CLI
// resolves refs identically.
func parseEnvFromRef(ref string) (scope, name string, err error) {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			scope = ref[:i]
			name = ref[i+1:]
			if scope == "" {
				return "", "", fmt.Errorf("env_from %q: empty scope before '/'", ref)
			}

			return scope, name, nil
		}
	}

	if ref == "" {
		return "", "", fmt.Errorf("env_from: empty ref")
	}

	return ref, "", nil
}

// Compile-time check that net/http is referenced — keeps the
// import stable when this file evolves to send custom auth
// headers via controllerDo. Without this, gofmt would strip
// the import on the next refactor.
var _ = http.MethodGet
