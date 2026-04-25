package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/controller"
)

func TestParseHCLDeployment(t *testing.T) {
	src := `
deployment "test" "api" {
  image = "nginx:${TAG:-1}"
  replicas = 3
  ports = ["8080", "8443"]
}
`
	tmp := writeTemp(t, "dep.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	if mans[0].Kind != controller.KindDeployment || mans[0].Name != "api" {
		t.Errorf("unexpected header: %+v", mans[0])
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Image != "nginx:1" {
		t.Errorf("interpolation failed: %q", spec.Image)
	}

	if spec.Replicas != 3 {
		t.Errorf("replicas: got %d", spec.Replicas)
	}

	if len(spec.Ports) != 2 {
		t.Errorf("ports: %+v", spec.Ports)
	}
}

func TestParseHCLDeploymentBuildMode(t *testing.T) {
	// The pipeline/container concerns stay at root (workdir, path,
	// ports, env, post_deploy, ...) while runtime build inputs live
	// inside the unified `lang {}` block. Go-specific cross-compile
	// flags (GOOS/GOARCH/CGO_ENABLED) ride inside build_args.
	src := `
deployment "test" "api" {
  workdir      = "apps/esl"
  dockerfile   = "Dockerfile.api"
  path         = "cmd/api"
  ports        = ["127.0.0.1:9092:9092"]
  volumes      = ["/opt/gokku/volumes/rtp:/app/recordings"]
  network      = "bridge"
  restart      = "unless-stopped"
  env          = { RAILS_ENV = "production" }
  post_deploy  = ["./bin/migrate"]
  health_check = "/healthz"

  lang {
    name    = "go"
    version = "1.25"
    build_args = {
      GOOS        = "linux"
      GOARCH      = "amd64"
      CGO_ENABLED = "0"
      GIT_SHA     = "abc123"
    }
  }
}
`
	tmp := writeTemp(t, "build.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Image != "" {
		t.Errorf("image should be empty in build mode, got %q", spec.Image)
	}

	if spec.Workdir != "apps/esl" || spec.Dockerfile != "Dockerfile.api" || spec.Path != "cmd/api" {
		t.Errorf("source fields not carried: %+v", spec)
	}

	if spec.Lang == nil {
		t.Fatal("lang block lost")
	}

	if spec.Lang.Name != "go" || spec.Lang.Version != "1.25" {
		t.Errorf("lang block fields lost: %+v", spec.Lang)
	}

	if spec.Lang.BuildArgs["GOOS"] != "linux" || spec.Lang.BuildArgs["GIT_SHA"] != "abc123" {
		t.Errorf("build_args lost: %+v", spec.Lang.BuildArgs)
	}

	if len(spec.PostDeploy) != 1 || spec.PostDeploy[0] != "./bin/migrate" {
		t.Errorf("post_deploy: %+v", spec.PostDeploy)
	}

	if spec.HealthCheck != "/healthz" || spec.Restart != "unless-stopped" || spec.Network != "bridge" {
		t.Errorf("runtime fields: %+v", spec)
	}
}

func TestParseHCLDeploymentLangBlockExotic(t *testing.T) {
	// The lang block accepts any name string — platforms the handler
	// registry doesn't know about (Elixir, Java, Haskell) still land
	// cleanly in the spec; handler dispatch at build time falls through
	// to the generic Dockerfile path.
	src := `
deployment "test" "api" {
  lang {
    name    = "elixir"
    version = "1.17"
    build_args = {
      MIX_ENV = "prod"
    }
  }
}
`
	tmp := writeTemp(t, "exotic.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Lang == nil || spec.Lang.Name != "elixir" || spec.Lang.Version != "1.17" {
		t.Errorf("exotic lang not carried: %+v", spec.Lang)
	}

	if spec.Lang.BuildArgs["MIX_ENV"] != "prod" {
		t.Errorf("build_args lost: %+v", spec.Lang.BuildArgs)
	}
}

func TestParseHCLDeploymentImageOptional(t *testing.T) {
	// Minimal build-mode deployment: no image, no path either. Parser
	// fills in `path="."` only — Dockerfile stays empty so lang handlers
	// can auto-resolve (use existing ./Dockerfile, else generate). A
	// forced default here would push the server-side pipeline down the
	// "custom Dockerfile" error path when the file isn't present,
	// blocking zero-config Rails/Ruby/Node builds.
	src := `deployment "test" "api" {}`

	tmp := writeTemp(t, "bare.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatalf("image should be optional for build mode: %v", err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Image != "" {
		t.Errorf("image should stay empty: %+v", spec)
	}

	if spec.Path != "." {
		t.Errorf("path default not applied: got %q, want %q", spec.Path, ".")
	}

	if spec.Dockerfile != "" {
		t.Errorf("dockerfile should stay empty so handlers can auto-resolve: got %q", spec.Dockerfile)
	}

	if spec.Workdir != "" {
		t.Errorf("workdir should stay empty (no default): %+v", spec)
	}
}

func TestParseHCLMultiKind(t *testing.T) {
	src := `
deployment "test" "api" {
  image = "a:1"
}
database "main" {
  engine = "postgres"
}
ingress "test" "api" {
  host    = "api.example.com"
  service = "api"
}
`
	tmp := writeTemp(t, "stack.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 3 {
		t.Fatalf("want 3 manifests, got %d", len(mans))
	}
}

// TestParseHCLAppExpandsToDeploymentAndIngress locks in the authoring
// sugar contract: `app "scope" "name" { … }` produces exactly two
// manifests, one deployment and one ingress, sharing the same
// identity. The runtime never sees an "app" — it sees the canonical
// pair — so describe/diff/prune/--force keep working without
// special-casing the block shape.
func TestParseHCLAppExpandsToDeploymentAndIngress(t *testing.T) {
	src := `
app "clowk-lp" "web" {
  replicas = 1
  ports    = ["3000"]

  lang {
    name    = "nodejs"
    version = "22"
  }

  host = "vd-web.lvh.me"

  tls {
    enabled  = true
    provider = "internal"
  }
}
`
	tmp := writeTemp(t, "app.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 2 {
		t.Fatalf("want 2 manifests (deployment+ingress), got %d", len(mans))
	}

	// Order is deterministic — apps are emitted before standalone
	// blocks, deployment before ingress within the pair.
	dep, ing := mans[0], mans[1]

	if dep.Kind != controller.KindDeployment || dep.Scope != "clowk-lp" || dep.Name != "web" {
		t.Errorf("deployment header wrong: %+v", dep)
	}

	if ing.Kind != controller.KindIngress || ing.Scope != "clowk-lp" || ing.Name != "web" {
		t.Errorf("ingress header wrong: %+v", ing)
	}

	var depSpec DeploymentSpec
	if err := json.Unmarshal(dep.Spec, &depSpec); err != nil {
		t.Fatal(err)
	}

	if depSpec.Replicas != 1 {
		t.Errorf("replicas not carried: %d", depSpec.Replicas)
	}

	if len(depSpec.Ports) != 1 || depSpec.Ports[0] != "3000" {
		t.Errorf("ports not carried: %+v", depSpec.Ports)
	}

	if depSpec.Lang == nil || depSpec.Lang.Name != "nodejs" || depSpec.Lang.Version != "22" {
		t.Errorf("lang block lost: %+v", depSpec.Lang)
	}

	// Path defaults must run on the app-emitted deployment too — same
	// applyDefaults() that `deployment` blocks get. Otherwise an app
	// authored deployment would diverge from a hand-written one.
	if depSpec.Path != "." {
		t.Errorf("default path not applied to app deployment: %q", depSpec.Path)
	}

	var ingSpec IngressSpec
	if err := json.Unmarshal(ing.Spec, &ingSpec); err != nil {
		t.Fatal(err)
	}

	if ingSpec.Host != "vd-web.lvh.me" {
		t.Errorf("host not carried: %q", ingSpec.Host)
	}

	// service/port omitted on purpose: the controller derives them
	// from the sibling deployment at reconcile time. If we filled
	// them in here we'd freeze a stale answer; leave the zero values
	// so the auto-resolution path runs.
	if ingSpec.Service != "" {
		t.Errorf("service should stay empty for auto-derive, got %q", ingSpec.Service)
	}

	if ingSpec.Port != 0 {
		t.Errorf("port should stay zero for auto-derive, got %d", ingSpec.Port)
	}

	if ingSpec.TLS == nil || !ingSpec.TLS.Enabled || ingSpec.TLS.Provider != "internal" {
		t.Errorf("tls block lost: %+v", ingSpec.TLS)
	}
}

// TestParseHCLAppCollidesWithStandaloneDeployment guards the most
// common authoring mistake: writing `app "x" "y"` and then also
// declaring `deployment "x" "y"` thinking the latter overrides the
// former. The parser surfaces it as a duplicate-identity error so
// the user fixes the file instead of debugging a silent merge.
func TestParseHCLAppCollidesWithStandaloneDeployment(t *testing.T) {
	src := `
app "clowk-lp" "web" {
  host = "vd-web.lvh.me"
  ports = ["3000"]
}

deployment "clowk-lp" "web" {
  image = "nginx:1"
}
`
	tmp := writeTemp(t, "collision.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected duplicate identity error, got nil")
	}

	if !strings.Contains(err.Error(), "duplicate identity") || !strings.Contains(err.Error(), "deployment/clowk-lp/web") {
		t.Errorf("error should name the colliding tuple, got: %v", err)
	}
}

