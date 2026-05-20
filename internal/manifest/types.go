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
// Shape: the root holds runtime concerns (replicas, env, networking,
// post-deploy hooks, release retention). Source-resolution and build-
// time concerns live inside an optional `build {}` block — see
// BuildSpec for the field shape.
//
// Source mode is mutually exclusive:
//
//   - `image = "..."` set → registry mode. The image is pulled from
//     the registry and run. Build is ignored (parse-time error if
//     both `image` and `build {}` are declared).
//   - `build { ... }` set OR neither set → build mode. The CLI
//     streams the working tree to the controller, which runs the
//     standard build pipeline. An absent `build {}` block defaults
//     to "build at repo root with auto-detected runtime" — the
//     terse "ship me from this repo, figure the rest out" shape.
//
// Build configuration lives in BuildSpec (Context, Dockerfile, Path,
// Args, Lang). docker-compose-shaped: `context` matches compose's
// `build.context`, `args` matches `build.args`.
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

	// Build holds the build-mode configuration when Image is empty.
	// Mutually exclusive with Image; see BuildSpec for the field shape.
	// nil + empty Image → implicit "build at repo root, auto-detect
	// runtime" (the terse shape applyDefaults synthesises).
	Build *BuildSpec `yaml:"build,omitempty" json:"build,omitempty"`

	// ExtraHosts maps to docker run `--add-host host:ip` entries on top
	// of the always-injected `host.docker.internal:host-gateway`. Each
	// entry is "name:ip" verbatim (validated at parse time). Useful for
	// pointing the container at internal services that don't have DNS
	// (legacy DB on a fixed IP, internal API on a VLAN, etc.). docker-
	// compose's `extra_hosts` field — identical semantics.
	ExtraHosts []string `yaml:"extra_hosts,omitempty" json:"extra_hosts,omitempty"`

	// CapAdd lists Linux capabilities to grant the container via docker
	// run `--cap-add`. Values are bare capability names without the
	// `CAP_` prefix (`SYS_NICE`, `NET_ADMIN`, `IPC_LOCK`, etc.). Mirrors
	// docker-compose `cap_add`. Common use cases: FreeSWITCH (SYS_NICE
	// for realtime scheduling), network tooling (NET_ADMIN), Redis with
	// memlock (IPC_LOCK). Avoid CAP_SYS_ADMIN unless you know exactly
	// why — it's effectively root in the container.
	CapAdd []string `yaml:"cap_add,omitempty" json:"cap_add,omitempty"`

	// EnvFile lists local file paths (relative to the operator's CWD)
	// whose KEY=value lines are merged into the spec's `env` map at
	// `vd apply` time. Files are read CLIENT-side; the controller sees
	// only the merged env map on the wire. Lets the operator keep
	// secrets out of the HCL — equivalent to docker-compose
	// `env_file: ./apps/foo/.env`.
	//
	// Precedence (last write wins, then operator-inline wins):
	//   1. env_file values (in declared order)
	//   2. inline `env = { ... }` block values
	//
	// Inline-wins means an operator who declared `env_file = "..."`
	// AND `env = { FOO = "explicit" }` gets FOO=explicit, even if the
	// .env file also declares FOO. Same Docker semantics.
	EnvFile []string `yaml:"env_file,omitempty" json:"env_file,omitempty"`

	// EnvFrom stacks env files from OTHER resources (not local files —
	// for those use EnvFile). Each entry is a `<scope>/<name>` ref
	// (or bare `<name>` for the current scope) pointing at any voodu-
	// managed resource that has a materialised env file at
	// /var/lib/voodu/apps/<id>.env (deployment, statefulset, even a
	// job).
	//
	// At reconcile, voodu emits one `--env-file` per entry BEFORE the
	// deployment's OWN env file, so the deployment's merged env (scope
	// config + per-app bucket + spec.Env + EnvFile) wins on conflicts.
	// Multiple sources stack in declared order (last one in the list
	// overrides earlier ones within the env_from stack itself).
	//
	// Use cases:
	//
	//   - Sidecar/web+worker pattern: web deployment owns the secrets,
	//     worker inherits via `env_from = ["web"]`.
	//   - Shared config bucket: `env_from = ["shared/credentials"]`
	//     pulls AWS keys, monitoring URLs, etc. from a config-only
	//     resource managed by `vd config set`.
	//   - Multi-tier inheritance: `env_from = ["shared, paired-api"]`
	//     base on shared secrets + paired API's env.
	EnvFrom []string `yaml:"env_from,omitempty" json:"env_from,omitempty"`

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

	// Autoscale is the optional CPU-driven horizontal autoscale block.
	// When set, voodu's autoscaler runs a periodic decision loop —
	// reading runtime CPU% via the same StatsCollector that powers
	// `vd stats` — and writes the new desired replica count back to
	// this deployment's spec. The reconciler picks the change up via
	// the standard watch path; no separate control plane.
	//
	// Mutually exclusive with the top-level `replicas` field at parse
	// time: an operator either pins a static count OR delegates to
	// the autoscaler, never both. See AutoscaleSpec for the per-field
	// contract (min/max bounds, target band, separate up/down
	// cooldowns).
	Autoscale *AutoscaleSpec `yaml:"autoscale,omitempty" json:"autoscale,omitempty"`

	// OnDeploy carries the optional webhook URLs invoked once a
	// rolling-restart completes (success) or fails. Best-effort —
	// see OnDeploySpec for the retry / drop contract. Empty / nil
	// is the steady-state shape: no posts, no log noise. Deliberately
	// NOT folded into the spec hash: changing a webhook URL must not
	// churn running replicas.
	OnDeploy *OnDeploySpec `yaml:"on_deploy,omitempty" json:"on_deploy,omitempty"`

	// Logs caps the docker json-file log driver per container. nil
	// means "platform default" — applyDefaults synthesises a
	// 10m/3-files cap so a runaway container can't fill the host
	// disk silently. See LogsSpec for the operator-facing contract.
	Logs *LogsSpec `yaml:"logs,omitempty" json:"logs,omitempty"`

	// Probes declares kubelet-style health checks voodu runs against
	// each replica. M1.1 wires liveness only — readiness + startup
	// land in M1.2 alongside ingress integration. See ProbesSpec for
	// the full contract; when nil, voodu relies on docker's restart
	// policy alone (no in-app health gating, no automatic restart on
	// process-alive-but-deadlocked).
	Probes *ProbesSpec `yaml:"probes,omitempty" json:"probes,omitempty"`

	// InitContainers declares ordered one-shot containers that must
	// each run to completion (exit 0) before the deployment's main
	// container starts for a given replica. K8s-parity contract:
	// inits run sequentially, share the deployment's env / networks /
	// volumes / extra_hosts / cap_add (so a migration step can write
	// the data the main process will read), and run again for every
	// freshly spawned replica (scale-up, rolling-restart). A failing
	// init blocks its replica from coming up; the failure surfaces
	// in DeploymentStatus.InitFailures. See InitContainerSpec for the
	// per-step shape.
	//
	// HCL surface is `init "name" { ... }` (no `_container` suffix —
	// voodu's manifest layer talks about pods, not containers). The
	// Go type name keeps "InitContainer" since the k8s concept is
	// the reference; the operator-facing keyword is just `init`.
	InitContainers []InitContainerSpec `yaml:"inits,omitempty" json:"inits,omitempty"`

	// AssetDigests is server-stamped at apply time: a sha256 per
	// asset ref the consumer uses. Folded into the spec hash so
	// asset content drift triggers rolling restart without the
	// operator needing to touch the manifest. Operators don't write
	// this field — the apply pipeline stamps it post plugin-expand.
	// Filtered out by `vd describe` (underscore prefix = internal).
	AssetDigests map[string]string `yaml:"-" json:"_asset_digests,omitempty"`
}

