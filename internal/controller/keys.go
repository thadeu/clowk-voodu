package controller

import (
	"fmt"
	"strings"
)

// KV layout in etcd — must match PLAN.md.
//
//	/desired/<kind>s/<name>        # desired state for a resource
//	/actual/nodes/<node>/health    # health beacons per node
//	/actual/nodes/<node>/containers/<id>
//	/config/<app>/<key>            # per-app config (optional; CLI writes .env directly)
//	/plugins/<name>/manifest
const (
	prefixDesired = "/desired/"
	prefixActual  = "/actual/"
	prefixConfig  = "/config/"
	prefixPlugins = "/plugins/"
	prefixStatus  = "/status/"
)

// Kind is the type of a declared resource. New kinds added in later
// milestones (e.g. "certificate", "cronjob") append here.
type Kind string

const (
	KindDeployment Kind = "deployment"
	KindDatabase   Kind = "database"
	KindService    Kind = "service"
	KindIngress    Kind = "ingress"
)

var validKinds = map[Kind]bool{
	KindDeployment: true,
	KindDatabase:   true,
	KindService:    true,
	KindIngress:    true,
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

	return "", fmt.Errorf("unknown kind %q (valid: deployment, database, service, ingress)", s)
}

// DesiredPrefix returns "/desired/<kind>s/".
func DesiredPrefix(kind Kind) string {
	return prefixDesired + string(kind) + "s/"
}

// DesiredKey returns "/desired/<kind>s/<name>".
func DesiredKey(kind Kind, name string) string {
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