// TestParseHCLAppCollidesWithStandaloneIngress mirrors the deployment
// collision path on the ingress side — both halves of the pair are
// load-bearing for the duplicate check.
func TestParseHCLAppCollidesWithStandaloneIngress(t *testing.T) {
	src := `
app "clowk-lp" "web" {
  host = "vd-web.lvh.me"
}

ingress "clowk-lp" "web" {
  host    = "alt.lvh.me"
  service = "web"
}
`
	tmp := writeTemp(t, "collision.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected duplicate identity error, got nil")
	}

	if !strings.Contains(err.Error(), "ingress/clowk-lp/web") {
		t.Errorf("error should name the ingress tuple, got: %v", err)
	}
}

// TestParseHCLDuplicateStandaloneDeployment makes the duplicate check
// a general invariant — it would have been silently last-wins before
// this guard. App is the motivating case but the rule applies
// uniformly.
func TestParseHCLDuplicateStandaloneDeployment(t *testing.T) {
	src := `
deployment "test" "api" {
  image = "nginx:1"
}

deployment "test" "api" {
  image = "nginx:2"
}
`
	tmp := writeTemp(t, "dup.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected duplicate identity error, got nil")
	}

	if !strings.Contains(err.Error(), "deployment/test/api") {
		t.Errorf("error should name the deployment tuple, got: %v", err)
	}
}

