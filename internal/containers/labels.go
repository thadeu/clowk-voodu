package containers

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Voodu container label vocabulary.
//
// Every container the controller spawns carries this set so the runtime
// is queryable without parsing names. Two consumers depend on it:
//
//   - The reconciler — `ListByIdentity(scope, name, kind)` filters
//     existing containers by labels to decide what to add/remove. It
//     used to parse names like `<app>-<N>`; that path was brittle once
//     replicas became unordered (M0).
//
//   - `voodu get pods` — groups Docker containers by `voodu.scope`,
//     filters by `voodu.kind`, etc. The CLI never has to know about
//     container name conventions.
//
// Adding a new label is cheap (just a constant + a field on Identity).
// Renaming or removing one is a migration: in-flight containers carry
// the old key. The legacy detection path below is the example — it
// exists only because pre-M0 containers had no `voodu.*` labels at all.
const (
	// LabelCreatedBy is the umbrella filter. Every voodu artifact
	// carries this; ListContainers in internal/docker uses it to scope
	// docker ps output to voodu-managed containers without leaking
	// into the host's other workloads.
	LabelCreatedBy      = "createdby"
	LabelCreatedByValue = "voodu"

	// LabelKind = deployment | job | cronjob. Distinguishes container
	// lifecycle: deployments are long-running with replicas, jobs are
	// one-shot, cronjobs are scheduled jobs. `voodu get pods -k job`
	// filters on this.
	LabelKind = "voodu.kind"

	// LabelScope and LabelName are the controller's identity tuple.
	// (kind, scope, name) uniquely identifies a desired manifest;
	// listing containers by these labels is how the reconciler maps
	// the spec back to runtime instances.
	LabelScope = "voodu.scope"
	LabelName  = "voodu.name"

	// LabelReplicaID is a 4-char opaque hex assigned at container
	// creation. It is intentionally NOT ordered — replicas of a
	// deployment are interchangeable, and the suffix exists only to
	// disambiguate sibling containers in docker's flat namespace.
	// Pre-M0 voodu used a numeric `<app>-0`/`<app>-1` suffix that
	// suggested per-slot identity; the opaque hex makes it visually
	// clear that no such identity exists.
	LabelReplicaID = "voodu.replica_id"

	// LabelManifestHash mirrors the spec-content hash from the
	// controller's status store. When the desired hash drifts from
	// what the container was tagged with, the reconciler knows the
	// container is stale without re-reading etcd. Optional — handlers
	// that don't compute a hash leave it empty.
	LabelManifestHash = "voodu.manifest_hash"

	// LabelCreatedAt is RFC3339 of container creation. Lets `voodu
	// get pods` show an age column without an extra `docker inspect`
	// per container.
	LabelCreatedAt = "voodu.created_at"

	// LabelReleaseID stamps the deployment-release record this
	// container was spawned from. Set by paths that orchestrate a
	// release (Release / Rollback / runReleaseIfNeeded) and left
	// empty for paths that just scale (`vd apply` initial create
	// of a release-block deployment, non-release-block apply).
	// Empty is fine — `vd describe` renders a "-" and `vd release`
	// just shows zero pods for that record.
	LabelReleaseID = "voodu.release_id"

	// LabelReplicaOrdinal carries the stable integer index of a
	// statefulset replica (0, 1, 2, ...). Distinct from
	// LabelReplicaID — that label exists on every voodu container
	// and is opaque hex for deployments. Statefulsets reuse the
	// ordinal as the ReplicaID so ContainerName produces
	// `<scope>-<name>.0` instead of `<scope>-<name>.a3f9`, and
	// the dedicated ordinal label exists so `docker ps --filter
	// label=voodu.replica_ordinal=0` works without parsing
	// names. Empty for non-statefulset kinds.
	LabelReplicaOrdinal = "voodu.replica_ordinal"

	// LabelRole categorises the container at a higher level than
	// Kind — useful for grouping / filtering / display where the
	// raw kind is too granular. Default mirrors Kind ("deployment",
	// "statefulset", "job", "cronjob") but plugins or operators
	// override via spec.Labels: voodu-postgres backups emit
	// `voodu.role=backup` so `docker ps --filter
	// label=voodu.role=backup` filters across resources without
	// name parsing. The release pipeline emits
	// `voodu.role=release` for the same reason.
	//
	// `vd get pd` groups its output by this label so backup jobs
	// don't drown out the actual services in the listing.
	LabelRole = "voodu.role"
)

