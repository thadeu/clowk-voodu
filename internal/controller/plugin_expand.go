package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"go.voodu.clowk.in/internal/plugins"
	"go.voodu.clowk.in/pkg/plugin"
)

// expandRequest is the JSON payload piped to a plugin's `expand`
// command via stdin. Plugins consume it to compose the
// statefulset (or other core kind) manifest the controller will
// persist on their behalf. Mirroring `controller.Manifest` keeps
// the contract small — plugin authors don't have to learn a
// fourth schema.
//
//	{
//	  "kind":   "postgres",
//	  "scope":  "data",
//	  "name":   "main",
//	  "spec":   { …attrs from the HCL block… }
//	}
//
// Plugins write a single Manifest (or an array, when one block
// fans out into multiple core kinds — e.g. a future `postgres-
// cluster` that wants both a statefulset and a separate
// monitoring sidecar) to stdout, wrapped in a plugin envelope.
type expandRequest struct {
	Kind  string          `json:"kind"`
	Scope string          `json:"scope,omitempty"`
	Name  string          `json:"name"`
	Spec  json.RawMessage `json:"spec,omitempty"`

	// Config is the merged config bucket (scope-level + app-level)
	// for (scope, name) at the time of expand. Stateful plugins
	// read this to detect "first apply vs re-apply" and act
	// idempotently — e.g. redis reads REDIS_PASSWORD: present →
	// reuses it, absent → generates one + emits a config_set
	// action so the next apply finds the same value. Non-stateful
	// plugins ignore the field. Empty map when the bucket has no
	// keys (or store fetch failed; in that case the plugin sees
	// "first apply" and may regenerate state — operator
	// observable, not silent).
	Config map[string]string `json:"config,omitempty"`
}

// expandResponseData is the shape the plugin's envelope.Data must
// match: a single object OR an array of objects, each one a
// Manifest the controller will persist as if the operator had
// written it directly. Anything else → "plugin returned malformed
// expand output" with the raw stdout for debugging.
//
// The plugin owns whatever transformation it wants — voodu-
// postgres turns `postgres { replicas = 2 }` into a
// `statefulset` with a volume_claim, env, image, etc. The
// controller doesn't peek inside; it just trusts the plugin's
// output to be valid manifests, validates them through the same
// path direct /apply takes, and persists.

// PluginExpansion records that a plugin block was expanded into
// one or more core manifests during /apply. Surface in the
// response so operators see the lineage in `vd apply` output:
//
//	expanded postgres/data/main → statefulset/data/main
//
// without losing track of which macro produced the running
// resource. M-D3 polish — no functional impact, just visibility.
type PluginExpansion struct {
	From string   `json:"from"`
	To   []string `json:"to"`
}

// PluginInstallation records that a plugin was JIT-installed
// during the apply (operator didn't pre-install via `vd
// plugins:install`). Surface in the response so the operator
// sees the version they got, sourced from where, and can pin it
// later if they want reproducibility:
//
//	installed plugin postgres v0.2.0 from thadeu/voodu-postgres
type PluginInstallation struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
}