// TestParseHCLAppDistinctIdentitiesCoexist ensures the duplicate check
// doesn't false-positive: an `app` and a `deployment` with different
// names (or scopes) in the same file must parse fine. Otherwise users
// can't have an app + a sidecar deployment in the same project.
func TestParseHCLAppDistinctIdentitiesCoexist(t *testing.T) {
	src := `
app "clowk-lp" "web" {
  host = "vd-web.lvh.me"
}

deployment "clowk-lp" "worker" {
  image = "alpine:3"
}
`
	tmp := writeTemp(t, "ok.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 1 deployment + 1 ingress from the app, plus 1 standalone
	// deployment = 3 manifests total.
	if len(mans) != 3 {
		t.Fatalf("want 3 manifests, got %d: %+v", len(mans), mans)
	}
}

func TestParseYAMLMultiDoc(t *testing.T) {
	src := `
---
kind: deployment
scope: test
name: api
spec:
  image: nginx:1
  replicas: 2
---
kind: database
name: main
spec:
  engine: postgres
`
	tmp := writeTemp(t, "stack.yaml", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 2 {
		t.Fatalf("want 2 manifests, got %d", len(mans))
	}

	if mans[0].Kind != controller.KindDeployment || mans[1].Kind != controller.KindDatabase {
		t.Errorf("unexpected kinds: %+v %+v", mans[0], mans[1])
	}
}

func TestParseDirMixedFormats(t *testing.T) {
	dir := t.TempDir()

	writeAt(t, filepath.Join(dir, "a.hcl"), `deployment "test" "api" { image = "x:1" }`)
	writeAt(t, filepath.Join(dir, "b.yaml"), "kind: database\nname: main\nspec:\n  engine: postgres\n")
	writeAt(t, filepath.Join(dir, "README.md"), "ignored")

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}

	writeAt(t, filepath.Join(sub, "c.yml"), "kind: deployment\nscope: test\nname: worker\nspec:\n  image: worker:1\n")

	mans, err := ParseDir(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(mans), mans)
	}
}