// Kind values used in LabelKind. Mirror controller.Kind constants —
// kept here as plain strings to avoid a circular import (containers
// is a leaf package; controller depends on it).
const (
	KindDeployment  = "deployment"
	KindStatefulset = "statefulset"
	KindJob         = "job"
	KindCronJob     = "cronjob"
)

// Identity is the structured form of voodu container labels. Two
// directions: BuildLabels turns this into `--label k=v` flags for
// `docker run`; ParseLabels recovers it from a docker label map.
type Identity struct {
	Kind         string
	Scope        string
	Name         string
	ReplicaID    string
	ManifestHash string
	CreatedAt    string

	// ReleaseID is the deployment-release record this container
	// belongs to. Optional — empty when the spawn path doesn't
	// run inside a release orchestrator (initial replica creation,
	// non-release-block deployments).
	ReleaseID string

	// ReplicaOrdinal is the integer index of this pod inside a
	// statefulset (0, 1, 2, …). Set only for Kind=statefulset
	// containers; deployments leave it as -1 (the "not set"
	// sentinel — distinct from a legitimate ordinal 0). Helpers
	// `OrdinalReplicaID` and `Identity.Ordinal()` translate
	// between the integer and the string form stored on
	// LabelReplicaOrdinal / reused as the ReplicaID.
	ReplicaOrdinal int

	// Role is the high-level category for grouping / display.
	// Empty defaults to Kind in BuildLabels (so every voodu
	// container ends up with a non-empty voodu.role label).
	// Specific paths override: release containers carry
	// `release`, backup capture jobs carry `backup`, etc.
	Role string
}

// NewReplicaID returns a 4-char hex string for use as a container
// suffix. 16 bits of entropy is plenty: uniqueness only matters
// among the active replicas of a single deployment (typically <10).
// The opacity is a feature — visually signals "interchangeable, not
// a slot number".
func NewReplicaID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}

// OrdinalReplicaID returns the canonical string form of a
// statefulset ordinal — just decimal digits ("0", "1", "12").
// Stored under both LabelReplicaID (so ContainerName produces
// `<app>.0` cleanly) and LabelReplicaOrdinal (for direct
// queryability via docker ps filters).
//
// Negative ordinals are clamped to 0; statefulsets index from
// zero by construction and a negative value would only land here
// from a programmer bug, where panicking would hide the cause.
func OrdinalReplicaID(n int) string {
	if n < 0 {
		n = 0
	}

	return strconv.Itoa(n)
}

// Ordinal recovers the integer ordinal from a statefulset
// container's labels. Returns (n, true) when Kind=statefulset
// and ReplicaID parses as a non-negative integer; (0, false)
// otherwise. The bool lets callers distinguish "ordinal 0"
// from "not a statefulset", which matters for the rolling
// restart path that iterates ordinals top-down.
func (id Identity) Ordinal() (int, bool) {
	if id.Kind != KindStatefulset {
		return 0, false
	}

	n, err := strconv.Atoi(id.ReplicaID)
	if err != nil || n < 0 {
		return 0, false
	}

	return n, true
}

// ContainerName composes a Docker container name from the (scope,
// name, replicaID) tuple.
//
//	scoped:   <scope>-<name>.<replicaID>     "softphone-web.a3f9"
//	unscoped: <name>.<replicaID>             "postgres.a3f9"
//
// The hyphen between scope and name preserves the AppID convention
// (controller.AppID returns "scope-name"). The dot before replicaID
// is the new separator — distinct from hyphen so callers can recover
// the boundary even if scope or name themselves contain hyphens.
//
// Job and cronjob runs use the same shape with replicaID standing in
// as the run ID — different value space (timestamps, sequence) but
// same place in the name string.
func ContainerName(scope, name, replicaID string) string {
	base := name
	if scope != "" {
		base = scope + "-" + name
	}

	return base + "." + replicaID
}