// InitContainerSpec describes one ordered prep step that must run
// to completion (exit 0) before a replica's main container starts.
// Operator-facing HCL keyword is `init` (the "container" suffix
// is dropped at the surface — voodu's manifest layer talks about
// pods, not containers — though the k8s "init container" concept
// is what's being implemented).
//
//	deployment "prod" "api" {
//	  image = "ghcr.io/acme/api:1.4"
//
//	  init "migrate" {
//	    command = ["bin/rails", "db:migrate"]
//	  }
//
//	  init "warm-cache" {
//	    command = ["bin/warm"]
//	  }
//	}
//
// The parser collects the blocks into this slice in declared
// order. Order matters: each init waits for the previous to
// succeed before starting.
//
// Inheritance rules (deployment → init container):
//
//   - Image defaults to the deployment's Image when empty. The
//     overwhelmingly common case ("run rails db:migrate with my
//     app's image") then needs zero `image` repetition.
//   - Command is REQUIRED — an init with no command would just
//     run the image's CMD, which is almost always wrong (you'd
//     be re-running the app's server entrypoint).
//   - Env, env_file, env_from, networks, volumes, extra_hosts,
//     cap_add: all inherited verbatim. The init sees the same
//     environment the main pod will see — that's the whole point
//     (writing to shared volumes, talking to the same services).
//   - Resources: per-init override; defaults to no limits when
//     omitted (the main pod's resources are a steady-state cap;
//     a one-shot migration may legitimately need more headroom).
//   - Timeout: per-attempt wall-clock cap. Defaults to "10m" when
//     empty (matches ReleaseSpec.Timeout — same rationale: long
//     enough for slow migrations, short enough that a stuck step
//     can't pin the rollout forever).
//   - Retries: number of additional attempts after the first
//     failure. 0 = "try once". Capped at 5 by the parser to
//     guard against chronic-failure loops.
//
// Init containers do NOT count as healthy replicas — they're
// transient prep work, not part of the steady-state pod count.
type InitContainerSpec struct {
	// Name is the operator-supplied identifier. Becomes the HCL
	// block label and the container's last name segment so an
	// operator can `docker logs voodu-prod-api-init-migrate-a3f9`
	// after a failed run. Charset: lowercase, digits, hyphens
	// (same shape as resource names).
	Name string `yaml:"name" json:"name"`

	// Image is the registry image to spawn for this step. Empty
	// inherits from the parent deployment — the common case for
	// "run this command with my app's image."
	Image string `yaml:"image,omitempty" json:"image,omitempty"`

	// Command is the entrypoint slice (`["bin/rails", "db:migrate"]`)
	// the init runs. Required — see type doc for rationale.
	Command []string `yaml:"command,omitempty" json:"command,omitempty"`

	// Timeout caps each ATTEMPT's wall-clock duration. Format is
	// time.ParseDuration ("30s", "5m"). Empty / unparseable falls
	// through to the platform default ("10m").
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`

	// Retries is the number of additional attempts after the first
	// failure (total attempts = 1 + Retries). Capped at 5; values
	// above 5 are rejected at parse time as antipatterns.
	Retries int `yaml:"retries,omitempty" json:"retries,omitempty"`

	// Resources caps CPU/memory for this init at the kernel level
	// — same shape as DeploymentSpec.Resources. nil means "no
	// limit", which is the operator-friendly default for prep
	// steps that may legitimately exceed steady-state caps.
	Resources *ResourcesSpec `yaml:"resources,omitempty" json:"resources,omitempty"`
}

// AutoscaleSpec is the M7 CPU-based horizontal autoscale block. Lives
// on DeploymentSpec.Autoscale. When present, voodu's autoscaler owns
// the deployment's effective replica count: a periodic loop (15s
// default) reads CPU% across running replicas via the shared
// StatsCollector and adjusts replicas up or down by one within the
// declared bounds.
//
//	deployment "clowk-lp" "worker" {
//	  image = "..."
//
//	  autoscale {
//	    min           = 2
//	    max           = 10
//	    cpu_target    = 70
//	    cooldown_up   = "30s"     # default 30s
//	    cooldown_down = "5m"      # default 5m
//	  }
//	}
//
// Decision band (hysteresis to dampen thrash):
//
//   - mean CPU > target * 1.1 → scale up by 1 (if replicas < max
//     AND now - lastUp >= cooldown_up)
//   - mean CPU < target * 0.7 → scale down by 1 (if replicas > min
//     AND now - lastDown >= cooldown_down)
//   - otherwise: hold
//
// Asymmetric cooldown is deliberate: scale-up is cheap to undo, so
// the operator wants quick response to load; scale-down is what
// causes 503s under bursty traffic, so it's intentionally
// conservative. Defaults (30s up, 5m down) reflect the
// "respond fast, retreat slowly" posture most workloads want.
//
// Validation (parse-time):
//   - Min >= 1 (zero-replica deployments aren't supported)
//   - Max >= Min
//   - CPUTarget in (0, 100]
//   - Mutex against DeploymentSpec.Replicas (one or the other)
//
// CooldownUp / CooldownDown are time.ParseDuration strings. Empty
// values default at controller-side, not parse-side — the operator
// can write `autoscale { min = 2 max = 10 cpu_target = 70 }` and
// get the defaults without typing them.
type AutoscaleSpec struct {
	// Min is the floor — the autoscaler will never scale below this.
	// Doubles as the initial replica count when applyDefaults runs
	// (so a deployment with autoscale but no explicit replicas boots
	// with Min pods, not 1).
	Min int `yaml:"min,omitempty" json:"min,omitempty"`

	// Max is the ceiling. Hard-stop on scale-up — protects the host
	// from runaway scale events under genuine traffic spikes.
	Max int `yaml:"max,omitempty" json:"max,omitempty"`

	// CPUTarget is the per-replica CPU% the autoscaler tries to
	// keep the fleet's mean at. The hysteresis bands (target * 0.7
	// to target * 1.1) widen the "hold" zone so noise doesn't
	// trigger churn.
	CPUTarget int `yaml:"cpu_target,omitempty" json:"cpu_target,omitempty"`

	// CooldownUp is the minimum wall-clock duration between scale-
	// up events. time.ParseDuration string ("30s", "1m", "2m30s").
	// Empty → 30s default. Short by design — scale-up is the cheap
	// direction.
	CooldownUp string `yaml:"cooldown_up,omitempty" json:"cooldown_up,omitempty"`

	// CooldownDown is the minimum wall-clock duration between
	// scale-down events. Empty → 5m default. Long by design — a
	// fleet that scaled up under a 30s burst shouldn't immediately
	// scale back down only to flap up again on the next burst.
	CooldownDown string `yaml:"cooldown_down,omitempty" json:"cooldown_down,omitempty"`
}

// OnDeploySpec carries the operator-supplied webhook
// endpoints invoked at the END of a rolling-restart. Both
// sub-blocks are optional and independent: an operator who only
// cares about failure pages declares `failure` alone and skips
// the chatty "everything's fine" pings.
//
//	deployment "x" "api" {
//	  on_deploy {
//	    success {
//	      url = "https://hooks.slack.com/services/T../B../..."
//	    }
//
//	    failure {
//	      url     = "https://events.pagerduty.com/v2/enqueue"
//	      method  = "POST"               # default
//	      headers = {
//	        "Authorization" = "Token token=${PD_TOKEN}"
//	      }
//	    }
//	  }
//	}
//
// Delivery is best-effort. The controller sends a small JSON
// payload (kind, scope, name, release_id, image, status, error,
// timestamps) to each endpoint, retries 3 times with exponential
// backoff (1s, 5s, 30s), and then drops on the floor. A webhook
// failure NEVER fails the deploy — the rollout already happened
// and there's nothing useful to "fail" by then.
//
// Payload shape stays fixed (see controller.WebhookPayload).
// Header + method customisation lets the operator hit real
// service APIs (PagerDuty Events v2, Datadog, internal
// receivers) without a transformer in the middle.
type OnDeploySpec struct {
	Success *DeployWebhook `yaml:"success,omitempty" json:"success,omitempty"`
	Failure *DeployWebhook `yaml:"failure,omitempty" json:"failure,omitempty"`
}

// DeployWebhook is one configured webhook target — URL plus the
// optional HTTP knobs the receiver may need to accept the call.
// Used inside OnDeploySpec for both the success and failure
// slots; future hook kinds (pre_apply, on_drift) will reuse the
// same shape so operators learn it once.
type DeployWebhook struct {
	// URL is the absolute endpoint to hit. Required. Operators
	// commonly use `${VAR}` shell-env interpolation to keep the
	// secret-bearing part (Slack incoming-webhook tokens, signed
	// query strings) out of the manifest text.
	URL string `yaml:"url" json:"url"`

	// Method is the HTTP verb. Defaults to "POST" when empty.
	// Parser validates against {POST, PUT, PATCH, DELETE} — GET
	// and HEAD don't carry request bodies, and the whole point
	// of an on_deploy webhook is to ship the JSON payload to the
	// receiver.
	Method string `yaml:"method,omitempty" json:"method,omitempty"`

	// Headers map sets extra request headers. Use this for
	// `Authorization` (most non-Slack receivers), routing keys,
	// or service-specific X-headers (X-Honeycomb-Team,
	// DD-API-KEY, etc.). Values support `${VAR}` interpolation
	// so secrets stay out of the manifest.
	//
	// Voodu always sets the User-Agent header to
	// "voodu-deploy-webhook" — operators cannot override that
	// (the source identifier is useful for debugging on the
	// receiver side). Content-Type defaults to
	// "application/json"; declaring it here overrides the
	// default.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// Body is the inline JSON body sent to the webhook. The
	// operator declares an HCL object literal or a YAML map; it
	// gets JSON-marshalled and POSTed verbatim, replacing
	// voodu's default WebhookPayload. Mutex with File.
	//
	// Empty (and File empty) → voodu sends the default
	// WebhookPayload (kind, scope, name, release_id, image,
	// status, started_at, completed_at, error). That's the
	// backward-compat path for the 90% of receivers that just
	// want "any JSON".
	//
	// String values within Body support two interpolation
	// contexts: `${VAR}` resolves shell env at parse time on
	// the operator's machine (same as URL / Headers); `{{field}}`
	// resolves deploy fields at fire time on the controller
	// (see WebhookPayload for the field list — name, status,
	// release_id, etc.).
	Body map[string]any `yaml:"body,omitempty" json:"body,omitempty"`

	// File is an asset reference pointing at a JSON template
	// file. The file's content becomes the webhook body (after
	// `{{field}}` substitution at fire time). Use this when the
	// body is too rich to keep inline — Slack Block Kit,
	// PagerDuty Events API v2, Telegram bot payloads, etc.
	// Mutex with Body.
	//
	// Format: an asset reference like
	// `${asset.<scope>.<name>.<key>}`. Bare paths are rejected
	// — assets give voodu the materialised host path + content
	// hash without operators needing to think about relative-
	// to-what semantics.
	//
	// Content type is opaque to voodu; the receiver decides
	// (declare Content-Type in Headers if not application/json).
	File string `yaml:"file,omitempty" json:"file,omitempty"`
}

// LogsSpec caps the docker json-file log driver per container.
// Equivalent to `docker run --log-opt max-size=X --log-opt
// max-file=Y`. Empty / nil block falls through to the platform's
// 10m / 3-files default (applied by applyDefaults) — voodu
// refuses to ship a container with unbounded logs because a
// chatty crash loop will fill the host disk silently otherwise.
//
//	deployment "x" "api" {
//	  logs {
//	    max_size  = "10m"
//	    max_files = 3
//	  }
//	}
//
// Value formats:
//   - MaxSize: k8s/docker-style suffixed string. "10m" = 10 MB,
//     "1g" = 1 GB, "500k" = 500 KB. Bare digits are accepted by
//     docker (interpreted as bytes); voodu passes through the
//     literal string after validating shape only.
//   - MaxFiles: positive integer. Docker rotates the active log
//     file when it hits MaxSize, keeps the most-recent MaxFiles-1
//     historical files alongside it (so total disk = roughly
//     MaxSize * MaxFiles).
//
// Folded into the spec hash: changing the cap is a runtime
// concern that requires recreating the container (docker freezes
// log driver options at create time, `docker update` does not
// touch them).
type LogsSpec struct {
	MaxSize  string `yaml:"max_size,omitempty"  json:"max_size,omitempty"`
	MaxFiles int    `yaml:"max_files,omitempty" json:"max_files,omitempty"`
}

// Defaults applied when the operator omits the logs block entirely.
// 10m * 3 files = ~30MB max per container — enough headroom for
// reasonable apps to keep recent history without risking the host's
// rootfs.
const (
	defaultLogsMaxSize  = "10m"
	defaultLogsMaxFiles = 3
)

// ProbesSpec is the operator-facing surface for kubelet-style
// health checks. Three probe kinds, exactly mirroring Kubernetes:
//
//	probes {
//	  liveness  { http_get { path = "/healthz", port = 8080 } }
//	  readiness { http_get { path = "/ready",   port = 8080 } }   # M1.2
//	  startup   { http_get { path = "/healthz", port = 8080 }, failure_threshold = 30 }   # M1.2
//	}
//
// Liveness is the M1.1 cut: when a probe fails past its
// failure_threshold, the controller restarts the container in-place
// (docker restart — keeps the same container ID, restarts the
// process). Catches deadlocks / hung event loops that docker's
// process-exit-based restart policy can't see.
//
// Readiness + startup wire in M1.2: readiness gates ingress
// traffic (caddy upstream membership), startup gives slow-boot apps
// a grace window before liveness counts failures.
type ProbesSpec struct {
	Liveness  *ProbeSpec `yaml:"liveness,omitempty"  json:"liveness,omitempty"`
	Readiness *ProbeSpec `yaml:"readiness,omitempty" json:"readiness,omitempty"`
	Startup   *ProbeSpec `yaml:"startup,omitempty"   json:"startup,omitempty"`
}

// ProbeSpec is one probe's full configuration: which action to run
// (http_get / tcp_socket / exec — exactly one), how often, and how
// many consecutive failures count as "down."
//
// Action fields are mutually exclusive — the parser enforces "exactly
// one selector" via probe.Spec.Validate(). All thresholds default at
// the controller side per the k8s-parity defaults (period 10s,
// timeout 1s, failure_threshold 3, success_threshold 1).
//
// Duration fields (InitialDelay, Period, Timeout) accept Go's
// time.ParseDuration format ("10s", "1m", "1m30s"). Empty strings
// fall through to defaults.
type ProbeSpec struct {
	HTTPGet   *HTTPGetAction   `yaml:"http_get,omitempty"   json:"http_get,omitempty"`
	TCPSocket *TCPSocketAction `yaml:"tcp_socket,omitempty" json:"tcp_socket,omitempty"`
	Exec      *ExecAction      `yaml:"exec,omitempty"       json:"exec,omitempty"`

	InitialDelay     string `yaml:"initial_delay,omitempty"     json:"initial_delay,omitempty"`
	Period           string `yaml:"period,omitempty"            json:"period,omitempty"`
	Timeout          string `yaml:"timeout,omitempty"           json:"timeout,omitempty"`
	FailureThreshold int    `yaml:"failure_threshold,omitempty" json:"failure_threshold,omitempty"`
	SuccessThreshold int    `yaml:"success_threshold,omitempty" json:"success_threshold,omitempty"`
}

// HTTPGetAction issues an HTTP GET against the container; 2xx/3xx
// counts as success. Port is required; the parser doesn't auto-pick
// from spec.ports because that coupling would break in non-trivial
// shapes (multi-port containers, sidecars).
type HTTPGetAction struct {
	Path        string            `yaml:"path"                  json:"path"`
	Port        int               `yaml:"port"                  json:"port"`
	Scheme      string            `yaml:"scheme,omitempty"      json:"scheme,omitempty"`
	HTTPHeaders map[string]string `yaml:"http_headers,omitempty" json:"http_headers,omitempty"`
}

// TCPSocketAction is the "can I connect?" probe — useful for raw-TCP
// daemons (postgres, redis) where opening the port is the smallest
// reliable "alive" signal.
type TCPSocketAction struct {
	Port int `yaml:"port" json:"port"`
}

// ExecAction runs a command INSIDE the container via docker exec.
// Exit code 0 = success; anything else fails. Command is the argv;
// shell expansion is NOT performed (use `sh -c "..."` if you need
// pipes or redirection).
type ExecAction struct {
	Command []string `yaml:"command" json:"command"`
}

// BuildSpec is the docker-compose-shaped build-mode block. Declared
// inside a resource (`build { ... }`) when the operator wants voodu to
// build the image from source instead of pulling it from a registry.
// Mutually exclusive with the resource's `image` field — parse fails
// loudly if both are set.
//
//	deployment "scope" "api" {
//	  build {
//	    context    = "apps/api"        # directory sent as docker build context
//	    dockerfile = "Dockerfile.api"  # custom name, relative to context
//	    path       = "cmd/api"         # only used by auto-generated Dockerfiles
//	    args       = { SERVICE = "api" }
//
//	    lang {
//	      name    = "go"
//	      version = "1.25"
//	    }
//	  }
//	}
//
// Field semantics:
//
//   - Context: the directory whose contents are sent to `docker build`
//     as the build context. Defaults to "." (repo root) when build {}
//     is declared but context is omitted. Matches docker-compose's
//     `build.context`.
//   - Dockerfile: picks a non-default Dockerfile name relative to
//     Context. Empty means look for "./Dockerfile" inside Context; if
//     none exists, language handlers may auto-generate one. Matches
//     `build.dockerfile`.
//   - Path: voodu-specific knob, used ONLY by auto-generated
//     Dockerfiles (Go handler emits `go build ./<path>`, etc.).
//     Custom Dockerfiles ignore this field — they handle their own
//     COPY/WORKDIR. No docker-compose equivalent.
//   - Args: docker `--build-arg KEY=value` map. Matches
//     `build.args`. Works for any Dockerfile.
//   - Lang: optional runtime hint. nil = handlers auto-detect from
//     marker files (go.mod, Gemfile, package.json, …) in Context.
//
// Auto-detect (omitting build {} entirely):
//
//	deployment "scope" "web" {}   # no image, no build → "build at repo
//	                              # root, auto-detect runtime"
//
// applyDefaults synthesises `Build = &BuildSpec{Context: "."}` for the
// minimal shape, so handlers downstream don't need to special-case.
type BuildSpec struct {
	Context    string            `yaml:"context,omitempty"    json:"context,omitempty"`
	Dockerfile string            `yaml:"dockerfile,omitempty" json:"dockerfile,omitempty"`
	Path       string            `yaml:"path,omitempty"       json:"path,omitempty"`
	Args       map[string]string `yaml:"args,omitempty"       json:"args,omitempty"`
	Lang       *LangSpec         `yaml:"lang,omitempty"       json:"lang,omitempty"`
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

// LangSpec is the runtime-handler hint inside a `build {}` block.
// The `name` field picks the handler (go, ruby, rails, python,
// nodejs, or any custom string — unknown values fall through to
// the generic Dockerfile path). `version` flows into auto-generated
// Dockerfiles where the handler knows what to do with it (Go base
// image tag, Ruby version pin, …). `entrypoint` lets the operator
// override the handler's default CMD.
//
// Build args live on the parent BuildSpec (`build.args = {...}`),
// NOT here — one source of truth. The Golang handler still injects
// platform defaults (GOOS/GOARCH/CGO_ENABLED) internally; operators
// override via `build.args = { GOOS = "darwin" }`.
type LangSpec struct {
	Name       string `yaml:"name,omitempty"       json:"name,omitempty"`
	Version    string `yaml:"version,omitempty"    json:"version,omitempty"`
	Entrypoint string `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`
}

// applyDefaults fills implicit values so the minimal HCL
//
//	deployment "scope" "web" {}
//
// means "build the repo root, health-check /". Dockerfile inside the
// synthesised Build is left empty on purpose: lang handlers auto-
// resolve it (use existing ./Dockerfile, else generate one for the
// detected runtime). Setting an explicit default would make the
// server-side pipeline treat it as a *custom* dockerfile path and
// error out with a misleading "custom Dockerfile not found" when it's
// missing — blocking the auto-generation fallback that handlers like
// Rails/Ruby/Node rely on for zero-config builds.
//
// Build defaults to `{Context: "."}` only when Image is empty (build
// mode). Registry-mode deployments (Image set) skip the synthesis
// because build metadata is meaningless when no build runs.
// HealthCheck defaults in both modes because the ingress probe needs
// a path regardless of how the image was produced.
func (s *DeploymentSpec) applyDefaults() {
	if s.HealthCheck == "" {
		s.HealthCheck = "/"
	}

	// When `autoscale {}` is declared and the operator hasn't pinned
	// a starting replica count, seed Replicas with autoscale.Min so
	// the first reconcile boots the floor (Min pods), not the
	// effectiveReplicas() default of 1. The autoscaler then takes
	// over and writes new counts back to spec.Replicas as load
	// dictates — Min stays the floor, Max the ceiling.
	//
	// Mutex against an explicit `replicas = N` is enforced at parse
	// time (errAutoscaleReplicasMix), so by the time we land here we
	// know s.Replicas is zero whenever s.Autoscale is set.
	if s.Autoscale != nil && s.Replicas == 0 && s.Autoscale.Min > 0 {
		s.Replicas = s.Autoscale.Min
	}

	s.Logs = applyLogsDefaults(s.Logs)

	if s.Image != "" {
		return
	}

	if s.Build == nil {
		s.Build = &BuildSpec{}
	}

	if s.Build.Context == "" {
		s.Build.Context = "."
	}
}

// applyLogsDefaults fills the platform's 10m / 3-files cap when the
// operator omitted the `logs {}` block entirely OR declared it
// with one field set and the other left zero. Returns a non-nil
// LogsSpec in every case — voodu's posture is "every container
// gets a cap, no exceptions". An operator who explicitly wants
// unbounded logs has to fork the controller; the silent disk-fill
// failure mode the default prevents is too easy to hit otherwise.
func applyLogsDefaults(s *LogsSpec) *LogsSpec {
	if s == nil {
		return &LogsSpec{
			MaxSize:  defaultLogsMaxSize,
			MaxFiles: defaultLogsMaxFiles,
		}
	}

	out := *s

	if out.MaxSize == "" {
		out.MaxSize = defaultLogsMaxSize
	}

	if out.MaxFiles <= 0 {
		out.MaxFiles = defaultLogsMaxFiles
	}

	return &out
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

	// Build holds the build-mode configuration when Image is empty.
	// See DeploymentSpec.Build / BuildSpec for the full contract —
	// shape and mutual-exclusivity are identical across kinds.
	Build *BuildSpec `yaml:"build,omitempty" json:"build,omitempty"`

	// ExtraHosts, CapAdd, EnvFile mirror the same fields on
	// DeploymentSpec — see DeploymentSpec for full docs. Behaviour is
	// identical; ordinal-stable pods don't change semantics for any of
	// these knobs. env_file values merge into Env at apply time with
	// inline-wins precedence.
	ExtraHosts []string `yaml:"extra_hosts,omitempty" json:"extra_hosts,omitempty"`
	CapAdd     []string `yaml:"cap_add,omitempty"     json:"cap_add,omitempty"`
	EnvFile    []string `yaml:"env_file,omitempty"    json:"env_file,omitempty"`

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

	// Logs caps the docker json-file log driver per ordinal — see
	// DeploymentSpec.Logs for the operator-facing contract. The
	// platform's 10m / 3-files default applies when omitted.
	Logs *LogsSpec `yaml:"logs,omitempty" json:"logs,omitempty"`

	// InitContainers mirrors DeploymentSpec.InitContainers — ordered
	// one-shot prep steps that must complete before the pod's main
	// container starts. For statefulsets the inits run per-ORDINAL
	// (each pod-N spawn re-runs every init against that pod's
	// volumes / env / aliases). See InitContainerSpec for the
	// per-step shape and inheritance rules.
	InitContainers []InitContainerSpec `yaml:"inits,omitempty" json:"inits,omitempty"`

	// Probes mirrors DeploymentSpec.Probes — kubelet-style
	// liveness / readiness / startup health checks, applied PER
	// ORDINAL. Each pod-N gets its own runner instances; failure
	// of pod-N's liveness restarts pod-N alone (data-preserving
	// docker restart on the same ordinal, same volumes). Useful
	// for stateful workloads where pg_isready / redis-cli ping
	// are the canonical "alive" signals.
	Probes *ProbesSpec `yaml:"probes,omitempty" json:"probes,omitempty"`

	// AssetDigests is server-stamped — see DeploymentSpec.AssetDigests.
	AssetDigests map[string]string `yaml:"-" json:"_asset_digests,omitempty"`
}

// applyDefaults fills implicit values for the build-mode case (mirror
// of DeploymentSpec.applyDefaults). Image-mode statefulsets skip this
// because Build is meaningless when no build runs.
//
// No HealthCheck default: statefulset workloads (postgres, redis,
// kafka) don't have a universal HTTP probe path the way HTTP apps do —
// the operator picks `pg_isready`, `redis-cli ping`, etc.
func (s *StatefulsetSpec) applyDefaults() {
	s.Logs = applyLogsDefaults(s.Logs)

	if s.Image != "" {
		return
	}

	if s.Build == nil {
		s.Build = &BuildSpec{}
	}

	if s.Build.Context == "" {
		s.Build.Context = "."
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
	Command     []string          `yaml:"command,omitempty"       json:"command,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"           json:"env,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"       json:"volumes,omitempty"`
	Network     string            `yaml:"network,omitempty"       json:"network,omitempty"`
	Networks    []string          `yaml:"networks,omitempty"      json:"networks,omitempty"`
	NetworkMode string            `yaml:"network_mode,omitempty"  json:"network_mode,omitempty"`

	// Build holds the build-mode configuration when Image is empty.
	// Jobs typically reuse a deployment's image via `image = "my-app:
	// latest"`, so the build block is rarely needed in practice — but
	// the surface is symmetric with deployment/statefulset for the
	// occasional one-off job that builds its own image.
	Build *BuildSpec `yaml:"build,omitempty" json:"build,omitempty"`

	// ExtraHosts, CapAdd, EnvFile mirror the same fields on
	// DeploymentSpec — same semantics. EnvFile is merged into Env
	// client-side at apply time; inline `env = {...}` wins on key
	// collisions.
	ExtraHosts []string `yaml:"extra_hosts,omitempty" json:"extra_hosts,omitempty"`
	CapAdd     []string `yaml:"cap_add,omitempty"     json:"cap_add,omitempty"`
	EnvFile    []string `yaml:"env_file,omitempty"    json:"env_file,omitempty"`

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

	// Logs caps the docker json-file log driver for the run
	// container — see DeploymentSpec.Logs for the operator-facing
	// contract. Inherited by cronjob via the embedded JobSpec, so
	// scheduled ticks get the same cap as standalone runs.
	Logs *LogsSpec `yaml:"logs,omitempty" json:"logs,omitempty"`

	// AssetDigests is server-stamped — see DeploymentSpec.AssetDigests.
	AssetDigests map[string]string `yaml:"-" json:"_asset_digests,omitempty"`
}

// applyDefaults fills the per-run platform defaults that aren't
// driver-level overrides (today: the logs cap). Image/build defaults
// are NOT synthesised here — jobs always require an explicit image
// (or build block), and the parser enforces that contract before we
// reach applyDefaults.
func (s *JobSpec) applyDefaults() {
	s.Logs = applyLogsDefaults(s.Logs)
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

// RegistrySpec is the wire shape for a private-registry pull secret.
// Each block declares one entry under `auths` in the host's docker
// config; the controller atomically regenerates ~/.docker/config.json
// whenever a registry manifest is created, updated, or deleted, so
// any subsequent `docker pull` (deployment image fetch, build cache,
// CI run) authenticates without a manual `docker login` on the box.
//
//	registry "ghcr" {
//	  url      = "ghcr.io"
//	  username = "${GHCR_USER}"
//	  token    = "${GHCR_TOKEN}"
//	}
//
// All three fields are required at parse time — a half-configured
// registry block would emit an `auths` entry that breaks docker's
// credential helper code path more often than it helps, so reject
// loudly instead of silently shipping a busted config.
//
// `password` is accepted as an alias of `token` in the HCL surface
// (some upstream registries — Docker Hub, generic SSO-fronted
// instances — use one term; others — GHCR, GitLab — use the other).
// Both decode into the same `Token` field on the wire spec; the
// HCL surface chooses which keyword reads more naturally to the
// operator and stays out of the way of muscle memory.
//
// Identity is singleton per host: the block's label IS the registry
// name, no scope segment. Two scopes can't both declare
// `registry "ghcr"` because `~/.docker/config.json` is global state
// — uniqueness is enforced at /apply via the standard
// (kind, scope, name) duplicate-detection sweep on parse output.
type RegistrySpec struct {
	// URL is the registry hostname as docker sees it on the wire —
	// `ghcr.io`, `registry-1.docker.io`, `index.docker.io`,
	// `gitlab.example.com:5050`. Whatever the operator would put
	// after `docker login` on the CLI lands here verbatim.
	//
	// No scheme (`https://`) — docker config.json stores the bare
	// host (or host:port). The controller doesn't normalise input;
	// operators using a non-standard form get back exactly what
	// they wrote so the resulting config.json matches the manual
	// `docker login` shape they may already have habits around.
	URL string `yaml:"url" json:"url"`

	// Username is the registry account name. Forwarded as-is into
	// the base64-encoded `auth` field of the docker config entry.
	Username string `yaml:"username" json:"username"`

	// Token is the secret half of the basic auth pair. Named
	// `token` because the overwhelmingly common case in 2026 is
	// a Personal Access Token (GHCR, GitLab, Quay) or a registry-
	// scoped service-account credential, not a literal password.
	// The HCL parser ALSO accepts `password = "..."` as an alias
	// and decodes into this same field — see RegistrySpec doc.
	Token string `yaml:"token" json:"token"`
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
