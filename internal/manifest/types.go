// Package manifest parses Voodu resource manifests (HCL or YAML) into
// the on-the-wire controller.Manifest shape. The controller never sees
// HCL/YAML — it only stores the JSON produced here.
//
// Supported kinds: deployment, database, ingress, job. Each kind has
// a typed Spec; the parser validates the shape before handing off to the
// controller, which keeps /apply errors local and readable.
//
// YAML tags drive the YAML path and JSON serialization into the
// controller wire format. HCL decoding goes through per-kind block
// structs in parse.go (hcl/v2 does not walk embedded specs), so these
// Spec structs stay free of HCL-specific tags.
package manifest

// DeploymentSpec is an app the controller should run as a container.
//
// Shape: the root holds pipeline- and container-level concerns (image,
// networking, env, post-deploy hooks, release retention). Language-
// specific build inputs live inside an optional `lang {}` block whose
// `name` field names the runtime — any value is accepted, so operators
// can target runtimes the platform doesn't know about (Elixir, Java,
// Haskell...) as long as they provide a custom Dockerfile.
//
// Source resolution is implicit. The handler picks a mode from which
// fields are set at reconcile time:
//
//   - Image non-empty        → pull from registry and run (no build)
//   - Image empty + Path set → docker build using <Workdir>/<Path> as context
//   - Image empty + no Path  → docker build at repo root
//
// Workdir narrows the repo subtree (monorepo case); Dockerfile picks a
// non-default filename. Both are ignored when Image is set.
//
// Language dispatch:
//   - `lang {}` is optional. When present, `name` picks the handler
//     (go, ruby, rails, python, nodejs, or any custom string — unknown
//     values fall through to the generic Dockerfile path).
//   - Zero block is fine: handlers auto-detect at build time from
//     well-known marker files (go.mod, Gemfile, package.json, ...).
//   - `version`, `entrypoint`, and `build_args` are forwarded to the
//     handler. Cross-compile flags (GOOS/GOARCH/CGO_ENABLED) live inside
//     `build_args` — the Go handler injects defaults and any explicit
//     entry overrides them.
//
// Networking:
//   - `network_mode = "host" | "none"` escapes docker bridge entirely.
//     Mutually exclusive with network/networks (container uses the host's
//     net stack directly). Use for apps with raw TCP/UDP needs (WebRTC,
//     SIP, RTP, socket servers) that can't live behind a bridge.
//   - `networks = [...]` — list of extra bridges to join. voodu0 is
//     always appended (it's the platform's plumbing bus for caddy +
//     plugins); operators can't opt out.
//   - `network = "..."` — legacy singular shorthand, treated as networks=[X].
//   - All empty → `[voodu0]`.
type DeploymentSpec struct {
	Image        string            `yaml:"image,omitempty"         json:"image,omitempty"`
	Workdir      string            `yaml:"workdir,omitempty"       json:"workdir,omitempty"`
	Dockerfile   string            `yaml:"dockerfile,omitempty"    json:"dockerfile,omitempty"`
	Path         string            `yaml:"path,omitempty"          json:"path,omitempty"`
	Replicas     int               `yaml:"replicas,omitempty"      json:"replicas,omitempty"`
	Command      []string          `yaml:"command,omitempty"       json:"command,omitempty"`
	Env          map[string]string `yaml:"env,omitempty"           json:"env,omitempty"`
	Ports        []string          `yaml:"ports,omitempty"         json:"ports,omitempty"`
	Volumes      []string          `yaml:"volumes,omitempty"       json:"volumes,omitempty"`
	Network      string            `yaml:"network,omitempty"       json:"network,omitempty"`
	Networks     []string          `yaml:"networks,omitempty"      json:"networks,omitempty"`
	NetworkMode  string            `yaml:"network_mode,omitempty"  json:"network_mode,omitempty"`
	Restart      string            `yaml:"restart,omitempty"       json:"restart,omitempty"`
	HealthCheck  string            `yaml:"health_check,omitempty"  json:"health_check,omitempty"`
	PostDeploy   []string          `yaml:"post_deploy,omitempty"   json:"post_deploy,omitempty"`
	KeepReleases int               `yaml:"keep_releases,omitempty" json:"keep_releases,omitempty"`

	// Lang is the single, runtime-agnostic build-input block. A nil
	// pointer means "not declared" — handlers fall back to auto-detect.
	Lang *LangSpec `yaml:"lang,omitempty" json:"lang,omitempty"`

	// Release is the optional release-phase block. When present, voodu
	// runs Command in a one-shot container BEFORE rolling restart of
	// the replicas — kubectl-/Heroku-style "migrate, then restart".
	// Failure of the release aborts the rollout: replicas stay on the
	// previous version, and the failure is recorded in the deployment
	// status's release history. See ReleaseSpec for details.
	Release *ReleaseSpec `yaml:"release,omitempty" json:"release,omitempty"`

	// DependsOn declares explicit dependencies — today only `assets`,
	// listing asset refs the consumer relies on. Server-side stamping
	// resolves these (plus textual ${asset.…} refs in other fields)
	// into AssetDigests at apply time.
	DependsOn *DependsOn `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`

	// Resources caps the container's CPU/memory at the kernel level
	// via cgroups (--cpus / --memory on docker run). See ResourcesSpec
	// for the field shape and value conventions.
	Resources *ResourcesSpec `yaml:"resources,omitempty" json:"resources,omitempty"`

	// AssetDigests is server-stamped at apply time: a sha256 per
	// asset ref the consumer uses. Folded into the spec hash so
	// asset content drift triggers rolling restart without the
	// operator needing to touch the manifest. Operators don't write
	// this field — the apply pipeline stamps it post plugin-expand.
	// Filtered out by `vd describe` (underscore prefix = internal).
	AssetDigests map[string]string `yaml:"-" json:"_asset_digests,omitempty"`
}

