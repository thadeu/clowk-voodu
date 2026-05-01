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

// MergePodEnv layers caller-supplied env on top of the platform
// identity env. The platform always wins on key collision — if an
// operator (or plugin) set VOODU_SCOPE in their HCL, the per-pod
// scope from the handler still lands. Returns a new map so the
// caller's input is left untouched.
//
// Order: extra is materialised first, then platform stamps on top.
// nil-safe on both sides; returns nil only when both are nil.
func MergePodEnv(platform, extra map[string]string) map[string]string {
	if len(platform) == 0 && len(extra) == 0 {
		return nil
	}

	merged := make(map[string]string, len(platform)+len(extra))

	for k, v := range extra {
		merged[k] = v
	}

	for k, v := range platform {
		merged[k] = v
	}

	return merged
}
