package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	// Build-mode deployment: container/runtime concerns at root (ports,
	// env, post_deploy, ...), build-time inputs nested inside the
	// `build {}` block (context, dockerfile, path, args, lang).
	// docker-compose-shaped — context matches compose's `build.context`,
	// args matches `build.args`.
	src := `
deployment "test" "api" {
  ports        = ["127.0.0.1:9092:9092"]
  volumes      = ["/opt/voodu/volumes/rtp:/app/recordings"]
  network      = "bridge"
  restart      = "unless-stopped"
  env          = { RAILS_ENV = "production" }
  post_deploy  = ["./bin/migrate"]
  health_check = "/healthz"

  build {
    context    = "apps/esl"
    dockerfile = "Dockerfile.api"
    path       = "cmd/api"
    args = {
      GOOS        = "linux"
      GOARCH      = "amd64"
      CGO_ENABLED = "0"
      GIT_SHA     = "abc123"
    }

    lang {
      name    = "go"
      version = "1.25"
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

	if spec.Build == nil {
		t.Fatal("build block lost")
	}

	if spec.Build.Context != "apps/esl" || spec.Build.Dockerfile != "Dockerfile.api" || spec.Build.Path != "cmd/api" {
		t.Errorf("build source fields not carried: %+v", spec.Build)
	}

	if spec.Build.Args["GOOS"] != "linux" || spec.Build.Args["GIT_SHA"] != "abc123" {
		t.Errorf("build.args lost: %+v", spec.Build.Args)
	}

	if spec.Build.Lang == nil {
		t.Fatal("lang block lost (should be inside build {})")
	}

	if spec.Build.Lang.Name != "go" || spec.Build.Lang.Version != "1.25" {
		t.Errorf("lang fields lost: %+v", spec.Build.Lang)
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
	// to the generic Dockerfile path. Build args (MIX_ENV etc.) live on
	// the parent build block (`build.args = {...}`).
	src := `