// ResourcesSpec is the k8s-shape resource constraint block. Voodu
// is a single-host docker runner today, so only the kernel-cap
// half of k8s (`limits`) is wired — `requests` is reserved for a
// future multi-host scheduler-aware mode and currently silently
// ignored when present.
//
//	resources {
//	  limits {
//	    cpu    = "2"        # 2 full CPUs (-> docker --cpus=2)
//	    memory = "4Gi"      # 4 binary gibibytes (-> docker --memory=4294967296)
//	  }
//	}
//
// CPU value formats:
//   - "2"      → 2 cpus
//   - "1.5"    → 1.5 cpus
//   - "500m"   → 0.5 cpus (millicores, k8s convention)
//
// Memory value formats:
//   - "4Gi"    → 4 * 1024^3 bytes (binary, k8s convention)
//   - "512Mi"  → 512 * 1024^2 bytes
//   - "1G"     → 1 * 1000^3 bytes (decimal SI)
//   - "1024"   → 1024 bytes (no suffix = bytes)
//
// Validation lands at parse time so apply fails loudly on a typo
// instead of silently emitting a container without limits.
type ResourcesSpec struct {
	Limits *ResourceLimits `yaml:"limits,omitempty" json:"limits,omitempty"`
}

// ResourceLimits caps the container at the kernel level via cgroups.
// Empty / unset fields mean "no limit" — docker daemon defaults apply
// (effectively unlimited until the host is exhausted).
type ResourceLimits struct {
	CPU    string `yaml:"cpu,omitempty"    json:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
}

// ReleaseSpec describes the one-shot command voodu runs between
// "manifest applied" and "rolling replicas" — the gap that
// migrations / cache warmup / smoke tests live in.
//
//	release {
//	  command = ["rails", "db:migrate"]
//	  timeout = "5m"
//
//	  pre_command  = ["bin/preflight"]   # optional
//	  post_command = ["bin/notify"]      # optional
//	}
//
// All commands run inside a fresh container spawned from the
// deployment's NEW image, so they see the new code (with whatever
// migrations / fixtures it ships) before any replica boots with
// it. Env is the merged scope+app config + manifest spec.env (same
// as the deployment containers see at boot).
//
// Lifecycle:
//
//	pre_command  →  command  →  rolling restart  →  post_command
//
// Any non-zero exit in pre_command or command aborts the rollout —
// post_command runs only after the rolling restart completes
// successfully. post_command failure is recorded but does NOT roll
// the deploy back; the new replicas are already live.
type ReleaseSpec struct {
	Command     []string `yaml:"command,omitempty"      json:"command,omitempty"`
	PreCommand  []string `yaml:"pre_command,omitempty"  json:"pre_command,omitempty"`
	PostCommand []string `yaml:"post_command,omitempty" json:"post_command,omitempty"`

	// Timeout caps each command's wall-clock duration. Format is
	// time.ParseDuration ("30s", "5m", "1h"). Defaults to "10m" when
	// omitted — enough for slow migrations on big tables, short
	// enough that a stuck command doesn't pin the rollout forever.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// LangSpec carries build-time inputs for the chosen runtime. The
// `name` field picks the handler; `version`, `entrypoint`, and
// `build_args` are universal — each handler reads what's meaningful to
// it and ignores the rest. That way new runtimes slot in without
// schema churn: the HCL shape is identical for Go, Ruby, Elixir, or a
// bespoke custom-Dockerfile app.
//
// build_args doubles as the escape hatch for lang-specific knobs.
// Go users pass `{ GOOS = "linux", GOARCH = "arm64", CGO_ENABLED = "0" }`
// here rather than getting a dedicated struct field — the Go handler
// auto-injects defaults and user entries override them.
type LangSpec struct {
	Name       string            `yaml:"name,omitempty"       json:"name,omitempty"`
	Version    string            `yaml:"version,omitempty"    json:"version,omitempty"`
	Entrypoint string            `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`
	BuildArgs  map[string]string `yaml:"build_args,omitempty" json:"build_args,omitempty"`
}

// applyDefaults fills implicit values so the minimal HCL
//
//	deployment "scope" "web" {}
//
// means "build the repo root, health-check /". Dockerfile is left
// empty on purpose: lang handlers auto-resolve it (use existing
// ./Dockerfile, else generate one for the detected runtime). Setting
// an explicit default here would make the server-side pipeline treat
// it as a *custom* dockerfile path and error out with a misleading
// "custom Dockerfile not found" when it's missing — blocking the
// auto-generation fallback that handlers like Rails/Ruby/Node rely
// on for zero-config builds.
//
// Path defaults to "." (build the repo root) in build mode only —
// registry-mode deployments (Image set) skip both defaults because
// build metadata is meaningless when no build runs. HealthCheck
// defaults in both modes because the ingress probe needs a path
// regardless of how the image was produced.
func (s *DeploymentSpec) applyDefaults() {
	if s.HealthCheck == "" {
		s.HealthCheck = "/"
	}

	if s.Image != "" {
		return
	}

	if s.Path == "" {
		s.Path = "."
	}
}

// StatefulsetSpec is a deployment shape with stable per-pod identity:
// each replica gets an integer ordinal (0, 1, 2, …) reflected in its
// container name (`<scope>-<name>.0`), its labels (voodu.replica_ordinal),
// and its DNS aliases (`<name>-0.<scope>`). Compared to DeploymentSpec:
//
//   - Replicas are NOT interchangeable: pod-0 is durably pod-0 across
//     restarts. Plugins like postgres lean on this — pod-0 is the
//     primary by convention, pg-1+ are followers.
//   - Storage attaches by ordinal (M-S2 introduces volume claim
//     templates). Volumes survive scale-down — operator opts into
//     destruction via `vd delete --prune`.
//   - Rollouts are ordered: scale-up provisions 0→N-1 sequentially,
//     scale-down removes N-1→0, rolling-replace iterates top-down.
//
// Build-mode (`workdir`/`dockerfile`/`path`/`lang {}`) is supported the
// same way DeploymentSpec supports it — when `Image` is empty the
// reconciler streams the operator's source via `voodu receive-pack`,
// runs the standard build pipeline, and tags `<scope>-<name>:latest`
// for the statefulset to pull. Use case: postgres + pgvector or redis
// with custom modules without needing a separate CI to publish the
// image.
//
// Networking and env behave identically to DeploymentSpec: voodu0
// auto-join, ports loopback by default, env from manifest+config
// merge. The shared aliases (`<name>.<scope>`) round-robin between
// replicas as before; per-pod aliases (`<name>-N.<scope>`) are the
// new affordance.
type StatefulsetSpec struct {
	Image       string            `yaml:"image,omitempty"        json:"image,omitempty"`
	Workdir     string            `yaml:"workdir,omitempty"      json:"workdir,omitempty"`
	Dockerfile  string            `yaml:"dockerfile,omitempty"   json:"dockerfile,omitempty"`
	Path        string            `yaml:"path,omitempty"         json:"path,omitempty"`
	Replicas    int               `yaml:"replicas,omitempty"     json:"replicas,omitempty"`
	Command     []string          `yaml:"command,omitempty"      json:"command,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"          json:"env,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"        json:"ports,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"      json:"volumes,omitempty"`
	Network     string            `yaml:"network,omitempty"      json:"network,omitempty"`
	Networks    []string          `yaml:"networks,omitempty"     json:"networks,omitempty"`
	NetworkMode string            `yaml:"network_mode,omitempty" json:"network_mode,omitempty"`
	Restart     string            `yaml:"restart,omitempty"      json:"restart,omitempty"`
	HealthCheck string            `yaml:"health_check,omitempty" json:"health_check,omitempty"`

	// Lang carries build-time inputs when Image is empty — same shape
	// and semantics as DeploymentSpec.Lang. Optional; the lang
	// handlers auto-detect from marker files (Gemfile, package.json,
	// go.mod, …) when omitted.
	Lang *LangSpec `yaml:"lang,omitempty" json:"lang,omitempty"`

	// EnvFrom stacks env files from other resources, same shape +
	// semantics as JobSpec.EnvFrom: each entry is `<scope>/<name>`
	// (or bare `<name>` for the current scope), the controller emits
	// a `--env-file` per entry BEFORE the statefulset's own env file
	// so the pod's merged env wins on conflicts. Multiple sources
	// stack in declared order.
	//
	// The canonical use case is a sentinel resource inheriting from
	// the data redis it monitors — REDIS_PASSWORD and
	// REDIS_MASTER_ORDINAL flow automatically without an operator
	// `vd config set` chore. Generalises beyond redis: any
	// statefulset that needs config from a peer (postgres replica
	// inheriting from primary's bucket, etc.).
	EnvFrom []string `yaml:"env_from,omitempty" json:"env_from,omitempty"`

	// VolumeClaims is the set of per-pod volume templates. Each
	// claim materialises into one docker volume per ordinal:
	// `voodu-<scope>-<name>-<claim>-<ordinal>`. Filled in M-S2;
	// the field is reserved on M-S0 so plugins authored against
	// the early shape don't need to migrate when the block lands.
	VolumeClaims []VolumeClaim `yaml:"volume_claims,omitempty" json:"volume_claims,omitempty"`

	// DependsOn declares explicit dependencies — see
	// DeploymentSpec.DependsOn for semantics. Same shape, same
	// stamping behaviour.
	DependsOn *DependsOn `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`

	// Resources caps the container's CPU/memory at the kernel level
	// — see DeploymentSpec.Resources for shape and value rules.
	Resources *ResourcesSpec `yaml:"resources,omitempty" json:"resources,omitempty"`

	// AssetDigests is server-stamped — see DeploymentSpec.AssetDigests.
	AssetDigests map[string]string `yaml:"-" json:"_asset_digests,omitempty"`
}

// applyDefaults fills implicit values for the build-mode case (mirror
// of DeploymentSpec.applyDefaults). Image-mode statefulsets skip this
// because Path/Workdir/Dockerfile are meaningless when no build runs.
//
// No HealthCheck default: statefulset workloads (postgres, redis,
// kafka) don't have a universal HTTP probe path the way HTTP apps do —
// the operator picks `pg_isready`, `redis-cli ping`, etc.
func (s *StatefulsetSpec) applyDefaults() {
	if s.Image != "" {
		return
	}

	if s.Path == "" {
		s.Path = "."
	}
}

// VolumeClaim is one per-pod storage template. Voodu provisions a
// docker named volume per ordinal at apply time and mounts it at
// MountPath inside every replica. The volume name is deterministic
// (`voodu-<scope>-<name>-<Name>-<ordinal>`) so reconcile-after-restart
// is idempotent.
//
// Size is currently informational — docker volumes don't enforce
// quotas at the daemon level. Storing it in the spec lets a future
// volume driver (loop-mount on a sized image, ZFS quota) honour it
// without a manifest re-roll.
type VolumeClaim struct {
	// Name disambiguates multiple claims on one statefulset (e.g.
	// "data" + "wal"). Must be lowercase alphanumeric — flows into
	// docker volume names which have a stricter charset than
	// arbitrary strings.
	Name string `yaml:"name" json:"name"`

	// MountPath is the path inside the container the volume binds
	// to. Required.
	MountPath string `yaml:"mount_path" json:"mount_path"`

	// Size is informational on M-S2 (docker has no native quota).
	// Format mirrors k8s ("10Gi", "500Mi").
	Size string `yaml:"size,omitempty" json:"size,omitempty"`
}

// AssetSpec is a bag of (key → source) pairs the controller
// materialises onto the host filesystem so deployments and
// statefulsets can mount the resulting paths via
// `${asset.<scope>.<name>.<key>}` (scoped) or
// `${asset.<name>.<key>}` (unscoped) interpolation.
//
// The shape is deliberately a flat map — there is no top-level
// metadata field. Convention: keys starting with `_` (like
// `_repo` for plugin blocks, `_labels` for nested HCL block
// labels) are reserved for future controller metadata; today
// none are recognised, so any underscore-prefixed key is just
// a regular file pair operators are free to declare.
//
// Each Source carries the materialisation hint:
//
//   - `{"_source":"file","content":"<base64>","filename":"x"}`
//     — bytes embedded by the CLI at apply time via `file("…")`
//   - `{"_source":"url","url":"https://…"}` — fetched server-side
//     during reconcile (cached by ETag/Last-Modified)
//   - plain string — inline literal, materialised verbatim
//
// HCL parses `key = file(…)` / `key = url(…)` / `key = "literal"`
// directly into this shape; YAML callers spell out the object
// form (see decodeYAMLSpec for how YAML rebuilds it).
type AssetSpec struct {
	// Files is keyed by the asset key (alphanumeric +
	// underscore). Values are heterogeneous JSON — string for
	// inline literals, object for file/url sources — so we
	// keep the field as RawMessage and let the handler decide
	// per-source how to materialise.
	Files map[string]any `yaml:"files,omitempty" json:"files,omitempty"`
}

// IngressSpec describes an externally reachable hostname. M6's voodu-caddy
// plugin reconciles these into a running Caddy config. Route rewriting
// and advanced matchers are out of scope for M4 — the caddy plugin will
// grow that vocabulary.
//
// `service` is optional: when omitted, the controller defaults it to the
// ingress name (so `ingress "api" { host = "api.example.com" }` just
// works). Explicit `service = "..."` still wins — use it for cross-app
// routing (ingress "public" → deployment "api").
//
// Path-based routing is expressed as zero-or-more `location {}` blocks.
// An empty Locations means "match everything for this host", which is
// the overwhelmingly common shape. Multiple entries let one ingress
// serve multiple path prefixes (e.g. `/api/v1` + `/api/v2` to the same
// backend). Strip controls whether the prefix is removed before the
// request reaches the container; default is preserve so the backend
// sees the original URI.
type IngressSpec struct {
	Host      string            `yaml:"host"                json:"host"`
	Service   string            `yaml:"service,omitempty"   json:"service,omitempty"`
	Port      int               `yaml:"port,omitempty"      json:"port,omitempty"`
	TLS       *IngressTLS       `yaml:"tls,omitempty"       json:"tls,omitempty"`
	Locations []IngressLocation `yaml:"locations,omitempty" json:"locations,omitempty"`
	LB        *IngressLB        `yaml:"lb,omitempty"        json:"lb,omitempty"`
}

// IngressLB configures how the ingress balances traffic across replicas
// of the target deployment. With a single replica it's effectively a
// no-op (Caddy still picks the only upstream). Two meaningful knobs:
//
//   - Policy: load-balancing algorithm. Values match Caddy's
//     `load_balancing.selection_policy` ("round_robin", "random",
//     "least_conn", "ip_hash"). Empty defaults to "round_robin".
//   - Interval: when non-empty, enables Caddy's active health check —
//     each upstream is probed at this cadence against the deployment's
//     health_check path, and an unhealthy upstream is taken out of
//     rotation until it recovers. Empty disables active probing
//     (Caddy still observes live requests for passive health).
type IngressLB struct {
	Policy   string `yaml:"policy,omitempty"   json:"policy,omitempty"`
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty"`
}