// expandPluginBlocks routes non-core manifests through their
// plugin's `expand` command, splicing the returned core
// manifests into the list. Core manifests pass through untouched.
//
// On a missing plugin, JIT install kicks in: the server attempts
// to fetch `thadeu/voodu-<kind>` (or whatever `_repo`
// override the block declared) via the configured Installer.
// Operators get plugins on first apply without a separate
// `vd plugins:install` step. JIT failures surface as the same
// "no plugin" error operators get when no installer is wired.
//
// The order of the input list is preserved as much as possible:
// when one plugin block expands to several manifests, those
// retain the relative position of the original block.
//
// Returns the expanded manifest list plus tracking metadata —
// per-block expansion and per-plugin installation events —
// for the apply response to surface in CLI output.
func (a *API) expandPluginBlocks(ctx context.Context, mans []*Manifest) (out []*Manifest, expansions []PluginExpansion, installs []PluginInstallation, err error) {
	out = make([]*Manifest, 0, len(mans))

	for _, m := range mans {
		if m == nil {
			continue
		}

		if IsCoreKind(m.Kind) {
			out = append(out, m)
			continue
		}

		if a.PluginBlocks == nil {
			return nil, nil, nil, fmt.Errorf("kind %q is not a core kind and the plugin registry is not configured (server bug — production wires DirPluginRegistry)", m.Kind)
		}

		// Strip the installer-only `plugin { … }` nested
		// block from the spec BEFORE the plugin's expand
		// sees it — the block carries metadata (version
		// pin, repo override) reserved by the controller.
		repo, version, cleanedSpec := extractInstallMetadata(m.Spec)
		m.Spec = cleanedSpec

		plug, ok := a.PluginBlocks.LookupByBlock(string(m.Kind))

		// "latest" is the operator opt-in for "always refresh
		// from the plugin repo's default branch". Treat it as
		// no-version on the wire to the installer (no
		// --branch flag → default branch checkout) but force
		// re-install on every apply.
		isLatest := version == "latest"

		installVersion := version
		if isLatest {
			installVersion = ""
		}

		// JIT install when:
		//   - plugin missing entirely (first apply), OR
		//   - operator pinned a specific `plugin { version }`
		//     and the installed plugin differs (manifest drift), OR
		//   - operator declared `version = "latest"` — opt-in
		//     to always-refresh per apply.
		// Plain plugin-installed-without-pin uses what's on
		// disk; `vd plugins:upgrade` (or pinning) is the way
		// to bump deterministically.
		needsInstall := !ok ||
			(!isLatest && version != "" && plug != nil && plug.Manifest.Version != version) ||
			isLatest

		if needsInstall {
			installed, err := a.installPluginForBlock(ctx, string(m.Kind), repo, installVersion)
			if err != nil {
				return nil, nil, nil, err
			}

			plug = installed

			installs = append(installs, PluginInstallation{
				Plugin:  installed.Manifest.Name,
				Version: installed.Manifest.Version,
				Source:  installed.Manifest.Source,
			})
		}

		expanded, runErr := a.runExpand(ctx, plug, m)
		if runErr != nil {
			return nil, nil, nil, fmt.Errorf("expand %s/%s: %w", m.Kind, m.Name, runErr)
		}

		// Lineage record: from the macro block to the core
		// manifests it produced. Pretty-print refs for the
		// CLI to render directly.
		exp := PluginExpansion{From: refOf(m.Kind, m.Scope, m.Name)}

		for _, e := range expanded {
			exp.To = append(exp.To, refOf(e.Kind, e.Scope, e.Name))
		}

		expansions = append(expansions, exp)

		out = append(out, expanded...)
	}

	return out, expansions, installs, nil
}

// refOf renders a (kind, scope, name) tuple in the standard
// `kind/scope/name` form (or `kind/name` when unscoped) — the
// shape `vd apply` already prints per resource. Used by
// expansion lineage so renderers can drop these strings into
// log lines verbatim.
func refOf(kind Kind, scope, name string) string {
	if scope == "" {
		return string(kind) + "/" + name
	}

	return string(kind) + "/" + scope + "/" + name
}

