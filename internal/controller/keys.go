package controller

import (
	"fmt"
	"strings"
)

// KV layout in etcd.
//
//	/desired/<kind>s/<scope>/<name>  # scoped kinds (deployment, ingress)
//	/desired/<kind>s/<name>          # unscoped kinds (database)
//	/actual/nodes/<node>/health      # health beacons per node
//	/actual/nodes/<node>/containers/<id>
//	/config/<scope>/_/<KEY>          # scope-level config (shared across resources in scope)
//	/config/<scope>/<name>/<KEY>     # app-level config (overrides scope on conflict)
//	/plugins/<name>/manifest
//	/status/<kind>s/<name>           # plugin-produced status (always keyed by
//	                                 # name; uniqueness of (kind, name) across
//	                                 # scopes is enforced at the /apply layer)
const (
	prefixDesired = "/desired/"
	prefixActual  = "/actual/"
	prefixConfig  = "/config/"
	prefixPlugins = "/plugins/"
	prefixStatus  = "/status/"
)

// Kind is the type of a declared resource. New kinds added in later
// milestones (e.g. "certificate") append here.
type Kind string

const (
	KindDeployment Kind = "deployment"
	KindDatabase   Kind = "database"
	KindIngress    Kind = "ingress"
	KindJob        Kind = "job"
	KindCronJob    Kind = "cronjob"
)

var validKinds = map[Kind]bool{
	KindDeployment: true,
	KindDatabase:   true,
	KindIngress:    true,
	KindJob:        true,
	KindCronJob:    true,
}

// ParseKind returns the canonical Kind for either the singular or plural
// form (deployment / deployments). Unknown kinds return an error.
//
// Singular is tried first so kinds whose name ends in "s" (ingress) are
// not mangled into "ingres" by an unconditional trailing-s strip.
func ParseKind(s string) (Kind, error) {
	k := Kind(strings.ToLower(s))

	if validKinds[k] {
		return k, nil
	}

	if trimmed := Kind(strings.TrimSuffix(string(k), "s")); validKinds[trimmed] {
		return trimmed, nil
	}

	return "", fmt.Errorf("unknown kind %q (valid: deployment, database, ingress, job, cronjob)", s)
}

// DesiredPrefix returns "/desired/<kind>s/" — the prefix covering every
// manifest of a kind across all scopes.
func DesiredPrefix(kind Kind) string {
	return prefixDesired + string(kind) + "s/"
}

// ScopedPrefix returns "/desired/<kind>s/<scope>/" — used to list a
// single (kind, scope) bucket when computing a prune diff.
func ScopedPrefix(kind Kind, scope string) string {
	return DesiredPrefix(kind) + scope + "/"
}

// DesiredKey returns the etcd key for a manifest. Scoped kinds get the
// extra path segment; unscoped kinds keep the original flat layout.
func DesiredKey(kind Kind, scope, name string) string {
	if IsScoped(kind) {
		return ScopedPrefix(kind, scope) + name
	}

	return DesiredPrefix(kind) + name
}

// AllDesiredPrefix returns "/desired/" — the root watch key.
func AllDesiredPrefix() string { return prefixDesired }

// NodeHealthKey returns "/actual/nodes/<node>/health".
func NodeHealthKey(node string) string {
	return prefixActual + "nodes/" + node + "/health"
}

// NodeContainerKey returns "/actual/nodes/<node>/containers/<id>".
func NodeContainerKey(node, id string) string {
	return prefixActual + "nodes/" + node + "/containers/" + id
}

// PluginManifestKey returns "/plugins/<name>/manifest".
func PluginManifestKey(name string) string {
	return prefixPlugins + name + "/manifest"
}

// PluginsPrefix returns "/plugins/" for listing.
func PluginsPrefix() string { return prefixPlugins }

// StatusKey returns "/status/<kind>s/<name>" — where the reconciler stores
// the runtime data a plugin returned (credentials, URLs, container ids).
// Kept separate from /desired/ so re-applying a manifest doesn't clobber
// state the plugin generated.
func StatusKey(kind Kind, name string) string {
	return prefixStatus + string(kind) + "s/" + name
}

// Config keys.
//
// scope-level config (shared across all resources in a scope) lives
// under "/config/<scope>/_/<KEY>" — the literal "_" is reserved as
// the synthetic "all apps" name to keep prefix listing trivial.
// app-level config lives under "/config/<scope>/<name>/<KEY>" and
// overrides scope-level on conflict at injection time.
//
// Why one tree per (scope, name) bucket:
//   - prefix listing is O(N) on the bucket, not on the whole config
//     space, so `vd config list -s clowk-lp -n web` is fast even
//     when there are many other apps
//   - the watch on a single bucket triggers exactly one reconcile per
//     mutated app, no fan-out filtering on the reconciler side
const configScopeAllName = "_"

// ConfigKey returns the etcd key for one config entry. scope is
// required; when name is empty the entry is scope-level (shared
// across resources in scope). KEY is the env-var name uppercased on
// the way in (the resolver case-folds anyway, but we normalise so
// the listing is deterministic).
func ConfigKey(scope, name, key string) string {
	return ConfigPrefix(scope, name) + key
}

// ConfigPrefix returns the prefix that covers every key under one
// (scope, name) bucket. Scope-only configs live under the synthetic
// "_" name segment so the prefix is well-formed even without an app.
func ConfigPrefix(scope, name string) string {
	if name == "" {
		name = configScopeAllName
	}

	return prefixConfig + scope + "/" + name + "/"
}

// ConfigScopeRoot returns "/config/<scope>/" — the prefix covering
// every config bucket inside a scope, used by handlers that want to
// resolve "all config relevant to this resource" (scope-level + the
// resource's own name-level).
func ConfigScopeRoot(scope string) string {
	return prefixConfig + scope + "/"
}