// All voodu-branded extensions parse as HCL. Keeps the extension set
// from silently drifting out of sync between formatFromExt and the
// parseHCL synthesized path.
func TestParseFileVooduExtensions(t *testing.T) {
	src := `deployment "test" "api" { image = "nginx:1" }`

	for _, ext := range []string{".hcl", ".voodu", ".vdu", ".vd"} {
		t.Run(ext, func(t *testing.T) {
			tmp := writeTemp(t, "web"+ext, src)

			mans, err := ParseFile(tmp, nil)
			if err != nil {
				t.Fatalf("ParseFile(%s): %v", tmp, err)
			}

			if len(mans) != 1 || mans[0].Name != "api" {
				t.Errorf("unexpected: %+v", mans)
			}
		})
	}
}

func TestParseReaderStdin(t *testing.T) {
	src := `deployment "test" "api" { image = "nginx:1" }`

	mans, err := ParseReader(strings.NewReader(src), FormatHCL, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 1 || mans[0].Name != "api" {
		t.Errorf("unexpected: %+v", mans)
	}
}

func TestParseJSONRoundTrip(t *testing.T) {
	src := `
deployment "test" "api" {
  image = "nginx:1.27"
  replicas = 2
}
`

	vars := map[string]string{}

	mans, err := ParseReader(strings.NewReader(src), FormatHCL, vars)
	if err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(mans)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ParseReader(strings.NewReader(string(body)), FormatJSON, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Kind != controller.KindDeployment || got[0].Name != "api" {
		t.Fatalf("round-trip changed shape: %+v", got)
	}

	if string(got[0].Spec) != string(mans[0].Spec) {
		t.Errorf("spec mismatch:\n  got:  %s\n  want: %s", got[0].Spec, mans[0].Spec)
	}
}

func TestParseJSONSkipsInterpolation(t *testing.T) {
	// JSON payloads are already-parsed manifests; ${VAR} inside a string
	// value must be preserved verbatim (it's legitimate content, not a
	// variable reference).
	src := `[{"kind":"deployment","scope":"test","name":"api","spec":{"image":"nginx","env":{"URL":"${STILL_HERE}"}}}]`

	got, err := ParseReader(strings.NewReader(src), FormatJSON, map[string]string{"STILL_HERE": "expanded"})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d manifests", len(got))
	}

	if !strings.Contains(string(got[0].Spec), "${STILL_HERE}") {
		t.Errorf("JSON path ran interpolation, spec was: %s", got[0].Spec)
	}
}

func TestParseRejectsUnknownKind(t *testing.T) {
	src := "kind: potato\nname: x\nspec: {}\n"

	tmp := writeTemp(t, "bad.yaml", src)

	_, err := ParseFile(tmp, nil)
	if err == nil || !strings.Contains(err.Error(), "potato") {
		t.Errorf("want unknown-kind error, got %v", err)
	}
}

func TestParseUnknownExtension(t *testing.T) {
	tmp := writeTemp(t, "bad.json", "{}")

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("want error for unknown extension")
	}
}

// TestParseHCLJob locks in the M3 surface for the `job` block: same
// 2-label scope/name shape as deployment, image + command + env are the
// fields the imperative `voodu run job` consumes. Without this the
// hclJob struct could silently lose a field on refactor.
func TestParseHCLJob(t *testing.T) {
	src := `
job "api" "migrate" {
  image   = "ghcr.io/acme/api:1.0.0"
  command = ["./migrate.sh", "--up"]
  env     = { DATABASE_URL = "postgres://u:p@h:5432/db" }
  volumes = ["/opt/voodu/migrations:/migrations"]
  networks = ["voodu0"]
  timeout  = "5m"
}
`
	tmp := writeTemp(t, "job.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	if mans[0].Kind != controller.KindJob || mans[0].Scope != "api" || mans[0].Name != "migrate" {
		t.Errorf("unexpected header: %+v", mans[0])
	}

	var spec JobSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Image != "ghcr.io/acme/api:1.0.0" {
		t.Errorf("image: %q", spec.Image)
	}

	if len(spec.Command) != 2 || spec.Command[0] != "./migrate.sh" || spec.Command[1] != "--up" {
		t.Errorf("command: %+v", spec.Command)
	}

	if spec.Env["DATABASE_URL"] != "postgres://u:p@h:5432/db" {
		t.Errorf("env: %+v", spec.Env)
	}

	if len(spec.Networks) != 1 || spec.Networks[0] != "voodu0" {
		t.Errorf("networks: %+v", spec.Networks)
	}

	if spec.Timeout != "5m" {
		t.Errorf("timeout: %q", spec.Timeout)
	}
}

