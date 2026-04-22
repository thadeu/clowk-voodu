// Package plugin is the public contract for Voodu plugins.
//
// # Plugin layout on disk
//
// A plugin is a directory under /opt/voodu/plugins/<name>/. It mirrors
// the Gokku layout so existing Gokku plugins port without changes:
//
//	<plugin>/
//	  plugin.yml             (optional metadata; falls back to convention)
//	  install                (optional lifecycle hook, run at plugins:install)
//	  uninstall              (optional lifecycle hook, run at plugins:remove)
//	  commands/
//	    name                 (required: echoes the plugin's CLI namespace)
//	    help                 (required: multi-line help text)
//	    info                 (recommended)
//	    logs                 (recommended)
//	    <custom...>          (any subcommand — `voodu <plugin> <custom>`)
//	  bin/
//	    <custom...>          (same as commands/, preferred when both exist)
//	  hooks/
//	    <event>              (optional — fired by controller on matching event)
//
// bin/ is checked before commands/ so plugin authors can ship compiled
// Go binaries without changing the layout a Gokku user already knows.
// Shell scripts and Go binaries are both valid — Voodu does not care.
//
// # plugin.yml (optional)
//
// When present, plugin.yml replaces conventions with declared metadata.
// Everything in it is metadata for the CLI (plugins:list, help text) —
// execution still resolves commands by path. The file is opt-in.
//
//	name: postgres
//	version: 0.1.0
//	description: Manage Postgres database instances
//	homepage: https://github.com/thadeu/voodu-postgres
//	commands:
//	  - name: create
//	    help: Provision a new Postgres instance
//	  - name: list
//	    help: List managed Postgres instances
//	env:                            # passed to every exec
//	  POSTGRES_DEFAULT_VERSION: "16"
//
// # Execution contract
//
// When a user types `voodu postgres create main`, the CLI forwards to
// the controller's /exec endpoint. The controller:
//
//  1. Looks up /plugins/postgres/manifest in etcd to find the plugin dir
//  2. Resolves the command (bin/create or commands/create)
//  3. Invokes the executable with the remaining args: ["main"]
//  4. Injects these environment variables:
//     VOODU_ROOT          — /opt/voodu (installation root)
//     VOODU_NODE          — controller node name
//     VOODU_ETCD_CLIENT   — etcd client URL
//     VOODU_PLUGIN_DIR    — absolute plugin directory
//     VOODU_APP           — app name, if --app/-a was passed
//     (any env declared in plugin.yml)
//  5. Captures stdout + exit code
//
// Plugins may output either:
//
//   - Plain text (backward-compatible with Gokku plugins). The controller
//     proxies stdout verbatim to the CLI, which renders it unmodified.
//
//   - A JSON envelope matching the Envelope type below. When the first
//     non-whitespace byte is '{', the controller parses the envelope and
//     the CLI can render with -o text|json|yaml.
//
// Exit code 0 means success regardless of which protocol the plugin used.
// Non-zero is reported to the CLI as an error.
package plugin

// Envelope is the JSON output protocol. Optional — plugins that emit
// plain text still work.
//
// Status must be "ok" or "error". On "error", Error should be a short
// human-readable sentence explaining the failure.
type Envelope struct {
	Status string `json:"status"`
	Data   any    `json:"data,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Manifest is the parsed shape of plugin.yml. All fields are optional;
// missing values are filled from directory conventions at load time.
type Manifest struct {
	Name        string            `yaml:"name"              json:"name"`
	Version     string            `yaml:"version,omitempty" json:"version,omitempty"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	Homepage    string            `yaml:"homepage,omitempty"    json:"homepage,omitempty"`
	Commands    []Command         `yaml:"commands,omitempty"    json:"commands,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"         json:"env,omitempty"`
	Source      string            `yaml:"-"                     json:"source,omitempty"`
}