deployment "test" "api" {
  build {
    args = {
      MIX_ENV = "prod"
    }

    lang {
      name    = "elixir"
      version = "1.17"
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

	if spec.Build == nil || spec.Build.Lang == nil {
		t.Fatalf("build/lang block lost: %+v", spec.Build)
	}

	if spec.Build.Lang.Name != "elixir" || spec.Build.Lang.Version != "1.17" {
		t.Errorf("exotic lang not carried: %+v", spec.Build.Lang)
	}

	if spec.Build.Args["MIX_ENV"] != "prod" {
		t.Errorf("build.args lost: %+v", spec.Build.Args)
	}
}

func TestParseHCLDeploymentImageOptional(t *testing.T) {
	// Minimal build-mode deployment: no image, no build {} either.
	// applyDefaults synthesises `build = { context = "." }` so
	// downstream consumers always see a concrete Build pointer.
	// Dockerfile stays empty so lang handlers can auto-resolve.
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

	if spec.Build == nil {
		t.Fatal("applyDefaults must synthesize Build when neither image nor build {} declared")
	}

	if spec.Build.Context != "." {
		t.Errorf("context default not applied: got %q, want %q", spec.Build.Context, ".")
	}

	if spec.Build.Dockerfile != "" {
		t.Errorf("dockerfile should stay empty so handlers can auto-resolve: got %q", spec.Build.Dockerfile)
	}
}

func TestParseHCLStatefulsetBuildMode(t *testing.T) {
	// Statefulset with build {} — exercises the build-mode path on the
	// stateful kind. Use case: postgres + pgvector built inline (FROM
	// postgres:16, apt-get install postgresql-16-pgvector) without a
	// separate CI to publish a custom registry image.
	src := `statefulset "data" "pg" {
  replicas = 1

  build {
    context    = "infra/postgres"
    dockerfile = "Dockerfile.pg"

    lang {
      name = "generic"
    }
  }
}`

	tmp := writeTemp(t, "stateful_build.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatalf("statefulset build-mode parse: %v", err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	var spec StatefulsetSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Image != "" {
		t.Errorf("image should stay empty (build mode): %q", spec.Image)
	}

	if spec.Build == nil {
		t.Fatal("build block lost")
	}

	if spec.Build.Context != "infra/postgres" {
		t.Errorf("context lost: %q", spec.Build.Context)
	}

	if spec.Build.Dockerfile != "Dockerfile.pg" {
		t.Errorf("dockerfile lost: %q", spec.Build.Dockerfile)
	}

	if spec.Build.Lang == nil || spec.Build.Lang.Name != "generic" {
		t.Errorf("nested lang block lost: %+v", spec.Build.Lang)
	}
}

func TestParseHCLStatefulsetRegistryModeKeepsBuildNil(t *testing.T) {
	// Image-mode: applyDefaults should NOT synthesize a Build block.
	// Mirrors the deployment behaviour — Build is meaningless when no
	// build runs.
	src := `statefulset "data" "redis" {
  image    = "redis:8"
  replicas = 1
}`

	tmp := writeTemp(t, "stateful_registry.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec StatefulsetSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Build != nil {
		t.Errorf("registry-mode statefulset must not synthesize Build, got %+v", spec.Build)
	}
}

func TestParseHCLDeploymentResourcesBlock(t *testing.T) {
	// resources { limits { cpu = "2" memory = "4Gi" } } — parsed
	// into the wire spec verbatim (k8svalues normalises later, at
	// the controller's docker-translation step). Tests pin that the
	// HCL surface round-trips through JSON.
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  resources {
    limits {
      cpu    = "2"
      memory = "4Gi"
    }
  }
}
`
	tmp := writeTemp(t, "dep_resources.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Resources == nil || spec.Resources.Limits == nil {
		t.Fatal("resources.limits missing in parsed spec")
	}

	if spec.Resources.Limits.CPU != "2" {
		t.Errorf("cpu: got %q, want 2", spec.Resources.Limits.CPU)
	}

	if spec.Resources.Limits.Memory != "4Gi" {
		t.Errorf("memory: got %q, want 4Gi", spec.Resources.Limits.Memory)
	}
}

func TestParseHCLStatefulsetResourcesBlock(t *testing.T) {
	// Same shape as deployment — voodu-postgres / voodu-redis
	// operators in production lean on this for the steady-state
	// services that need bounded memory.
	src := `
statefulset "data" "db" {
  image    = "postgres:16"
  replicas = 3

  resources {
    limits {
      cpu    = "500m"
      memory = "2Gi"
    }
  }
}
`
	tmp := writeTemp(t, "stateful_resources.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec StatefulsetSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Resources == nil || spec.Resources.Limits == nil {
		t.Fatal("resources.limits missing in parsed spec")
	}

	if spec.Resources.Limits.CPU != "500m" {
		t.Errorf("cpu: got %q", spec.Resources.Limits.CPU)
	}

	if spec.Resources.Limits.Memory != "2Gi" {
		t.Errorf("memory: got %q", spec.Resources.Limits.Memory)
	}
}

// TestParseHCLStatefulsetProbes pins the M1.3 surface: a
// statefulset can declare the same kubelet-style probes block
// as a deployment. Useful for postgres (pg_isready), redis
// (redis-cli ping), or HTTP-fronted databases.
func TestParseHCLStatefulsetProbes(t *testing.T) {
	src := `
statefulset "data" "redis" {
  image    = "redis:7"
  replicas = 3

  probes {
    liveness {
      tcp_socket { port = 6379 }
      period            = "5s"
      failure_threshold = 3
    }

    readiness {
      exec {
        command = ["redis-cli", "ping"]
      }

      period = "5s"
    }
  }
}
`
	tmp := writeTemp(t, "stateful_probes.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec StatefulsetSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Probes == nil {
		t.Fatal("statefulset probes block lost")
	}

	if spec.Probes.Liveness == nil || spec.Probes.Liveness.TCPSocket == nil {
		t.Fatal("liveness tcp_socket missing")
	}

	if spec.Probes.Liveness.TCPSocket.Port != 6379 {
		t.Errorf("liveness port: %d", spec.Probes.Liveness.TCPSocket.Port)
	}

	if spec.Probes.Readiness == nil || spec.Probes.Readiness.Exec == nil {
		t.Fatal("readiness exec missing")
	}

	if len(spec.Probes.Readiness.Exec.Command) != 2 || spec.Probes.Readiness.Exec.Command[0] != "redis-cli" {
		t.Errorf("readiness command: %v", spec.Probes.Readiness.Exec.Command)
	}
}

// TestParseHCLStatefulsetProbes_NoSelectorFails pins that the
// same mutual-exclusion validator that runs on deployments
// catches malformed statefulset probes too.
func TestParseHCLStatefulsetProbes_NoSelectorFails(t *testing.T) {
	src := `
statefulset "data" "redis" {
  image = "redis:7"

  probes {
    liveness {
      period = "5s"
    }
  }
}
`
	tmp := writeTemp(t, "stateful_probes_empty.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error when no probe action selector declared")
	}

	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("expected 'exactly one' in error: %v", err)
	}
}

func TestParseHCLResourcesBlockOptional(t *testing.T) {
	// Operator omitting `resources { }` entirely → spec.Resources
	// is nil. No-limit container, docker daemon defaults apply.
	src := `deployment "test" "api" { image = "nginx:1.27" }`

	tmp := writeTemp(t, "no_resources.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Resources != nil {
		t.Errorf("expected nil resources when omitted, got %+v", spec.Resources)
	}
}

func TestParseHCLResourcesBlockEmptyLimitsOK(t *testing.T) {
	// `resources { limits { } }` with no inner fields — valid;
	// surfaces as &ResourcesSpec{Limits: &ResourceLimits{}} and
	// dockerResources translates that to ("", 0, nil) = no limit.
	src := `
deployment "test" "api" {
  image = "nginx:1.27"

  resources {
    limits {}
  }
}
`
	tmp := writeTemp(t, "empty_limits.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Resources == nil || spec.Resources.Limits == nil {
		t.Fatal("resources.limits should be present (empty body)")
	}

	if spec.Resources.Limits.CPU != "" || spec.Resources.Limits.Memory != "" {
		t.Errorf("expected empty cpu/memory, got %+v", spec.Resources.Limits)
	}
}

func TestParseHCLJobResourcesBlock(t *testing.T) {
	src := `
job "scope" "migrate" {
  image   = "ghcr.io/clowk/web:latest"
  command = ["rails", "db:migrate"]

  resources {
    limits {
      cpu    = "1"
      memory = "1Gi"
    }
  }
}
`
	tmp := writeTemp(t, "job_resources.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec JobSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Resources == nil || spec.Resources.Limits == nil {
		t.Fatal("resources.limits missing on job")
	}

	if spec.Resources.Limits.CPU != "1" || spec.Resources.Limits.Memory != "1Gi" {
		t.Errorf("limits: %+v", spec.Resources.Limits)
	}
}

// TestParseHCLDeploymentAutoscale pins the wire shape of the M7
// `autoscale { ... }` block: every field round-trips into the typed
// AutoscaleSpec, the spec.Autoscale pointer is non-nil, and the
// rest of the deployment surface (image, image-mode-mutex with
// build, etc.) keeps working.
func TestParseHCLDeploymentAutoscale(t *testing.T) {
	src := `
deployment "prod" "worker" {
  image = "ghcr.io/acme/worker:1.0.0"

  autoscale {
    min           = 2
    max           = 10
    cpu_target    = 70
    cooldown_up   = "30s"
    cooldown_down = "5m"
  }
}
`
	tmp := writeTemp(t, "dep_autoscale.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Autoscale == nil {
		t.Fatal("autoscale missing in parsed spec")
	}

	if spec.Autoscale.Min != 2 {
		t.Errorf("min: %d", spec.Autoscale.Min)
	}

	if spec.Autoscale.Max != 10 {
		t.Errorf("max: %d", spec.Autoscale.Max)
	}

	if spec.Autoscale.CPUTarget != 70 {
		t.Errorf("cpu_target: %d", spec.Autoscale.CPUTarget)
	}

	if spec.Autoscale.CooldownUp != "30s" {
		t.Errorf("cooldown_up: %q", spec.Autoscale.CooldownUp)
	}

	if spec.Autoscale.CooldownDown != "5m" {
		t.Errorf("cooldown_down: %q", spec.Autoscale.CooldownDown)
	}
}

// TestParseHCLDeploymentAutoscale_RejectsReplicasMix pins the mutex
// rule. An operator pinning a static replica count AND declaring an
// autoscale block is ambiguous — one of the two takes effect, the
// other becomes a footgun. The parser must reject up-front.
func TestParseHCLDeploymentAutoscale_RejectsReplicasMix(t *testing.T) {
	src := `
deployment "prod" "worker" {
  image    = "ghcr.io/acme/worker:1.0.0"
  replicas = 3

  autoscale {
    min        = 2
    max        = 10
    cpu_target = 70
  }
}
`
	tmp := writeTemp(t, "dep_autoscale_mix.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected parse error for replicas + autoscale combo")
	}

	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error must mention mutual exclusivity, got: %v", err)
	}
}

// TestApplyDefaults_AutoscaleSetsInitialReplicasToMin pins the
// bootstrap semantics: a deployment with only an autoscale block
// (no explicit replicas) should seed Replicas with autoscale.Min so
// the first reconcile boots at the floor, not the legacy default
// of 1. Without this, an autoscaled deployment with min = 5 would
// start with 1 pod and the autoscaler would have to ramp up over
// several ticks.
func TestApplyDefaults_AutoscaleSetsInitialReplicasToMin(t *testing.T) {
	src := `
deployment "prod" "worker" {
  image = "ghcr.io/acme/worker:1.0.0"

  autoscale {
    min        = 5
    max        = 20
    cpu_target = 70
  }
}
`
	tmp := writeTemp(t, "dep_autoscale_initial.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Replicas != 5 {
		t.Errorf("expected Replicas to be seeded to Min (5), got %d", spec.Replicas)
	}
}

// TestParseHCLDeploymentAutoscale_RejectsBadBounds pins the
// numeric guard rails — operators should get a parse error for
// nonsense (min < 1, max < min, target outside (0, 100]) rather
// than a silently-broken scaler at runtime.
func TestParseHCLDeploymentAutoscale_RejectsBadBounds(t *testing.T) {
	tpl := func(min, max, target int) string {
		return "deployment \"p\" \"w\" {\n" +
			"  image = \"x\"\n" +
			"  autoscale {\n" +
			"    min        = " + itoa(min) + "\n" +
			"    max        = " + itoa(max) + "\n" +
			"    cpu_target = " + itoa(target) + "\n" +
			"  }\n" +
			"}\n"
	}

	cases := []struct {
		name string
		src  string
		want string
	}{
		{"min zero", tpl(0, 5, 70), "autoscale.min must be >= 1"},
		{"max less than min", tpl(5, 2, 70), "autoscale.max"},
		{"cpu_target zero", tpl(1, 5, 0), "autoscale.cpu_target"},
		{"cpu_target over 100", tpl(1, 5, 150), "autoscale.cpu_target"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := writeTemp(t, "bad.hcl", c.src)

			_, err := ParseFile(tmp, nil)
			if err == nil {
				t.Fatal("expected parse error")
			}

			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error should mention %q, got: %v", c.want, err)
			}
		})
	}
}

// itoa is a tiny local int-to-string helper so the HCL templates
// stay readable. strconv.Itoa would work but would mean an extra
// import for a 6-line function used only in one test.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	neg := n < 0
	if neg {
		n = -n
	}

	var buf [20]byte

	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	if neg {
		i--
		buf[i] = '-'
	}

	return string(buf[i:])
}

func TestParseHCLMultiKind(t *testing.T) {
	src := `
deployment "test" "api" {
  image = "a:1"
}
statefulset "data" "pg" {
  image = "postgres:15"
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

  build {
    lang {
      name    = "nodejs"
      version = "22"
    }
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

	if depSpec.Build == nil || depSpec.Build.Lang == nil {
		t.Fatalf("build/lang block lost: %+v", depSpec.Build)
	}

	if depSpec.Build.Lang.Name != "nodejs" || depSpec.Build.Lang.Version != "22" {
		t.Errorf("lang block fields lost: %+v", depSpec.Build.Lang)
	}

	// applyDefaults must synthesise build mode on the app-emitted
	// deployment too — same as a standalone `deployment` block.
	// Otherwise an app-authored deployment would diverge from a
	// hand-written one.
	if depSpec.Build.Context != "." {
		t.Errorf("default context not applied to app deployment: %q", depSpec.Build.Context)
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

// TestParseHCLIngressTLSDefaults locks in the "block-present = TLS
// on" semantics: declaring `tls {}` (even bare) flips the wire spec
// to `enabled = true, provider = "letsencrypt"`. The whole point of
// these defaults is so the 90% case writes nothing redundant:
//
//	tls {
//	  email = "ops@example.com"
//	}
//
// matches what every web app wants — TLS enabled, letsencrypt as
// issuer.
func TestParseHCLIngressTLSDefaults(t *testing.T) {
	cases := []struct {
		name        string
		tlsBlock    string
		wantEnabled bool
		wantProv    string
	}{
		{
			name: "empty block defaults to enabled + letsencrypt",
			tlsBlock: `
tls {
}`,
			wantEnabled: true,
			wantProv:    "letsencrypt",
		},
		{
			name: "email-only fills enabled + letsencrypt",
			tlsBlock: `
tls {
  email = "ops@example.com"
}`,
			wantEnabled: true,
			wantProv:    "letsencrypt",
		},
		{
			name: "explicit provider keeps custom value",
			tlsBlock: `
tls {
  provider = "internal"
}`,
			wantEnabled: true,
			wantProv:    "internal",
		},
		{
			name: "explicit enabled=true is no-op (matches default)",
			tlsBlock: `
tls {
  enabled = true
}`,
			wantEnabled: true,
			wantProv:    "letsencrypt",
		},
		{
			name: "explicit enabled=false honoured (escape hatch)",
			tlsBlock: `
tls {
  enabled = false
  email   = "x@y.z"
}`,
			wantEnabled: false,
			wantProv:    "letsencrypt",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `
ingress "scope" "web" {
  host = "x.example.com"
` + tc.tlsBlock + `
}
`
			tmp := writeTemp(t, "tls.hcl", src)

			mans, err := ParseFile(tmp, nil)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			var spec IngressSpec
			if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
				t.Fatal(err)
			}

			if spec.TLS == nil {
				t.Fatal("declared tls block but wire spec has nil TLS")
			}

			if spec.TLS.Enabled != tc.wantEnabled {
				t.Errorf("Enabled: got %v, want %v", spec.TLS.Enabled, tc.wantEnabled)
			}

			if spec.TLS.Provider != tc.wantProv {
				t.Errorf("Provider: got %q, want %q", spec.TLS.Provider, tc.wantProv)
			}
		})
	}
}

// TestParseHCLIngressNoTLSBlockMeansNoTLS pins the opposite case:
// when the operator omits the `tls {}` block entirely, the wire spec
// has nil TLS — no http→https redirect, no cert issuance. Defaults
// only fire when the block is present.
func TestParseHCLIngressNoTLSBlockMeansNoTLS(t *testing.T) {
	src := `ingress "scope" "web" { host = "x.example.com" }`
	tmp := writeTemp(t, "no-tls.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec IngressSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.TLS != nil {
		t.Errorf("omitted tls block should yield nil TLS, got %+v", spec.TLS)
	}
}

// TestParseHCLAppInheritsTLSDefaults — same defaults apply to the
// `app` sugar block. Parity with standalone `ingress` is the whole
// point of the sugar.
func TestParseHCLAppInheritsTLSDefaults(t *testing.T) {
	src := `
app "scope" "web" {
  image = "nginx:1.27"
  ports = ["80"]
  host  = "x.example.com"

  tls {
    email = "ops@example.com"
  }
}
`
	tmp := writeTemp(t, "app-tls.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	// app expands to (deployment, ingress); the ingress half carries
	// TLS.
	var ing IngressSpec
	if err := json.Unmarshal(mans[1].Spec, &ing); err != nil {
		t.Fatal(err)
	}

	if ing.TLS == nil || !ing.TLS.Enabled || ing.TLS.Provider != "letsencrypt" {
		t.Errorf("app sugar should inherit TLS defaults: %+v", ing.TLS)
	}
}

// TestParseHCLAppCarriesReleaseBlock locks in that the authoring
// sugar `app` exposes the release-phase block on parity with
// `deployment`. Without this, web apps written in the `app` shape
// silently lose their migration step on rollout — the parser would
// either reject the block (HCL: unsupported) or, worse, accept and
// drop it. Both outcomes broke once and the test exists so the
// next refactor catches the regression.
func TestParseHCLAppCarriesReleaseBlock(t *testing.T) {
	src := `
app "clowk-lp" "web" {
  image = "clowk-lp:latest"
  ports = ["3000"]
  host  = "clowk.lp"

  release {
    command      = ["rails", "db:migrate"]
    pre_command  = ["bin/preflight"]
    post_command = ["bin/notify"]
    timeout      = "5m"
  }
}
`
	tmp := writeTemp(t, "app_release.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 2 {
		t.Fatalf("want 2 manifests (deployment+ingress), got %d", len(mans))
	}

	dep := mans[0]
	if dep.Kind != controller.KindDeployment {
		t.Fatalf("first manifest should be deployment, got %s", dep.Kind)
	}

	var depSpec DeploymentSpec
	if err := json.Unmarshal(dep.Spec, &depSpec); err != nil {
		t.Fatal(err)
	}

	if depSpec.Release == nil {
		t.Fatal("release block lost — app should propagate it to deployment")
	}

	if got, want := depSpec.Release.Command, []string{"rails", "db:migrate"}; !slices.Equal(got, want) {
		t.Errorf("release.command: got %v, want %v", got, want)
	}

	if got, want := depSpec.Release.PreCommand, []string{"bin/preflight"}; !slices.Equal(got, want) {
		t.Errorf("release.pre_command: got %v, want %v", got, want)
	}

	if got, want := depSpec.Release.PostCommand, []string{"bin/notify"}; !slices.Equal(got, want) {
		t.Errorf("release.post_command: got %v, want %v", got, want)
	}

	if depSpec.Release.Timeout != "5m" {
		t.Errorf("release.timeout: got %q, want %q", depSpec.Release.Timeout, "5m")
	}
}

// TestParseHCLAppCarriesResourcesBlock pins that `app` exposes the
// resources block on parity with standalone `deployment`. Without
// this, every operator using the authoring-sugar shape silently
// loses CPU/memory caps — the docker container boots with no
// limits even when the HCL declared them. The kernel-cap field is
// the same wire shape on both kinds, so dropping it on the app
// path was a parser-only oversight.
func TestParseHCLAppCarriesResourcesBlock(t *testing.T) {
	src := `
app "clowk-lp" "web" {
  image = "clowk-lp:latest"
  ports = ["3000"]
  host  = "clowk.lp"

  resources {
    limits {
      cpu    = "500m"
      memory = "256Mi"
    }
  }
}
`
	tmp := writeTemp(t, "app_resources.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 2 {
		t.Fatalf("want 2 manifests (deployment+ingress), got %d", len(mans))
	}

	dep := mans[0]
	if dep.Kind != controller.KindDeployment {
		t.Fatalf("first manifest should be deployment, got %s", dep.Kind)
	}

	var depSpec DeploymentSpec
	if err := json.Unmarshal(dep.Spec, &depSpec); err != nil {
		t.Fatal(err)
	}

	if depSpec.Resources == nil || depSpec.Resources.Limits == nil {
		t.Fatal("resources.limits missing — app should propagate the block to deployment")
	}

	if got, want := depSpec.Resources.Limits.CPU, "500m"; got != want {
		t.Errorf("resources.limits.cpu: got %q, want %q", got, want)
	}

	if got, want := depSpec.Resources.Limits.Memory, "256Mi"; got != want {
		t.Errorf("resources.limits.memory: got %q, want %q", got, want)
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

// TestParseDirWalksRecursively verifies ParseDir walks subdirs
// and parses every voodu-branded HCL file it finds while skipping
// unrelated content (README.md, etc.). Replaces an older mixed-
// format variant that exercised the YAML parser too.
func TestParseDirWalksRecursively(t *testing.T) {
	dir := t.TempDir()

	writeAt(t, filepath.Join(dir, "a.hcl"), `deployment "test" "api" { image = "x:1" }`)
	writeAt(t, filepath.Join(dir, "b.voodu"), `statefulset "data" "pg" { image = "postgres:15" }`)
	writeAt(t, filepath.Join(dir, "README.md"), "ignored")

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}

	writeAt(t, filepath.Join(sub, "c.vd"), `deployment "test" "worker" { image = "worker:1" }`)

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

// TestParseHCLPluginBlockEmitsDynamicKind locks down the M-D0d
// contract: any block type the parser doesn't have a typed
// decoder for becomes a Manifest with Kind = block type and Spec
// = JSON of the body's attributes. The server side decides
// whether a plugin is registered for that kind — the parser
// stays agnostic.
//
// Three label shapes covered: 0 (singleton), 1 (name-only), 2
// (scope+name). Each maps to the (Scope, Name) pair the
// downstream handler expects; without explicit testing the rule
// could silently drift to "always 2 labels" or similar.
func TestParseHCLPluginBlockEmitsDynamicKind(t *testing.T) {
	src := `
postgres "data" "main" {
  image    = "postgres:15-alpine"
  replicas = 2

  env = {
    POSTGRES_DB = "myapp"
  }
}

redis "cache" {
  image   = "redis:7-alpine"
  cluster = false
}

mongo {
  image = "mongo:7"
}
`
	tmp := writeTemp(t, "plugins.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(mans) != 3 {
		t.Fatalf("expected 3 manifests, got %d", len(mans))
	}

	byKind := make(map[string]controller.Manifest, len(mans))
	for _, m := range mans {
		byKind[string(m.Kind)] = m
	}

	// 2 labels → scope + name.
	pg := byKind["postgres"]
	if pg.Scope != "data" || pg.Name != "main" {
		t.Errorf("postgres scope/name: %q/%q", pg.Scope, pg.Name)
	}

	var pgSpec map[string]any
	if err := json.Unmarshal(pg.Spec, &pgSpec); err != nil {
		t.Fatalf("decode postgres spec: %v", err)
	}

	if pgSpec["image"] != "postgres:15-alpine" {
		t.Errorf("postgres image: %v", pgSpec["image"])
	}

	if pgSpec["replicas"].(float64) != 2 {
		t.Errorf("postgres replicas: %v", pgSpec["replicas"])
	}

	envMap, _ := pgSpec["env"].(map[string]any)
	if envMap["POSTGRES_DB"] != "myapp" {
		t.Errorf("postgres env: %v", envMap)
	}

	// 1 label → name only, scope empty.
	rd := byKind["redis"]
	if rd.Scope != "" || rd.Name != "cache" {
		t.Errorf("redis scope/name: %q/%q", rd.Scope, rd.Name)
	}

	var rdSpec map[string]any
	_ = json.Unmarshal(rd.Spec, &rdSpec)

	if rdSpec["cluster"] != false {
		t.Errorf("redis cluster: %v", rdSpec["cluster"])
	}

	// 0 labels → name = block type (singleton).
	mg := byKind["mongo"]
	if mg.Scope != "" || mg.Name != "mongo" {
		t.Errorf("mongo (singleton) scope/name: %q/%q", mg.Scope, mg.Name)
	}
}

// TestParseHCLPluginBlockNestedBlocks covers nested-block
// rollup: `postgres "main" { backup { schedule = "..." } }`
// surfaces backup as an object inside the spec; multiple
// occurrences would surface as an array. Plugins that need
// labels on nested blocks (e.g. `volume_claim "data" {…}`)
// recover them via the synthetic "_labels" key.
func TestParseHCLPluginBlockNestedBlocks(t *testing.T) {
	src := `
postgres "main" {
  image = "postgres:15-alpine"

  backup {
    schedule  = "@daily"
    retention = "7d"
  }

  volume_claim "data" {
    mount_path = "/var/lib/postgresql/data"
  }

  volume_claim "wal" {
    mount_path = "/var/lib/postgresql/wal"
  }
}
`
	tmp := writeTemp(t, "nested.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(mans) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(mans))
	}

	var spec map[string]any
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	// Single occurrence — flat object.
	backup, ok := spec["backup"].(map[string]any)
	if !ok {
		t.Fatalf("backup not an object: %T %v", spec["backup"], spec["backup"])
	}

	if backup["schedule"] != "@daily" {
		t.Errorf("backup.schedule: %v", backup["schedule"])
	}

	// Multiple occurrence — array of objects.
	claims, ok := spec["volume_claim"].([]any)
	if !ok {
		t.Fatalf("volume_claim not a list: %T %v", spec["volume_claim"], spec["volume_claim"])
	}

	if len(claims) != 2 {
		t.Fatalf("expected 2 volume_claims, got %d", len(claims))
	}

	first, _ := claims[0].(map[string]any)

	labels, _ := first["_labels"].([]any)
	if len(labels) != 1 {
		t.Errorf("first claim labels: %v", labels)
	}
}

// TestParseHCLPluginBlockEnvCoercion pins the env-stringify
// contract: HCL number / bool literals inside a plugin block's
// `env` attribute serialise as JSON strings, not JSON numbers/
// bools. Without this, the consumer kind's `Env map[string]string`
// decode silently drops the env (or whole spec) on type
// mismatch, and the operator never knows the var didn't reach
// the container.
func TestParseHCLPluginBlockEnvCoercion(t *testing.T) {
	src := `
redis "data" "cache" {
  image = "redis:8"

  env = {
    SKIP_FIX_PERMS = 1
    DEBUG          = true
    MAX_CONNS      = 100
    LOG_LEVEL      = "info"
    SAMPLE_RATE    = 0.25
  }
}
`
	tmp := writeTemp(t, "env-coerce.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(mans) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(mans))
	}

	var spec map[string]any
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	env, ok := spec["env"].(map[string]any)
	if !ok {
		t.Fatalf("env not an object: %T", spec["env"])
	}

	cases := map[string]string{
		"SKIP_FIX_PERMS": "1",
		"DEBUG":          "true",
		"MAX_CONNS":      "100",
		"LOG_LEVEL":      "info",
		"SAMPLE_RATE":    "0.25",
	}

	for k, want := range cases {
		got, ok := env[k].(string)
		if !ok {
			t.Errorf("env[%q] = %v (%T), want string %q", k, env[k], env[k], want)
			continue
		}

		if got != want {
			t.Errorf("env[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestStringifyEnvMap is the unit-level pin for the helper —
// covers the type variants that ctyValueToGo emits for an HCL
// object literal (string, bool, float64-from-int, float64
// genuine, null, empty string).
func TestStringifyEnvMap(t *testing.T) {
	in := map[string]any{
		"S":     "hello",
		"EMPTY": "",
		"B":     true,
		"BF":    false,
		"I":     float64(42),
		"F":     3.14,
		"Z":     float64(0),
		"NULL":  nil,
	}

	out, ok := stringifyEnvMap(in).(map[string]any)
	if !ok {
		t.Fatalf("not a map: %T", out)
	}

	cases := map[string]string{
		"S":     "hello",
		"EMPTY": "",
		"B":     "true",
		"BF":    "false",
		"I":     "42",
		"F":     "3.14",
		"Z":     "0",
		"NULL":  "",
	}

	for k, want := range cases {
		got, ok := out[k].(string)
		if !ok {
			t.Errorf("[%s] = %v (%T), want string %q", k, out[k], out[k], want)
			continue
		}

		if got != want {
			t.Errorf("[%s] = %q, want %q", k, got, want)
		}
	}
}

// TestStringifyEnvMap_PreservesNonMap covers the defensive
// passthrough: if env isn't a map (somehow — author error), the
// helper hands back the original value rather than panicking.
// The downstream JSON decoder will surface the type mismatch
// loudly.
func TestStringifyEnvMap_PreservesNonMap(t *testing.T) {
	got := stringifyEnvMap("not a map")
	if got != "not a map" {
		t.Errorf("got %v, want passthrough", got)
	}

	got = stringifyEnvMap(nil)
	if got != nil {
		t.Errorf("got %v, want nil passthrough", got)
	}
}

// TestParseHCLDeploymentComposeShapedFields pins the docker-compose-
// shaped pass-through knobs (extra_hosts, cap_add, env_file) at the
// parse layer + the nested `build { args = {...} }` block. We lock in
// that the HCL surface round-trips them verbatim into the wire spec.
func TestParseHCLDeploymentComposeShapedFields(t *testing.T) {
	src := `
deployment "test" "fsw" {
  extra_hosts = [
    "host.docker.internal:host-gateway",
    "legacy-api:10.0.0.5",
  ]

  cap_add = ["SYS_NICE", "NET_ADMIN"]

  build {
    args = {
      SERVICE = "api"
      VERSION = "1.2.3"
    }
  }

  env_file = ["./app.env", "./secrets.env"]
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

	var spec DeploymentSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	wantHosts := []string{"host.docker.internal:host-gateway", "legacy-api:10.0.0.5"}
	if len(spec.ExtraHosts) != len(wantHosts) {
		t.Fatalf("extra_hosts: got %+v, want %+v", spec.ExtraHosts, wantHosts)
	}

	for i, want := range wantHosts {
		if spec.ExtraHosts[i] != want {
			t.Errorf("extra_hosts[%d] = %q, want %q", i, spec.ExtraHosts[i], want)
		}
	}

	wantCaps := []string{"SYS_NICE", "NET_ADMIN"}
	if len(spec.CapAdd) != len(wantCaps) {
		t.Fatalf("cap_add: got %+v, want %+v", spec.CapAdd, wantCaps)
	}

	for i, want := range wantCaps {
		if spec.CapAdd[i] != want {
			t.Errorf("cap_add[%d] = %q, want %q", i, spec.CapAdd[i], want)
		}
	}

	if spec.Build == nil {
		t.Fatal("build block lost")
	}

	if spec.Build.Args["SERVICE"] != "api" || spec.Build.Args["VERSION"] != "1.2.3" {
		t.Errorf("build.args lost: %+v", spec.Build.Args)
	}

	wantEnvFiles := []string{"./app.env", "./secrets.env"}
	if len(spec.EnvFile) != len(wantEnvFiles) {
		t.Fatalf("env_file: got %+v, want %+v", spec.EnvFile, wantEnvFiles)
	}

	for i, want := range wantEnvFiles {
		if spec.EnvFile[i] != want {
			t.Errorf("env_file[%d] = %q, want %q", i, spec.EnvFile[i], want)
		}
	}
}

// TestParseHCLStatefulsetComposeShapedFields mirrors the deployment
// test for statefulset — extra_hosts/cap_add/env_file live at root,
// build args live inside the nested build block. Image-mode here
// (no build {}) — different from the deployment test on purpose to
// exercise the image-mode path on the stateful kind.
func TestParseHCLStatefulsetComposeShapedFields(t *testing.T) {
	src := `
statefulset "test" "redis" {
  image       = "redis:7"
  cap_add     = ["IPC_LOCK"]
  extra_hosts = ["sentinel-bootstrap:10.0.0.10"]
  env_file    = ["./redis.env"]
}
`
	tmp := writeTemp(t, "sts.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec StatefulsetSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if len(spec.CapAdd) != 1 || spec.CapAdd[0] != "IPC_LOCK" {
		t.Errorf("cap_add: %+v", spec.CapAdd)
	}

	if len(spec.ExtraHosts) != 1 || spec.ExtraHosts[0] != "sentinel-bootstrap:10.0.0.10" {
		t.Errorf("extra_hosts: %+v", spec.ExtraHosts)
	}

	if len(spec.EnvFile) != 1 || spec.EnvFile[0] != "./redis.env" {
		t.Errorf("env_file: %+v", spec.EnvFile)
	}

	// Image-mode → Build must stay nil (no auto-synthesis).
	if spec.Build != nil {
		t.Errorf("image-mode statefulset must keep Build nil, got %+v", spec.Build)
	}
}

// TestParseHCLAppComposeShapedFields covers the `app` authoring sugar:
// the deployment-half should expose the same fields as a standalone
// deployment block (parity is the whole point of the sugar).
func TestParseHCLAppComposeShapedFields(t *testing.T) {
	src := `
app "test" "web" {
  image = "nginx:1.25"
  host  = "web.example.com"

  extra_hosts = ["api.internal:10.0.0.1"]
  cap_add     = ["NET_ADMIN"]
  env_file    = ["./web.env"]
}
`
	tmp := writeTemp(t, "app.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 2 {
		t.Fatalf("app should expand to (deployment, ingress) = 2 manifests, got %d", len(mans))
	}

	// First manifest should be the deployment (parser emits in that order).
	if mans[0].Kind != controller.KindDeployment {
		t.Fatalf("expected first manifest to be deployment, got %s", mans[0].Kind)
	}

	var spec DeploymentSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if len(spec.ExtraHosts) != 1 || spec.ExtraHosts[0] != "api.internal:10.0.0.1" {
		t.Errorf("extra_hosts: %+v", spec.ExtraHosts)
	}

	if len(spec.CapAdd) != 1 || spec.CapAdd[0] != "NET_ADMIN" {
		t.Errorf("cap_add: %+v", spec.CapAdd)
	}

	if len(spec.EnvFile) != 1 || spec.EnvFile[0] != "./web.env" {
		t.Errorf("env_file: %+v", spec.EnvFile)
	}
}

// TestParseHCLDeploymentEnvFrom pins env_from parsing on the
// deployment shape — it's the same field statefulset/job/cronjob
// already had, extended to deployment for parity. Common use case:
// sidecar/worker pattern where two deployments share secrets.
func TestParseHCLDeploymentEnvFrom(t *testing.T) {
	src := `
deployment "clowk-lp" "worker" {
  image    = "ghcr.io/clowk/worker:1.0"
  env_from = ["clowk-lp/web", "shared/credentials"]
}
`
	tmp := writeTemp(t, "envfrom.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if len(spec.EnvFrom) != 2 {
		t.Fatalf("env_from: got %+v, want 2 entries", spec.EnvFrom)
	}

	if spec.EnvFrom[0] != "clowk-lp/web" || spec.EnvFrom[1] != "shared/credentials" {
		t.Errorf("env_from order/values lost: %+v", spec.EnvFrom)
	}
}

// TestParseHCLAppEnvFrom — the `app` sugar must expose env_from too,
// otherwise it diverges from standalone deployment and ops authoring
// in the sugar form would have to drop back to the verbose pair just
// to inherit env. Parity is the whole point of the sugar.
func TestParseHCLAppEnvFrom(t *testing.T) {
	src := `
app "scope" "web" {
  image    = "nginx:1.27"
  host     = "x.example.com"
  env_from = ["shared/credentials"]
}
`
	tmp := writeTemp(t, "app-envfrom.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if len(spec.EnvFrom) != 1 || spec.EnvFrom[0] != "shared/credentials" {
		t.Errorf("env_from on app sugar: %+v", spec.EnvFrom)
	}
}

// TestParseHCLDeploymentImageBuildMutuallyExclusive pins the parse-
// time guard: declaring both `image = "..."` AND a `build {}` block
// on the same resource must error out, because they're two ways of
// spelling mutually exclusive intents. Operator picks one or the
// other.
func TestParseHCLDeploymentImageBuildMutuallyExclusive(t *testing.T) {
	src := `
deployment "test" "api" {
  image = "ghcr.io/me/api:v1"

  build {
    context = "."
  }
}
`
	tmp := writeTemp(t, "conflict.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error for image + build {} on same resource")
	}

	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutual-exclusivity error, got: %v", err)
	}
}

// TestParseHCLStatefulsetImageBuildMutuallyExclusive — same guard on
// the stateful kind.
func TestParseHCLStatefulsetImageBuildMutuallyExclusive(t *testing.T) {
	src := `
statefulset "data" "pg" {
  image = "postgres:16"

  build {
    context = "infra/postgres"
  }
}
`
	tmp := writeTemp(t, "conflict_sts.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error for image + build {} on statefulset")
	}
}

// TestParseHCLDeploymentAutoBuildDefault pins the implicit-build
// auto-detect path: a deployment with no image and no build block
// gets `Build = {Context: "."}` synthesised by applyDefaults — the
// "ship me from this repo, figure the rest out" shape.
func TestParseHCLDeploymentAutoBuildDefault(t *testing.T) {
	src := `deployment "test" "api" {}`

	tmp := writeTemp(t, "auto.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Build == nil {
		t.Fatal("auto-detect: expected Build synthesised, got nil")
	}

	if spec.Build.Context != "." {
		t.Errorf("auto-detect context: got %q, want %q", spec.Build.Context, ".")
	}
}

// TestParseHCLRegistry pins the canonical registry block shape:
// one label (the registry's name), `url`/`username`/`token` body
// attributes, no scope segment in the wire manifest, and the
// RegistrySpec carries through unchanged. The M2 contract this
// test locks in: registry is a CORE kind (KindRegistry), not a
// plugin block — `vd apply` must not route it through the plugin
// expand pipeline. A regression to plugin-block status would
// cause this test to fail at the `mans[0].Kind` assertion.
func TestParseHCLRegistry(t *testing.T) {
	src := `
registry "ghcr" {
  url      = "ghcr.io"
  username = "thadeu"
  token    = "ghp_secret"
}
`
	tmp := writeTemp(t, "registry.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(mans) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(mans))
	}

	got := mans[0]

	if got.Kind != controller.KindRegistry {
		t.Errorf("Kind: got %q, want %q (registry is a core kind, not a plugin block)", got.Kind, controller.KindRegistry)
	}

	if got.Scope != "" {
		t.Errorf("Scope: got %q, want empty (registry is unscoped — singleton per host)", got.Scope)
	}

	if got.Name != "ghcr" {
		t.Errorf("Name: got %q, want %q (block label)", got.Name, "ghcr")
	}

	var spec RegistrySpec
	if err := json.Unmarshal(got.Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.URL != "ghcr.io" {
		t.Errorf("URL: got %q, want %q", spec.URL, "ghcr.io")
	}

	if spec.Username != "thadeu" {
		t.Errorf("Username: got %q, want %q", spec.Username, "thadeu")
	}

	if spec.Token != "ghp_secret" {
		t.Errorf("Token: got %q, want %q", spec.Token, "ghp_secret")
	}
}

// TestParseHCLRegistry_PasswordAlias verifies the `password = "..."`
// ergonomic alias — some operators have muscle memory from older
// docker / ECR / Quay tutorials that spell the credential field
// `password`, and forcing them to translate to `token` is busywork
// that scares off the on-ramp. Both spellings must decode into the
// same wire Token field so the controller's auth-emission path
// stays a single shape regardless of which keyword the operator
// wrote.
func TestParseHCLRegistry_PasswordAlias(t *testing.T) {
	src := `
registry "dockerhub" {
  url      = "registry-1.docker.io"
  username = "robot"
  password = "dckr_pat_xyz"
}
`
	tmp := writeTemp(t, "registry_password.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var spec RegistrySpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Token != "dckr_pat_xyz" {
		t.Errorf("password alias: Token got %q, want %q (password should decode into Token)", spec.Token, "dckr_pat_xyz")
	}
}

// TestParseHCLRegistry_RequiredFields pins the parse-time
// rejection for half-configured blocks. A registry missing any
// of url / username / token would emit an `auths` entry that
// breaks docker's credential helper code path more often than
// it helps, so we reject at apply time with a kind-prefixed
// message the operator can act on directly.
func TestParseHCLRegistry_RequiredFields(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		wantSubs string
	}{
		{
			name: "missing url",
			src: `
registry "ghcr" {
  username = "thadeu"
  token    = "x"
}`,
			wantSubs: "url is required",
		},
		{
			name: "missing username",
			src: `
registry "ghcr" {
  url   = "ghcr.io"
  token = "x"
}`,
			wantSubs: "username is required",
		},
		{
			name: "missing token AND password",
			src: `
registry "ghcr" {
  url      = "ghcr.io"
  username = "thadeu"
}`,
			wantSubs: "token (or password) is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := writeTemp(t, "registry_invalid.hcl", tc.src)

			_, err := ParseFile(tmp, nil)
			if err == nil {
				t.Fatalf("want parse error containing %q, got nil", tc.wantSubs)
			}

			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSubs)
			}
		})
	}
}

// TestParseHCLDeploymentOnDeploy pins the on_deploy webhook
// block surface: success + failure sub-blocks, each with url +
// optional method + optional headers. URL round-trips verbatim
// (no trimming, no scheme rewriting). Method defaults are
// applied at request-build time on the controller, NOT at parse
// time — parse leaves Method "" when the operator omitted it.
// Headers map preserves operator keys verbatim.
func TestParseHCLDeploymentOnDeploy(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  on_deploy {
    success {
      url = "https://hooks.slack.com/services/T1/B1/abc"
    }

    failure {
      url    = "https://events.pagerduty.com/v2/enqueue"
      method = "POST"

      headers = {
        "Authorization" = "Token token=abc123"
        "X-Source"      = "voodu"
      }
    }
  }
}
`
	tmp := writeTemp(t, "dep_on_deploy.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.OnDeploy == nil {
		t.Fatal("on_deploy missing in parsed spec")
	}

	if spec.OnDeploy.Success == nil || spec.OnDeploy.Success.URL != "https://hooks.slack.com/services/T1/B1/abc" {
		t.Errorf("success url lost: %+v", spec.OnDeploy.Success)
	}

	if spec.OnDeploy.Success != nil && spec.OnDeploy.Success.Method != "" {
		t.Errorf("success.method should be empty when omitted (parser leaves default to runtime); got %q", spec.OnDeploy.Success.Method)
	}

	if spec.OnDeploy.Failure == nil || spec.OnDeploy.Failure.URL != "https://events.pagerduty.com/v2/enqueue" {
		t.Errorf("failure url lost: %+v", spec.OnDeploy.Failure)
	}

	if spec.OnDeploy.Failure.Method != "POST" {
		t.Errorf("failure.method: %q, want POST", spec.OnDeploy.Failure.Method)
	}

	if spec.OnDeploy.Failure.Headers["Authorization"] != "Token token=abc123" {
		t.Errorf("Authorization header lost: %+v", spec.OnDeploy.Failure.Headers)
	}

	if spec.OnDeploy.Failure.Headers["X-Source"] != "voodu" {
		t.Errorf("X-Source header lost: %+v", spec.OnDeploy.Failure.Headers)
	}
}

// TestParseHCLDeploymentOnDeploy_InvalidMethod pins the method
// whitelist. GET / HEAD / OPTIONS don't fit the "POST a JSON
// body to a receiver" contract, so they fail parse rather than
// produce a runtime webhook that the receiver silently rejects.
func TestParseHCLDeploymentOnDeploy_InvalidMethod(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  on_deploy {
    success {
      url    = "https://example.com/hook"
      method = "GET"
    }
  }
}
`
	tmp := writeTemp(t, "dep_on_deploy_get.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error for GET method on on_deploy webhook")
	}

	if !strings.Contains(err.Error(), "method must be one of") {
		t.Errorf("expected method-whitelist error, got %v", err)
	}
}

// TestParseHCLDeploymentOnDeploy_InlineBody pins that an HCL
// object literal in `body = { ... }` decodes into the spec as
// map[string]any (nested OK), round-trips through JSON
// serialisation, and survives parse-time validation.
func TestParseHCLDeploymentOnDeploy_InlineBody(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  on_deploy {
    failure {
      url = "https://events.pagerduty.com/v2/enqueue"

      body = {
        routing_key  = "ROUTING_KEY"
        event_action = "trigger"
        payload = {
          summary  = "voodu rollout {{name}} failed"
          severity = "error"
          custom_details = {
            release_id = "{{release_id}}"
          }
        }
      }
    }
  }
}
`
	tmp := writeTemp(t, "dep_on_deploy_body.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.OnDeploy == nil || spec.OnDeploy.Failure == nil {
		t.Fatal("failure block lost")
	}

	body := spec.OnDeploy.Failure.Body
	if body == nil {
		t.Fatal("inline body lost in parse")
	}

	if body["routing_key"] != "ROUTING_KEY" {
		t.Errorf("top-level field lost: %+v", body)
	}

	payload, ok := body["payload"].(map[string]any)
	if !ok {
		t.Fatalf("nested payload missing or wrong type: %T", body["payload"])
	}

	if payload["summary"] != "voodu rollout {{name}} failed" {
		t.Errorf("nested summary lost: %+v", payload)
	}

	cd, ok := payload["custom_details"].(map[string]any)
	if !ok {
		t.Fatalf("doubly-nested custom_details missing: %+v", payload)
	}

	if cd["release_id"] != "{{release_id}}" {
		t.Errorf("deeply-nested template token: %+v", cd)
	}
}

// TestParseHCLDeploymentOnDeploy_FileMustBeAssetRef pins that
// the `file` field rejects bare paths. Asset-only enforces a
// uniform resolution pipeline (host path + content hash) and
// avoids "relative to what?" ambiguity.
func TestParseHCLDeploymentOnDeploy_FileMustBeAssetRef(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  on_deploy {
    failure {
      url  = "https://example.com/hook"
      file = "./webhooks/body.json"
    }
  }
}
`
	tmp := writeTemp(t, "dep_on_deploy_bare_file.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error for bare path in file attribute")
	}

	if !strings.Contains(err.Error(), "must be an asset reference") {
		t.Errorf("expected asset-ref error, got %v", err)
	}
}

// TestParseHCLDeploymentOnDeploy_BodyAndFileMutex pins that
// declaring both inline body AND file is rejected at parse —
// they're conceptually two ways to do the same thing, and
// silent precedence would confuse the operator.
func TestParseHCLDeploymentOnDeploy_BodyAndFileMutex(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  on_deploy {
    success {
      url  = "https://example.com/hook"
      file = "${asset.prod.webhooks.template}"

      body = {
        text = "hello"
      }
    }
  }
}
`
	tmp := writeTemp(t, "dep_on_deploy_both.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error when both body and file declared")
	}

	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutex error, got %v", err)
	}
}

// TestParseHCLDeploymentOnDeploy_UrlRequired pins that a sub-block
// must carry a non-empty url. An empty url would silently no-op
// the webhook — better to fail parse and surface the typo.
func TestParseHCLDeploymentOnDeploy_UrlRequired(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  on_deploy {
    success {
      url = ""
    }
  }
}
`
	tmp := writeTemp(t, "dep_on_deploy_emptyurl.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error for empty url on on_deploy webhook")
	}

	if !strings.Contains(err.Error(), "url is required") {
		t.Errorf("expected url-required error, got %v", err)
	}
}

// TestParseHCLDeploymentLogs pins the logs cap block: both fields
// round-trip through the parser without docker-side coercion (no
// "10m" → bytes conversion at parse time; that's the runtime
// driver's job). Operator-declared values win over the platform
// default — which means TWO things this test asserts: the values
// land verbatim, AND applyLogsDefaults does NOT clobber them.
func TestParseHCLDeploymentLogs(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  logs {
    max_size  = "50m"
    max_files = 5
  }
}
`
	tmp := writeTemp(t, "dep_logs.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Logs == nil {
		t.Fatal("logs missing in parsed spec")
	}

	if spec.Logs.MaxSize != "50m" {
		t.Errorf("max_size: got %q, want %q", spec.Logs.MaxSize, "50m")
	}

	if spec.Logs.MaxFiles != 5 {
		t.Errorf("max_files: got %d, want 5", spec.Logs.MaxFiles)
	}
}

// TestApplyDefaults_LogsDefaults locks in the "no logs block →
// platform default" contract. Without this default, a deployment
// declared with the bare minimum (image only) would inherit
// docker's daemon default of unbounded logs — a crash-looping
// container could fill the host disk silently in hours. The
// default protects every deployment unless the operator opts
// out by declaring custom values.
func TestApplyDefaults_LogsDefaults(t *testing.T) {
	src := `deployment "prod" "api" { image = "nginx:1.27" }`

	tmp := writeTemp(t, "dep_default_logs.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec

	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Logs == nil {
		t.Fatal("logs default missing — applyDefaults must synthesise a LogsSpec when omitted")
	}

	if spec.Logs.MaxSize != "10m" {
		t.Errorf("default max_size: got %q, want %q", spec.Logs.MaxSize, "10m")
	}

	if spec.Logs.MaxFiles != 3 {
		t.Errorf("default max_files: got %d, want 3", spec.Logs.MaxFiles)
	}
}

// TestParseHCLDeploymentProbes pins the probes block surface — the
// three sub-blocks (liveness/readiness/startup), each carrying one
// action selector + threshold knobs. Values round-trip verbatim;
// the controller-side runner parses durations later (tolerantly).
func TestParseHCLDeploymentProbes(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"
  ports = ["8080"]

  probes {
    liveness {
      http_get {
        path = "/healthz"
        port = 8080
      }

      initial_delay     = "10s"
      period            = "10s"
      timeout           = "1s"
      failure_threshold = 3
    }

    readiness {
      http_get {
        path = "/ready"
        port = 8080
      }

      period = "5s"
    }

    startup {
      http_get {
        path = "/healthz"
        port = 8080
      }

      period            = "1s"
      failure_threshold = 30
    }
  }
}
`
	tmp := writeTemp(t, "dep_probes.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if spec.Probes == nil {
		t.Fatal("probes block lost")
	}

	if spec.Probes.Liveness == nil || spec.Probes.Liveness.HTTPGet == nil {
		t.Fatal("liveness http_get missing")
	}

	if spec.Probes.Liveness.HTTPGet.Path != "/healthz" || spec.Probes.Liveness.HTTPGet.Port != 8080 {
		t.Errorf("liveness http_get fields: %+v", spec.Probes.Liveness.HTTPGet)
	}

	if spec.Probes.Liveness.InitialDelay != "10s" || spec.Probes.Liveness.Period != "10s" || spec.Probes.Liveness.Timeout != "1s" {
		t.Errorf("liveness durations: %+v", spec.Probes.Liveness)
	}

	if spec.Probes.Liveness.FailureThreshold != 3 {
		t.Errorf("liveness failure_threshold: %d", spec.Probes.Liveness.FailureThreshold)
	}

	if spec.Probes.Readiness == nil || spec.Probes.Readiness.HTTPGet.Path != "/ready" {
		t.Errorf("readiness http_get: %+v", spec.Probes.Readiness)
	}

	if spec.Probes.Startup == nil || spec.Probes.Startup.FailureThreshold != 30 {
		t.Errorf("startup: %+v", spec.Probes.Startup)
	}
}

// TestParseHCLDeploymentProbes_TCPSocket pins the tcp_socket action
// — common for raw-TCP daemons (postgres, redis) where opening the
// port is the smallest reliable "alive" signal.
func TestParseHCLDeploymentProbes_TCPSocket(t *testing.T) {
	src := `
deployment "data" "redis" {
  image = "redis:7"

  probes {
    liveness {
      tcp_socket { port = 6379 }
      period = "5s"
    }
  }
}
`
	tmp := writeTemp(t, "dep_probes_tcp.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec
	_ = json.Unmarshal(mans[0].Spec, &spec)

	if spec.Probes == nil || spec.Probes.Liveness == nil || spec.Probes.Liveness.TCPSocket == nil {
		t.Fatal("tcp_socket missing")
	}

	if spec.Probes.Liveness.TCPSocket.Port != 6379 {
		t.Errorf("port: %d", spec.Probes.Liveness.TCPSocket.Port)
	}

	if spec.Probes.Liveness.HTTPGet != nil {
		t.Error("http_get should be nil when tcp_socket is declared")
	}
}

// TestParseHCLDeploymentProbes_Exec pins the exec action — used
// when a probe needs to query the app via an in-container CLI
// (pg_isready, redis-cli ping). Command rides through as a slice.
func TestParseHCLDeploymentProbes_Exec(t *testing.T) {
	src := `
deployment "data" "pg" {
  image = "postgres:16"

  probes {
    liveness {
      exec {
        command = ["pg_isready", "-h", "localhost"]
      }

      period = "10s"
    }
  }
}
`
	tmp := writeTemp(t, "dep_probes_exec.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec
	_ = json.Unmarshal(mans[0].Spec, &spec)

	if spec.Probes == nil || spec.Probes.Liveness.Exec == nil {
		t.Fatal("exec missing")
	}

	want := []string{"pg_isready", "-h", "localhost"}
	if len(spec.Probes.Liveness.Exec.Command) != len(want) {
		t.Fatalf("command length: %d, want %d", len(spec.Probes.Liveness.Exec.Command), len(want))
	}

	for i, v := range want {
		if spec.Probes.Liveness.Exec.Command[i] != v {
			t.Errorf("command[%d]: %q, want %q", i, spec.Probes.Liveness.Exec.Command[i], v)
		}
	}
}

// TestParseHCLDeploymentProbes_MutuallyExclusive validates the
// "exactly one selector" rule. Two selectors in one probe block
// is rejected at apply time with a clear error.
func TestParseHCLDeploymentProbes_MutuallyExclusive(t *testing.T) {
	src := `
deployment "x" "api" {
  image = "nginx:1.27"

  probes {
    liveness {
      http_get {
        path = "/h"
        port = 80
      }

      tcp_socket {
        port = 80
      }
    }
  }
}
`
	tmp := writeTemp(t, "dep_probes_conflict.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error when two selectors declared on same probe")
	}

	if !strings.Contains(err.Error(), "only one of") {
		t.Errorf("expected 'only one of' in error: %v", err)
	}
}

// TestParseHCLDeploymentProbes_NoSelectorFails verifies that a
// probe with no action declared (just durations / thresholds)
// fails fast — an empty probe is always-failing and would just
// spam restart calls.
func TestParseHCLDeploymentProbes_NoSelectorFails(t *testing.T) {
	src := `
deployment "x" "api" {
  image = "nginx:1.27"

  probes {
    liveness {
      period = "5s"
    }
  }
}
`
	tmp := writeTemp(t, "dep_probes_empty.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error when no probe action selector declared")
	}

	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("expected 'exactly one' in error: %v", err)
	}
}

// TestParseHCLDeploymentInitContainers pins the happy-path shape:
// multiple init blocks ride the deployment manifest with
// labels preserved, declaration order preserved, and field-by-field
// values round-tripping through the JSON encode/decode the apply
// pipeline uses.
func TestParseHCLDeploymentInitContainers(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "ghcr.io/acme/api:1.4"

  init "migrate" {
    command = ["bin/rails", "db:migrate"]
    timeout = "5m"
    retries = 2
  }

  init "warm-cache" {
    image   = "ghcr.io/acme/api:1.4"
    command = ["bin/warm-cache"]

    resources {
      limits {
        cpu    = "1"
        memory = "512Mi"
      }
    }
  }
}
`
	tmp := writeTemp(t, "dep_inits.hcl", src)

	mans, err := ParseFile(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}

	var spec DeploymentSpec
	if err := json.Unmarshal(mans[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}

	if len(spec.InitContainers) != 2 {
		t.Fatalf("want 2 init containers, got %d", len(spec.InitContainers))
	}

	if spec.InitContainers[0].Name != "migrate" {
		t.Errorf("init[0] name: %q, want migrate (order preserved)", spec.InitContainers[0].Name)
	}

	if got := spec.InitContainers[0].Command; len(got) != 2 || got[0] != "bin/rails" || got[1] != "db:migrate" {
		t.Errorf("init[0] command: %v", got)
	}

	if spec.InitContainers[0].Timeout != "5m" {
		t.Errorf("init[0] timeout: %q", spec.InitContainers[0].Timeout)
	}

	if spec.InitContainers[0].Retries != 2 {
		t.Errorf("init[0] retries: %d", spec.InitContainers[0].Retries)
	}

	if spec.InitContainers[1].Name != "warm-cache" {
		t.Errorf("init[1] name: %q", spec.InitContainers[1].Name)
	}

	if spec.InitContainers[1].Resources == nil || spec.InitContainers[1].Resources.Limits == nil {
		t.Fatalf("init[1] resources lost")
	}

	if spec.InitContainers[1].Resources.Limits.CPU != "1" || spec.InitContainers[1].Resources.Limits.Memory != "512Mi" {
		t.Errorf("init[1] resources: %+v", spec.InitContainers[1].Resources.Limits)
	}
}

// TestParseHCLDeploymentInitContainers_NoCommandFails verifies a
// command-less init is rejected at parse time. An init with no
// command would just re-run the image's CMD (the deployment's
// main entrypoint), which is almost always a misconfig — fail
// loudly instead of silently doing the wrong thing.
func TestParseHCLDeploymentInitContainers_NoCommandFails(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  init "noop" {
    timeout = "1m"
  }
}
`
	tmp := writeTemp(t, "dep_inits_nocmd.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error when init has no command")
	}

	if !strings.Contains(err.Error(), "requires command") {
		t.Errorf("expected 'requires command' error: %v", err)
	}
}

// TestParseHCLDeploymentInitContainers_DuplicateNameFails pins the
// uniqueness rule. Two init blocks with the same label would map
// to two docker containers with the same name suffix — a
// guaranteed runtime collision during the second's Recreate.
func TestParseHCLDeploymentInitContainers_DuplicateNameFails(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  init "migrate" {
    command = ["true"]
  }

  init "migrate" {
    command = ["true"]
  }
}
`
	tmp := writeTemp(t, "dep_inits_dup.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error on duplicate init container name")
	}

	if !strings.Contains(err.Error(), "duplicate init") {
		t.Errorf("expected 'duplicate init' error: %v", err)
	}
}

// TestParseHCLDeploymentInitContainers_BadNameFails verifies the
// charset rule. Names flow into docker container name segments,
// so anything outside [a-z0-9-] starting with a letter/digit is
// rejected at parse time.
func TestParseHCLDeploymentInitContainers_BadNameFails(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  init "Bad_Name" {
    command = ["true"]
  }
}
`
	tmp := writeTemp(t, "dep_inits_badname.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error on invalid init container name")
	}

	if !strings.Contains(err.Error(), "invalid name") {
		t.Errorf("expected 'invalid name' error: %v", err)
	}
}

// TestParseHCLDeploymentInitContainers_RetriesCap pins the
// retries ceiling. Retries above 5 indicate an antipattern (the
// init is presumed to be flaky and the operator wants the
// platform to mask it) — better surfaced loudly so they fix the
// init.
func TestParseHCLDeploymentInitContainers_RetriesCap(t *testing.T) {
	src := `
deployment "prod" "api" {
  image = "nginx:1.27"

  init "migrate" {
    command = ["true"]
    retries = 6
  }
}
`
	tmp := writeTemp(t, "dep_inits_retries.hcl", src)

	_, err := ParseFile(tmp, nil)
	if err == nil {
		t.Fatal("expected error on retries > 5")
	}

	if !strings.Contains(err.Error(), "retries must be in [0, 5]") {
		t.Errorf("expected retries-range error: %v", err)
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