// BuildLabels assembles the --label arguments for `docker run` from
// an Identity. The umbrella createdby label is always emitted so the
// existing voodu-wide filter keeps working. Empty fields are skipped
// — docker rejects `--label k=` (no value) and there's no signal
// value in setting empty labels anyway.
func BuildLabels(id Identity) []string {
	out := []string{
		fmt.Sprintf("%s=%s", LabelCreatedBy, LabelCreatedByValue),
	}

	if id.Kind != "" {
		out = append(out, fmt.Sprintf("%s=%s", LabelKind, id.Kind))
	}

	if id.Scope != "" {
		out = append(out, fmt.Sprintf("%s=%s", LabelScope, id.Scope))
	}

	if id.Name != "" {
		out = append(out, fmt.Sprintf("%s=%s", LabelName, id.Name))
	}

	if id.ReplicaID != "" {
		out = append(out, fmt.Sprintf("%s=%s", LabelReplicaID, id.ReplicaID))
	}

	if id.ManifestHash != "" {
		out = append(out, fmt.Sprintf("%s=%s", LabelManifestHash, id.ManifestHash))
	}

	if id.CreatedAt != "" {
		out = append(out, fmt.Sprintf("%s=%s", LabelCreatedAt, id.CreatedAt))
	}

	if id.ReleaseID != "" {
		out = append(out, fmt.Sprintf("%s=%s", LabelReleaseID, id.ReleaseID))
	}

	// LabelReplicaOrdinal is emitted only for statefulset pods.
	// Deployments leave Identity.ReplicaOrdinal at the zero value;
	// emitting "0" on every deployment replica would be noise
	// (and a docker filter on ordinal=0 would suddenly match
	// every deployment pod ever spawned). Gate strictly on Kind.
	if id.Kind == KindStatefulset {
		out = append(out, fmt.Sprintf("%s=%d", LabelReplicaOrdinal, id.ReplicaOrdinal))
	}

	// Role defaults to Kind so every voodu container gets a
	// non-empty voodu.role label without callers having to set
	// it explicitly. Specific paths (release, backup) override
	// by setting Identity.Role before calling BuildLabels.
	role := id.Role
	if role == "" {
		role = id.Kind
	}

	if role != "" {
		out = append(out, fmt.Sprintf("%s=%s", LabelRole, role))
	}

	return out
}

// ParseLabels extracts a voodu Identity from a flat label map (as
// returned by `docker inspect --format '{{json .Config.Labels}}'`).
// Returns (Identity{}, false) when the createdby label is missing or
// has the wrong value — i.e. the container was not created by voodu
// or was created before M0 introduced structured labels.
func ParseLabels(m map[string]string) (Identity, bool) {
	if m == nil {
		return Identity{}, false
	}

	if m[LabelCreatedBy] != LabelCreatedByValue {
		return Identity{}, false
	}

	id := Identity{
		Kind:           m[LabelKind],
		Scope:          m[LabelScope],
		Name:           m[LabelName],
		ReplicaID:      m[LabelReplicaID],
		ManifestHash:   m[LabelManifestHash],
		CreatedAt:      m[LabelCreatedAt],
		ReleaseID:      m[LabelReleaseID],
		ReplicaOrdinal: -1,
		Role:           m[LabelRole],
	}

	// Recover the ordinal only for statefulset pods. The label
	// shouldn't exist on deployment containers; if it does (e.g.
	// a hand-edited container or a future label rename) we leave
	// the sentinel -1 so downstream logic that branches on
	// "is this stateful?" stays driven by Kind alone.
	if id.Kind == KindStatefulset {
		if v, ok := m[LabelReplicaOrdinal]; ok {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				id.ReplicaOrdinal = n
			}
		}
	}

	return id, true
}

// Matches reports whether id describes the (kind, scope, name) tuple.
// Used by the reconciler to filter live containers down to a single
// deployment's replicas.
func (id Identity) Matches(kind, scope, name string) bool {
	return id.Kind == kind && id.Scope == scope && id.Name == name
}

// LegacyContainerName reports whether name matches the pre-M0 naming
// `<app>` (bare) or `<app>-<digits>` (numeric slot suffix). Used by
// the reconciler during the M0 transition: existing containers from
// older voodu releases lack `voodu.*` labels, so label-based listing
// can't see them. Name-pattern detection lets the next reconcile
// adopt-or-replace them without a dedicated upgrade path.
//
// Once all live containers carry M0 labels (after the first apply
// post-upgrade) this helper becomes a no-op survivor — keep it as a
// safety net rather than hot-deleting.
func LegacyContainerName(name, app string) bool {
	if app == "" {
		return false
	}

	if name == app {
		return true
	}

	rest, ok := strings.CutPrefix(name, app+"-")
	if !ok {
		return false
	}

	return isAllDigits(rest)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}