// Command documents one plugin subcommand for `plugins:list` and help
// output. Execution does not depend on this — commands are resolved by
// looking for an executable under bin/ or commands/.
type Command struct {
	Name string `yaml:"name"             json:"name"`
	Help string `yaml:"help,omitempty"   json:"help,omitempty"`
}

// EnvVar keys that the controller injects on every plugin invocation.
// Exported so plugin authors using this SDK can reference them without
// stringly-typed lookups.
const (
	EnvRoot       = "VOODU_ROOT"
	EnvNode       = "VOODU_NODE"
	EnvEtcdClient = "VOODU_ETCD_CLIENT"
	EnvPluginDir  = "VOODU_PLUGIN_DIR"
	EnvApp        = "VOODU_APP"

	// Database-kind env — set by the reconciler when calling
	// <engine>:create and <engine>:destroy for a declared database.
	// Plugins that only implement user-facing subcommands can ignore
	// these; database-flavored plugins use them instead of parsing args.
	EnvDBName    = "VOODU_DB_NAME"
	EnvDBEngine  = "VOODU_DB_ENGINE"
	EnvDBVersion = "VOODU_DB_VERSION"
	EnvDBStorage = "VOODU_DB_STORAGE"
	EnvDBParams  = "VOODU_DB_PARAMS" // JSON-encoded map[string]string

	// Ingress-kind env — set by the reconciler when calling
	// <ingress-plugin>:apply and :remove for a declared ingress.
	EnvIngressHost        = "VOODU_INGRESS_HOST"
	EnvIngressService     = "VOODU_INGRESS_SERVICE"
	EnvIngressPort        = "VOODU_INGRESS_PORT"
	EnvIngressTLS         = "VOODU_INGRESS_TLS"
	EnvIngressTLSProvider = "VOODU_INGRESS_TLS_PROVIDER"
	EnvIngressTLSEmail    = "VOODU_INGRESS_TLS_EMAIL"
)

// Database-kind plugin contract.
//
// A plugin that wants to materialise `database "<name>" { engine =
// "<engine>" ... }` manifests must expose two commands:
//
//	create    Provision a new instance named $VOODU_DB_NAME.
//	          Must be idempotent if called twice before status is
//	          persisted (rare, but happens on crash-during-reconcile).
//	          Returns a JSON envelope:
//	              {"status":"ok","data":{
//	                "url":"...", "host":"...", "port":"5432",
//	                "username":"...", "password":"...", "database":"..."
//	              }}
//	          Fields listed above are conventional, not mandatory —
//	          ${ref.database.NAME.FIELD} resolves against whatever keys
//	          the plugin emits.
//
//	destroy   Tear down the instance named $VOODU_DB_NAME. Safe to call
//	          on a non-existent instance (the reconciler invokes it
//	          unconditionally when a manifest is deleted).
//
// Plugins that only offer CLI subcommands (postgres:backup, etc.) don't
// need create/destroy. The reconciler only invokes them when a database
// manifest references the plugin's engine.

// Ingress-kind plugin contract.
//
// A plugin acting as the host router (the reference implementation is
// voodu-caddy) must expose:
//
//	apply     Configure the router so $VOODU_INGRESS_HOST routes to
//	          $VOODU_INGRESS_SERVICE. Must be idempotent — the
//	          reconciler re-invokes on every manifest re-apply and on
//	          every startup replay.
//	          When TLS is requested ($VOODU_INGRESS_TLS=true), the
//	          plugin is responsible for certificate acquisition
//	          (provider/email env vars supply the knobs).
//	          Returns a JSON envelope with whatever fields are useful
//	          upstream — e.g. {"status":"ok","data":{"url":"https://..."}}
//
//	remove    Tear down the ingress named $VOODU_APP. Safe to call on a
//	          non-existent one.
//
// The plugin name is configurable (IngressHandler.PluginName) so a host
// can swap Caddy for nginx/traefik by installing a different plugin and
// setting the controller flag.