// IngressLocation is a single path match rule. Multiple entries per
// ingress are independent (caddy generates one route per location) and
// ordered by specificity at match time, not declaration order.
type IngressLocation struct {
	// Path is a URI prefix. Matching is prefix-based (caddy's `path`
	// matcher). Must start with `/`.
	Path string `yaml:"path" json:"path"`

	// Strip removes the matched prefix before forwarding to the
	// upstream. Default false (preserve) — common for apps that know
	// they live under a basePath. Set true when routing a generic image
	// (static nginx, arbitrary upstream) that expects root-relative URIs.
	Strip bool `yaml:"strip,omitempty" json:"strip,omitempty"`
}

// JobSpec is a one-shot container declaration. The controller registers
// the manifest at apply time but does NOT auto-execute — running a
// declared job is an explicit, imperative act (`voodu run job`).
// Apply-side responsibilities are limited to validation and persisting
// the spec so subsequent runs reuse it.
//
// The spec is a deliberately narrow subset of DeploymentSpec — the
// fields that have meaning for a process that exits:
//
//   - Image is required (build-from-source via `lang {}` is forwarded
//     to the standard build pipeline, but registry-mode is the M3
//     supported path; build-mode is best-effort).
//   - Replicas / Restart / HealthCheck / Ports are intentionally
//     absent: a job runs once, has no listening endpoint, and either
//     succeeds or fails. Restart-on-exit would defeat the point.
//   - Timeout caps execution. The reconciler honours it best-effort
//     (kills the container when the deadline passes); M4's cron
//     scheduler shares this knob.
type JobSpec struct {
	Image       string            `yaml:"image,omitempty"         json:"image,omitempty"`
	Workdir     string            `yaml:"workdir,omitempty"       json:"workdir,omitempty"`
	Dockerfile  string            `yaml:"dockerfile,omitempty"    json:"dockerfile,omitempty"`
	Path        string            `yaml:"path,omitempty"          json:"path,omitempty"`
	Command     []string          `yaml:"command,omitempty"       json:"command,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"           json:"env,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"       json:"volumes,omitempty"`
	Network     string            `yaml:"network,omitempty"       json:"network,omitempty"`
	Networks    []string          `yaml:"networks,omitempty"      json:"networks,omitempty"`
	NetworkMode string            `yaml:"network_mode,omitempty"  json:"network_mode,omitempty"`

	// EnvFrom stacks env files from other resources at run time.
	// Each entry is a `<scope>/<name>` ref (or bare `<name>` for
	// the current scope). At RunOnce, voodu emits a `--env-file`
	// per entry BEFORE the job's own env file, so the job's
	// merged env (scope + per-app + spec.env) wins on conflicts.
	//
	// Typical use: pair a job with the deployment whose image it
	// shares, so the job inherits DATABASE_URL/REDIS_URL/etc.
	// without redeclaration. Multiple sources stack in declared
	// order — common pattern is `[shared-secrets, paired-app]`.
	EnvFrom []string `yaml:"env_from,omitempty" json:"env_from,omitempty"`

	// Lang mirrors the deployment's lang block — same handler dispatch,
	// same escape-hatch via build_args. Jobs typically reuse a
	// deployment's image via `image = "my-app:latest"`, so the lang
	// block is rarely needed in practice.
	Lang *LangSpec `yaml:"lang,omitempty" json:"lang,omitempty"`

	// Timeout is a Go duration string (`"30s"`, `"5m"`). Empty means
	// no enforced cap. The controller kills + records a non-zero exit
	// when the deadline passes.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`

	// SuccessfulHistoryLimit / FailedHistoryLimit cap how many run
	// containers (and matching JobStatus.History entries) the runner
	// keeps after each invocation. The runner drops AutoRemove and
	// instead garbage-collects stopped run containers beyond these
	// caps, so `voodu logs job <name>` can replay the recent past.
	// Defaults match cronjobs: 3 successes, 1 failure.
	SuccessfulHistoryLimit int `yaml:"successful_history_limit,omitempty" json:"successful_history_limit,omitempty"`
	FailedHistoryLimit     int `yaml:"failed_history_limit,omitempty"     json:"failed_history_limit,omitempty"`

	// DependsOn declares explicit dependencies — see
	// DeploymentSpec.DependsOn for semantics.
	DependsOn *DependsOn `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`

	// Resources caps the container's CPU/memory at the kernel level
	// — see DeploymentSpec.Resources for shape and value rules.
	// Per-run cap; cron + standalone jobs share the same constraint
	// model as long-running deployments.
	Resources *ResourcesSpec `yaml:"resources,omitempty" json:"resources,omitempty"`

	// AssetDigests is server-stamped — see DeploymentSpec.AssetDigests.
	AssetDigests map[string]string `yaml:"-" json:"_asset_digests,omitempty"`
}