// installPluginForBlock fetches the plugin that handles
// `blockType` from `repo` (with optional `version` pin) and
// validates it's a macro plugin (exposes an `expand` command
// and self-identifies under the expected name). On success the
// plugin is now resolvable through PluginBlocks.LookupByBlock —
// production registries load fresh on each call, so no cache
// invalidation needed.
//
// Errors describe the failure mode operators care about:
// installer not configured (server flag), repo / tag
// unreachable (network or missing tag), plugin name mismatch
// (wrong repo), version pin mismatch (operator pinned X but
// the plugin.yml at that tag declares Y), missing expand
// (plugin is not a macro). Each lands the operator one step
// closer to the fix.
func (a *API) installPluginForBlock(ctx context.Context, blockType, repo, version string) (*plugins.LoadedPlugin, error) {
	if a.PluginInstaller == nil {
		return nil, fmt.Errorf("kind %q has no installed plugin and JIT install is not configured (try `vd plugins:install %s`)", blockType, blockType)
	}

	if repo == "" {
		repo = defaultPluginRepo(blockType)
	}

	loaded, err := a.PluginInstaller.Install(ctx, repo, version)
	if err != nil {
		return nil, fmt.Errorf("auto-install plugin %q from %q: %w (try `vd plugins:install %s` manually)", blockType, repo, err, blockType)
	}

	if loaded.Manifest.Name != blockType {
		return nil, fmt.Errorf("auto-install plugin %q from %q: repo declares plugin name %q (mismatch — wrong repo or block name)", blockType, repo, loaded.Manifest.Name)
	}

	if _, ok := loaded.Commands["expand"]; !ok {
		return nil, fmt.Errorf("auto-install plugin %q from %q: plugin has no `expand` command (not a macro plugin)", blockType, repo)
	}

	if version != "" && loaded.Manifest.Version != version {
		return nil, fmt.Errorf("auto-install plugin %q from %q at v%s: repo declares plugin.yml version %q (tag and plugin.yml drifted — open issue on the plugin repo)",
			blockType, repo, version, loaded.Manifest.Version)
	}

	return loaded, nil
}

// defaultPluginRepo applies the `thadeu/voodu-<kind>` convention.
// Operators that want a different default per-server can set the
// VOODU_PLUGIN_DEFAULT_REPO_<KIND> env var (uppercased), and
// individual blocks override via `repo = "..."` inside the
// `plugin { ... }` nested block.
func defaultPluginRepo(blockType string) string {
	if v := os.Getenv("VOODU_PLUGIN_DEFAULT_REPO_" + strings.ToUpper(blockType)); v != "" {
		return v
	}

	return "thadeu/voodu-" + blockType
}


// blockCoreAttributes is the catalogue of nested-block names
// the controller reserves at the plugin-block level. The
// reserved blocks carry installer metadata (which version of
// the plugin to fetch, where to fetch from, etc.) and are
// stripped from the spec BEFORE the plugin's `expand` command
// sees it — plugin authors never see them, and operators must
// not name a nested spec block after one of these (the value
// would never reach the plugin).
//
// Adding a new block-level metadata block (e.g. `auth { … }`
// for repository credentials, `lock { … }` for pinning modes)
// is a single-line addition here plus wiring in
// extractInstallMetadata.
var blockCoreAttributes = map[string]bool{
	"plugin": true,
}

// IsReservedBlockAttribute reports whether `name` is a
// controller-reserved nested-block name on plugin blocks.
// Exposed so the CLI / docs / future linters can validate
// manifests against the same source of truth.
func IsReservedBlockAttribute(name string) bool {
	return blockCoreAttributes[name]
}

// extractInstallMetadata pulls the installer-only nested
// `plugin { … }` block out of a plugin spec and returns its
// fields alongside a cleaned spec the plugin's `expand`
// command receives. Operators write:
//
//	postgres "data" "main" {
//	  plugin {
//	    version = "0.3.1"                       # optional, pin git tag
//	    repo    = "myorg/voodu-postgres-fork"   # optional, override default repo
//	  }
//
//	  image = "postgres:15-alpine"
//	  …
//	}
//
// The nested block separates installer metadata (which
// version, where from) from the spec attributes the plugin
// receives — there's no risk of operator-supplied `version`
// or `repo` colliding with future spec fields, and the
// HCL reads more naturally (matches `tls { … }`, `release { … }`,
// `volume_claim "…" { … }` shapes).
//
// `version` semantics: pins the plugin's git tag (leading
// `v` auto-prefixed by the installer when missing). If
// declared and the installed plugin has a different
// version, the controller re-installs at the pinned tag —
// version moves forward AND backward declaratively.
//
// `version = "latest"` is the explicit opt-in for "always
// refresh from the default branch on every apply". Useful
// during plugin development / solo dev. Forces a re-install
// even when the plugin is already on disk, picking up
// whatever the maintainer just published. Pin a real tag
// for prod determinism.
//
// Block omitted entirely (or `version` left empty): the
// currently-installed plugin is used as-is, no network
// roundtrip, no surprise upgrades. `vd plugins:upgrade` is
// the way to bump explicitly when desired.
//
// `repo` overrides the default `thadeu/voodu-<kind>`
// convention. Useful for forks, internal mirrors, or
// air-gapped clones reachable via `https://gitea.internal/…`.
//
// On the wire the nested block surfaces in the spec JSON as
// a `plugin: { version, repo }` sub-object thanks to the
// parser's bodyToJSON nested-rollup. We pop it off here.
func extractInstallMetadata(spec json.RawMessage) (repo, version string, cleaned json.RawMessage) {
	if len(spec) == 0 {
		return "", "", spec
	}

	var raw map[string]any
	if err := json.Unmarshal(spec, &raw); err != nil {
		return "", "", spec
	}

	pluginBlock, ok := raw["plugin"].(map[string]any)
	if !ok {
		return "", "", spec
	}

	if v, ok := pluginBlock["repo"]; ok {
		if s, isString := v.(string); isString {
			repo = strings.TrimSpace(s)
		}
	}

	if v, ok := pluginBlock["version"]; ok {
		if s, isString := v.(string); isString {
			version = strings.TrimSpace(s)
		}
	}

	delete(raw, "plugin")

	out, err := json.Marshal(raw)
	if err != nil {
		return repo, version, spec
	}

	return repo, version, out
}

