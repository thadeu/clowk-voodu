// Package manifest parses Voodu resource manifests (HCL or YAML) into
// the on-the-wire controller.Manifest shape. The controller never sees
// HCL/YAML — it only stores the JSON produced here.
//
// Supported kinds: deployment, database, service, ingress. Each kind has
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
// means "build the repo root with ./Dockerfile, health-check /".
// Build-mode fields (Path/Dockerfile) only fire when Image is empty —
// registry-mode deployments should not carry build metadata they
// won't use. HealthCheck defaults in both modes because the ingress
// probe needs a path regardless of how the image was produced.
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

	if s.Dockerfile == "" {
		s.Dockerfile = "Dockerfile"
	}
}

// DatabaseSpec is a managed data service. The Engine field selects which
// plugin materialises the instance (M7 lands postgres, M9 lands mongo).
type DatabaseSpec struct {
	Engine  string            `yaml:"engine"            json:"engine"`
	Version string            `yaml:"version,omitempty" json:"version,omitempty"`
	Storage string            `yaml:"storage,omitempty" json:"storage,omitempty"`
	Backup  *DatabaseBackup   `yaml:"backup,omitempty"  json:"backup,omitempty"`
	Params  map[string]string `yaml:"params,omitempty"  json:"params,omitempty"`
}

type DatabaseBackup struct {
	Schedule  string `yaml:"schedule,omitempty"  json:"schedule,omitempty"`
	Retention string `yaml:"retention,omitempty" json:"retention,omitempty"`
	Target    string `yaml:"target,omitempty"    json:"target,omitempty"`
}

// ServiceSpec exposes a deployment (or external endpoint) under a stable
// name inside the cluster. Thin on purpose in M4 — plugin-facing contract
// will grow with M6 (ingress) and M7 (database bindings).
type ServiceSpec struct {
	Target string   `yaml:"target"         json:"target"`
	Port   int      `yaml:"port,omitempty" json:"port,omitempty"`
	Ports  []string `yaml:"ports,omitempty" json:"ports,omitempty"`
}

// IngressSpec describes an externally reachable hostname. M6's voodu-caddy
// plugin reconciles these into a running Caddy config. Route rewriting
// and advanced matchers are out of scope for M4 — the caddy plugin will
// grow that vocabulary.
//
// `service` is optional: when omitted, the controller defaults it to the
// ingress name (so `ingress "api" { host = "api.example.com" }` just
// works). Explicit `service = "..."` still wins — use it for cross-app
// routing (ingress "public" → service "api").
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

type IngressTLS struct {
	Enabled  bool   `yaml:"enabled,omitempty"  json:"enabled,omitempty"`
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Email    string `yaml:"email,omitempty"    json:"email,omitempty"`

	// On-demand TLS (wildcard multi-tenant). OnDemand turns on Caddy's
	// issue-cert-on-request behaviour; Ask is the gating callback URL.
	OnDemand bool   `yaml:"on_demand,omitempty" json:"on_demand,omitempty"`
	Ask      string `yaml:"ask,omitempty"       json:"ask,omitempty"`
}