// DependsOn declares explicit, non-textual dependencies on other
// manifests. Currently only `assets` is supported — listing asset
// refs that the consumer depends on but doesn't necessarily have a
// `${asset.…}` interpolation for in any field.
//
// The `assets` list accepts both ref shapes:
//
//   - "name.key"               — 3-segment, unscoped global asset
//   - "scope.name.key"         — 4-segment, scoped asset
//
// Use this when the dependency is semantic-only (e.g. the app
// reads the asset path via env var injected by the controller, or
// "this redis must restart whenever <unrelated asset> changes")
// and therefore not visible to the textual-ref scanner. The asset
// stamping pipeline folds these refs into AssetDigests just like
// it folds in textual refs from volumes / command / env.
type DependsOn struct {
	Assets []string `yaml:"assets,omitempty" json:"assets,omitempty"`
}

// CronJobSpec wraps a JobSpec with a schedule. Apply registers the
// schedule; the controller's internal scheduler watches the wall clock
// and dispatches a JobHandler.RunOnce on each tick.
//
// Schedule grammar (M4): standard 5-field cron expression
//
//	"<min> <hour> <dom> <month> <dow>"
//
// Each field accepts a literal, "*", a comma-separated list, a step
// ("*\/15"), or a range ("0-30"). Seconds aren't supported (k8s parity
// — sub-minute schedules are almost never what an operator wants in a
// PaaS context).
//
// ConcurrencyPolicy mirrors k8s: "Allow" (default — overlapping runs
// are fine) or "Forbid" (skip a tick if the previous run hasn't
// finished). "Replace" — kill the in-flight run and start a new one
// — is reserved for a later milestone; until then the parser rejects
// it loudly so the manifest doesn't silently degrade.
//
// SuccessfulHistoryLimit / FailedHistoryLimit cap the JobStatus
// history bucket per cronjob (the same bucket JobHandler.RunOnce
// writes into). Defaults: 3 / 1, matching k8s defaults.
type CronJobSpec struct {
	// Schedule is the 5-field cron expression. Required.
	Schedule string `yaml:"schedule"            json:"schedule"`

	// Job is the spec each tick runs. Same shape as a standalone job —
	// it inherits the same image / command / env / network handling.
	Job JobSpec `yaml:"job"                 json:"job"`

	// ConcurrencyPolicy: "Allow" (default) or "Forbid".
	ConcurrencyPolicy string `yaml:"concurrency_policy,omitempty" json:"concurrency_policy,omitempty"`

	// Timezone is an IANA tz name ("UTC", "America/Sao_Paulo"). Empty
	// → UTC. The scheduler uses this when interpreting the schedule.
	Timezone string `yaml:"timezone,omitempty"  json:"timezone,omitempty"`

	// Suspend pauses dispatch without removing the manifest. Useful for
	// "stop running this for now without forgetting it exists".
	Suspend bool `yaml:"suspend,omitempty"   json:"suspend,omitempty"`

	// SuccessfulHistoryLimit caps successful runs in JobStatus.History.
	// Zero (the default) → 3.
	SuccessfulHistoryLimit int `yaml:"successful_history_limit,omitempty" json:"successful_history_limit,omitempty"`

	// FailedHistoryLimit caps failed runs in JobStatus.History.
	// Zero (the default) → 1.
	FailedHistoryLimit int `yaml:"failed_history_limit,omitempty" json:"failed_history_limit,omitempty"`
}

type IngressTLS struct {
	Enabled  bool   `yaml:"enabled,omitempty"  json:"enabled,omitempty"`
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Email    string `yaml:"email,omitempty"    json:"email,omitempty"`

	// On-demand TLS (wildcard multi-tenant). OnDemand turns on Caddy's
	// issue-cert-on-request behaviour; Ask is the gating callback URL.
	OnDemand bool   `yaml:"on_demand,omitempty" json:"on_demand,omitempty"`
	Ask      string `yaml:"ask,omitempty"       json:"ask,omitempty"`
}