// TestParseHCLJobHistoryLimits guards the wire bind for the
// successful_history_limit / failed_history_limit fields the runner
// uses to retain stopped run containers (and matching JobStatus
// history entries) past AutoRemove. A drift here means `voodu logs
// job <name>` would mysteriously lose old runs even when the operator
// requested they stick around.
func TestParseHCLJobHistoryLimits(t *testing.T) {
	src := `
job "api" "migrate" {
  image                    = "img:1"
  successful_history_limit = 5
  failed_history_limit     = 2
}
`
	tmp := writeTemp(t, "job.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec JobSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.SuccessfulHistoryLimit != 5 || spec.FailedHistoryLimit != 2 {
		t.Errorf("history limits: succ=%d fail=%d (want 5, 2)",
			spec.SuccessfulHistoryLimit, spec.FailedHistoryLimit)
	}
}

// TestParseHCLJobMissingScope mirrors the deployment validator: jobs
// are scoped, so a single-label `job "name" {}` must error loudly with
// the suggested fix in the message.
func TestParseHCLJobMissingScope(t *testing.T) {
	// HCL strictly types the labels list, so a single-label decl fails
	// at parse time. The error message comes from hclsimple — we just
	// assert the parse rejects, since the label-arity is the schema
	// invariant we care about.
	src := `job "migrate" { image = "img:1" }`

	tmp := writeTemp(t, "bad.hcl", src)

	if _, err := ParseFile(tmp, nil); err == nil {
		t.Fatal("expected single-label job decl to fail HCL parse")
	}
}

