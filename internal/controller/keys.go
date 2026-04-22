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
func ParseKind(s string) (Kind, error) {
	k := Kind(strings.TrimSuffix(strings.ToLower(s), "s"))

	if !validKinds[k] {
		return "", fmt.Errorf("unknown kind %q (valid: deployment, database, service, ingress)", s)
	}

	return k, nil
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
