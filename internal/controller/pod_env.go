package controller

import (
	"strconv"

	"go.voodu.clowk.in/internal/containers"
)

// Reserved env keys voodu injects into every managed container.
// Plugin-authored entrypoints (redis, postgres) read these to pick
// a role at boot — e.g. ordinal-0 of a redis statefulset starts as
// master, the rest start as replicas pointed at `redis-0.<scope>.voodu`.
//
// Centralised so a typo in any handler doesn't silently lose a name.
const (
	EnvPodOrdinal = "VOODU_REPLICA_ORDINAL"
	EnvPodReplica = "VOODU_REPLICA_ID"
	EnvPodScope   = "VOODU_SCOPE"
	EnvPodName    = "VOODU_NAME"

	// EnvControllerURL exposes the controller's HTTP base URL to
	// every managed container. Plugin-authored entrypoint scripts
	// (sentinel's failover hook, future probes, etc.) call back
	// to /describe, /config, /plugin/<name>/<command> using this
	// without any operator boilerplate. Auto-injected by
	// BuildPlatformEnv when the controller knows its own URL —
	// empty string in tests / setups without HTTP plumbing leaves
	// the var unset, callers detect and skip the callback path.
	//
	// The address must be reachable from inside container network.
	// On a single-VM setup, host.docker.internal (Mac) or the
	// host gateway IP (Linux) typically work; on multi-VM, the
	// controller's private IP. The controller doesn't pick the
	// address — server wiring sets it from --controller-addr or
	// VOODU_CONTROLLER_URL at startup.
	EnvControllerURL = "VOODU_CONTROLLER_URL"
)

// BuildStatefulsetPodEnv emits the per-pod identity env for a
// statefulset replica. Includes the ordinal because statefulsets
// have stable identity by design — a replica at ordinal 0 is
// distinguishable from ordinal 1, and plugins rely on that
// (master at 0, replicas at 1..N).
func BuildStatefulsetPodEnv(scope, name string, ordinal int) map[string]string {
	if ordinal < 0 {
		ordinal = 0
	}

	return map[string]string{
		EnvPodScope:   scope,
		EnvPodName:    name,
		EnvPodOrdinal: strconv.Itoa(ordinal),
		EnvPodReplica: containers.OrdinalReplicaID(ordinal),
	}
}

// BuildDeploymentPodEnv emits the per-container identity env for
// a deployment replica. Deployments are stateless: replicas are
// interchangeable, so there's no ordinal — only scope+name. Apps
// can read VOODU_SCOPE/VOODU_NAME for self-identification (logs,
// metrics tags) without the operator having to thread these in
// through their own env file.
func BuildDeploymentPodEnv(scope, name string) map[string]string {
	return map[string]string{
		EnvPodScope: scope,
		EnvPodName:  name,
	}
}

// BuildPlatformEnv emits cluster-wide env every managed container
// gets — currently just VOODU_CONTROLLER_URL so in-container
// plugin scripts can reach back to the controller without operator
// gymnastics (no `env = { VOODU_CONTROLLER_URL = ... }` boilerplate).
//
// Empty controllerURL → nil map (caller's merge stays a no-op).
// This shape lets tests and bare setups skip the platform layer
// without paying for an empty merge.
//
// Why a separate function (vs cramming into BuildStatefulsetPodEnv):
// the platform env is INDEPENDENT of pod kind — deployments and
// statefulsets and (future) standalone pods all want it. Keeping
// it as its own function lets each handler do exactly one merge
// regardless of which other pod-kind env they stack.
func BuildPlatformEnv(controllerURL string) map[string]string {
	if controllerURL == "" {
		return nil
	}

	return map[string]string{
		EnvControllerURL: controllerURL,
	}
}

// MergePodEnv layers caller-supplied env on top of the platform
// defaults — last-wins, the operator's value beats the platform
// default on key collision. Mirrors docker's `-e` semantics
// (later -e flag wins) and the env file precedence operators
// already understand.
//
// Order on disk: platform first (low priority), extra later
// (high priority). When the operator deliberately sets
// VOODU_CONTROLLER_URL = "http://my-mock:8000" or VOODU_SCOPE
// to a logical alias, that override lands. Cross-tenant
// boundaries are NOT enforced by env (voodu's authorization
// uses manifest source + container labels), so an operator
// "spoofing" VOODU_SCOPE in their own pod just confuses their
// own application — no platform invariant is broken.
//
// nil-safe on both sides; returns nil only when both are nil.
//
// Returns a new map so the caller's input is left untouched.
func MergePodEnv(platform, extra map[string]string) map[string]string {
	if len(platform) == 0 && len(extra) == 0 {
		return nil
	}

	merged := make(map[string]string, len(platform)+len(extra))

	for k, v := range platform {
		merged[k] = v
	}

	for k, v := range extra {
		merged[k] = v
	}

	return merged
}