// runExpand invokes the plugin's `expand` command with the block
// JSON on stdin and parses its envelope. The plugin may emit
// either:
//
//   - A single manifest object (one-resource plugins)
//   - An array of manifests (multi-resource fan-out)
//   - An object {manifests: [...], actions: [...]} when the
//     plugin needs to mutate config alongside the manifests
//     (typical case: redis generates a password on first apply
//     and stores it in the config bucket via a config_set
//     action so subsequent applies are idempotent)
//
// Pre-fetches the merged config bucket for (scope, name) and
// passes it to the plugin in stdin. Lets stateful plugins read
// "have I been initialised before?" state without round-tripping
// through their own commands. Errors during ResolveConfig are
// non-fatal — the plugin gets an empty config map and decides
// whether the absence is meaningful.
func (a *API) runExpand(ctx context.Context, plug *plugins.LoadedPlugin, m *Manifest) ([]*Manifest, error) {
	var existingConfig map[string]string

	if a.Store != nil {
		existingConfig, _ = a.Store.ResolveConfig(ctx, m.Scope, m.Name)
	}

	req := expandRequest{
		Kind:   string(m.Kind),
		Scope:  m.Scope,
		Name:   m.Name,
		Spec:   m.Spec,
		Config: existingConfig,
	}

	stdin, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal expand request: %w", err)
	}

	// Inject the same env the rest of the plugin pipeline uses
	// (controller URL, node name, etcd) so the plugin can call
	// back into the controller if it ever needs to (it
	// shouldn't, but voodu-caddy did in M-6 and leaving the
	// affordance costs nothing).
	env := map[string]string{
		plugin.EnvRoot:       a.PluginsRoot,
		plugin.EnvNode:       a.NodeName,
		plugin.EnvEtcdClient: a.EtcdClient,
	}

	res, err := plug.Run(ctx, plugins.RunOptions{
		Command: "expand",
		Stdin:   stdin,
		Env:     env,
	})
	if err != nil {
		return nil, err
	}

	if res.ExitCode != 0 {
		detail := pluginErrorDetail(res)
		return nil, fmt.Errorf("plugin %q expand exited %d: %s", plug.Manifest.Name, res.ExitCode, detail)
	}

	if res.Envelope == nil {
		return nil, fmt.Errorf("plugin %q expand returned no JSON envelope (got: %s)",
			plug.Manifest.Name, truncateForLog(res.Raw, 200))
	}

	if res.Envelope.Status == "error" {
		return nil, fmt.Errorf("plugin %q expand: %s", plug.Manifest.Name, res.Envelope.Error)
	}

	manifests, actions, err := decodeExpandedPayload(res.Envelope.Data)
	if err != nil {
		return nil, fmt.Errorf("plugin %q expand: %w", plug.Manifest.Name, err)
	}

	// Apply expand-emitted actions before returning the manifests.
	// Today the only emitter is voodu-redis (config_set with the
	// freshly-generated REDIS_PASSWORD on first apply), but the
	// shape is generic so any future plugin can persist its
	// initialised state through the same channel. Errors here
	// abort the expand — partial-apply (manifest persisted but
	// action lost) would leave the bucket in an inconsistent
	// state operators couldn't recover from without manual
	// intervention.
	for i, action := range actions {
		if _, err := a.applyPluginDispatchAction(ctx, action); err != nil {
			return nil, fmt.Errorf("plugin %q expand action %d (%s): %w", plug.Manifest.Name, i, action.Type, err)
		}
	}

	for _, em := range manifests {
		if em == nil {
			return nil, fmt.Errorf("plugin %q expand: returned a nil manifest", plug.Manifest.Name)
		}

		// Plugins must produce CORE kinds — recursive expansion
		// (a plugin returning another plugin block) is not
		// supported. Forbidding it keeps the dependency graph
		// cycle-free and the behaviour predictable.
		if !IsCoreKind(em.Kind) {
			return nil, fmt.Errorf("plugin %q expand: returned non-core kind %q (plugins must expand to core kinds)", plug.Manifest.Name, em.Kind)
		}

		if em.Name == "" {
			return nil, fmt.Errorf("plugin %q expand: returned manifest with empty name", plug.Manifest.Name)
		}
	}

	return manifests, nil
}

