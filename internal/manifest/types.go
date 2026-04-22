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
// Minimal in M4 — the full lifecycle (health checks, replicas, volumes)
// fills out over M4/M5.
type DeploymentSpec struct {
	Image       string            `yaml:"image"                 json:"image"`
	Replicas    int               `yaml:"replicas,omitempty"    json:"replicas,omitempty"`
	Command     []string          `yaml:"command,omitempty"     json:"command,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"         json:"env,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"       json:"ports,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"     json:"volumes,omitempty"`
	Network     string            `yaml:"network,omitempty"     json:"network,omitempty"`
	Restart     string            `yaml:"restart,omitempty"     json:"restart,omitempty"`
	HealthCheck string            `yaml:"health_check,omitempty" json:"health_check,omitempty"`
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
type IngressSpec struct {
	Host    string      `yaml:"host"            json:"host"`
	Service string      `yaml:"service"         json:"service"`
	Port    int         `yaml:"port,omitempty"  json:"port,omitempty"`
	TLS     *IngressTLS `yaml:"tls,omitempty"   json:"tls,omitempty"`
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