// TestParseYAMLJob covers the YAML variant. spec.image / spec.command /
// spec.env all decode into JobSpec via the generic decodeYAMLSpec
// dispatch. Tests the kind switch in decodeYAMLSpec.
func TestParseYAMLJob(t *testing.T) {
	src := `
kind: job
scope: api
name: migrate
spec:
  image: ghcr.io/acme/api:1.0.0
  command:
    - ./migrate.sh
    - --up
  env:
    DATABASE_URL: postgres://u:p@h:5432/db
  timeout: 5m
`
	tmp := writeTemp(t, "job.yaml", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	if mans[0].Kind != controller.KindJob || mans[0].Scope != "api" || mans[0].Name != "migrate" {
		t.Errorf("unexpected header: %+v", mans[0])
	}

	var spec JobSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Image != "ghcr.io/acme/api:1.0.0" {
		t.Errorf("image: %q", spec.Image)
	}

	if len(spec.Command) != 2 || spec.Command[0] != "./migrate.sh" {
		t.Errorf("command: %+v", spec.Command)
	}

	if spec.Env["DATABASE_URL"] != "postgres://u:p@h:5432/db" {
		t.Errorf("env: %+v", spec.Env)
	}

	if spec.Timeout != "5m" {
		t.Errorf("timeout: %q", spec.Timeout)
	}
}

// TestParseHCLCronJob locks in the M4 surface for `cronjob`. The
// schedule + concurrency knobs sit at the block root next to the
// flattened job spec — no nested `job {}` — so authoring stays
// single-block. Without this an accidental rename of an HCL field
// (e.g. `concurrency_policy` → `concurrency`) would silently drop
// the value.
func TestParseHCLCronJob(t *testing.T) {
	src := `
cronjob "ops" "purge" {
  schedule           = "*/15 * * * *"
  timezone           = "America/Sao_Paulo"
  suspend            = false
  concurrency_policy = "Forbid"

  successful_history_limit = 5
  failed_history_limit     = 2

  image    = "ghcr.io/acme/api:1.0.0"
  command  = ["./purge.sh", "--retention=30d"]
  env      = { DATABASE_URL = "postgres://u:p@h:5432/db" }
  networks = ["voodu0"]
  timeout  = "10m"
}
`
	tmp := writeTemp(t, "cron.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	if mans[0].Kind != controller.KindCronJob || mans[0].Scope != "ops" || mans[0].Name != "purge" {
		t.Errorf("unexpected header: %+v", mans[0])
	}

	var spec CronJobSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Schedule != "*/15 * * * *" {
		t.Errorf("schedule: %q", spec.Schedule)
	}

	if spec.Timezone != "America/Sao_Paulo" {
		t.Errorf("timezone: %q", spec.Timezone)
	}

	if spec.ConcurrencyPolicy != "Forbid" {
		t.Errorf("concurrency_policy: %q", spec.ConcurrencyPolicy)
	}

	if spec.SuccessfulHistoryLimit != 5 || spec.FailedHistoryLimit != 2 {
		t.Errorf("history limits: succ=%d fail=%d", spec.SuccessfulHistoryLimit, spec.FailedHistoryLimit)
	}

	if spec.Job.Image != "ghcr.io/acme/api:1.0.0" {
		t.Errorf("job.image: %q", spec.Job.Image)
	}

	if len(spec.Job.Command) != 2 || spec.Job.Command[0] != "./purge.sh" {
		t.Errorf("job.command: %+v", spec.Job.Command)
	}

	if spec.Job.Env["DATABASE_URL"] != "postgres://u:p@h:5432/db" {
		t.Errorf("job.env: %+v", spec.Job.Env)
	}

	if len(spec.Job.Networks) != 1 || spec.Job.Networks[0] != "voodu0" {
		t.Errorf("job.networks: %+v", spec.Job.Networks)
	}

	if spec.Job.Timeout != "10m" {
		t.Errorf("job.timeout: %q", spec.Job.Timeout)
	}
}

// TestParseHCLCronJobMissingScope mirrors the job test: cronjobs are
// scoped, so a single-label decl must fail HCL parse. The label arity
// is the schema invariant we care about.
func TestParseHCLCronJobMissingScope(t *testing.T) {
	src := `cronjob "purge" { schedule = "* * * * *" image = "img:1" }`

	tmp := writeTemp(t, "bad.hcl", src)

	if _, err := ParseFile(tmp, nil); err == nil {
		t.Fatal("expected single-label cronjob decl to fail HCL parse")
	}
}

// TestParseHCLCronJobScheduleRequired ensures a cronjob without a
// schedule is rejected at parse time, not silently turned into a
// "fires never" manifest.
func TestParseHCLCronJobScheduleRequired(t *testing.T) {
	src := `cronjob "ops" "purge" { image = "img:1" }`

	tmp := writeTemp(t, "noschedule.hcl", src)

	if _, err := ParseFile(tmp, nil); err == nil {
		t.Fatal("expected error when schedule is missing")
	}
}

// TestParseYAMLCronJob covers the YAML variant. Note YAML's CronJobSpec
// shape: the job fields nest under `spec.job` because YAML doesn't
// give us the flattening sugar the HCL block does, and the wire shape
// the controller decodes already mirrors that.
func TestParseYAMLCronJob(t *testing.T) {
	src := `
kind: cronjob
scope: ops
name: purge
spec:
  schedule: "*/15 * * * *"
  timezone: America/Sao_Paulo
  concurrency_policy: Forbid
  successful_history_limit: 5
  failed_history_limit: 2
  job:
    image: ghcr.io/acme/api:1.0.0
    command:
      - ./purge.sh
      - --retention=30d
    env:
      DATABASE_URL: postgres://u:p@h:5432/db
    timeout: 10m
`
	tmp := writeTemp(t, "cron.yaml", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	if mans[0].Kind != controller.KindCronJob || mans[0].Scope != "ops" || mans[0].Name != "purge" {
		t.Errorf("unexpected header: %+v", mans[0])
	}

	var spec CronJobSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Schedule != "*/15 * * * *" {
		t.Errorf("schedule: %q", spec.Schedule)
	}

	if spec.ConcurrencyPolicy != "Forbid" {
		t.Errorf("concurrency_policy: %q", spec.ConcurrencyPolicy)
	}

	if spec.Job.Image != "ghcr.io/acme/api:1.0.0" {
		t.Errorf("job.image: %q", spec.Job.Image)
	}

	if len(spec.Job.Command) != 2 {
		t.Errorf("job.command: %+v", spec.Job.Command)
	}

	if spec.SuccessfulHistoryLimit != 5 || spec.FailedHistoryLimit != 2 {
		t.Errorf("history limits: succ=%d fail=%d", spec.SuccessfulHistoryLimit, spec.FailedHistoryLimit)
	}
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), name)
	writeAt(t, p, content)

	return p
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