// decodeExpandedPayload handles three shapes a plugin may emit
// from its `expand` command, in order of preference:
//
//   - {"manifests": [...], "actions": [...]}  — full envelope.
//     Plugin emits manifests AND wants the controller to apply
//     side-effect actions (config_set, config_unset). New shape;
//     opt-in for stateful plugins like voodu-redis.
//
//   - [...]   — an array of manifest objects. The classic shape
//                voodu-postgres-multi and any fan-out plugin uses.
//
//   - {...}   — a single manifest object. The simplest shape,
//                used by single-resource plugins like voodu-caddy
//                ingress (one ingress in, one ingress out).
//
// The shape is detected by inspecting the first non-whitespace
// byte of the re-marshalled JSON. `[` → array; `{` with a
// `manifests` key → full envelope; `{` without → single manifest.
//
// Returning (manifests, actions, err) keeps the caller's call
// site uniform regardless of which shape the plugin used.
func decodeExpandedPayload(data any) ([]*Manifest, []pluginDispatchAction, error) {
	if data == nil {
		return nil, nil, fmt.Errorf("envelope.data is empty")
	}

	// Re-marshal then re-unmarshal: the envelope's Data field is
	// `any`, but we want to decode straight into Manifest. A
	// round-trip through JSON is the simplest way to convert
	// (avoids reflection surgery, keeps tags consistent).
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, nil, fmt.Errorf("re-marshal envelope.data: %w", err)
	}

	trimmed := []byte(strings.TrimSpace(string(raw)))

	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("envelope.data is empty")
	}

	if trimmed[0] == '[' {
		var arr []*Manifest

		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, nil, fmt.Errorf("decode array of manifests: %w", err)
		}

		return arr, nil, nil
	}

	// Object shape — try the new envelope first ({manifests,
	// actions}); if it doesn't have a `manifests` key, fall back
	// to single-manifest decode.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return nil, nil, fmt.Errorf("decode envelope.data object: %w", err)
	}

	if _, hasManifests := probe["manifests"]; hasManifests {
		var combined struct {
			Manifests []*Manifest            `json:"manifests"`
			Actions   []pluginDispatchAction `json:"actions"`
		}

		if err := json.Unmarshal(trimmed, &combined); err != nil {
			return nil, nil, fmt.Errorf("decode {manifests, actions}: %w", err)
		}

		return combined.Manifests, combined.Actions, nil
	}

	var m Manifest
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, nil, fmt.Errorf("decode manifest: %w", err)
	}

	return []*Manifest{&m}, nil, nil
}

// truncateForLog clips raw plugin output for inclusion in error
// messages. A misbehaving plugin can dump megabytes of garbage
// to stdout; we just want enough context to debug.
func truncateForLog(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}

	return string(b[:max]) + "…"
}
