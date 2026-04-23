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
// Source resolution is intentionally implicit (mirrors legacy voodu.yml —
// no `source {}` block). The handler picks a mode from which fields are
// set at reconcile time:
//
//   - Image non-empty        → pull from registry and run (no build)
//   - Image empty + Path set → docker build using <Workdir>/<Path> as context
//   - Image empty + no Path  → docker build at repo root (same as `gokku deploy`)
//
// Workdir narrows the repo subtree (monorepo case); Dockerfile picks a
// non-default filename. Both are ignored when Image is set.
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
	Image       string            `yaml:"image,omitempty"        json:"image,omitempty"`
	Workdir     string            `yaml:"workdir,omitempty"      json:"workdir,omitempty"`
	Dockerfile  string            `yaml:"dockerfile,omitempty"   json:"dockerfile,omitempty"`
	Path        string            `yaml:"path,omitempty"         json:"path,omitempty"`
	Lang        string            `yaml:"lang,omitempty"         json:"lang,omitempty"`
	GoVersion   string            `yaml:"go_version,omitempty"   json:"go_version,omitempty"`
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
	PostDeploy  []string          `yaml:"post_deploy,omitempty"  json:"post_deploy,omitempty"`
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
