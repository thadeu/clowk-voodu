package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sort"
	"sync"
	"time"

	"go.voodu.clowk.in/internal/containers"
)

// StatefulsetHandler reconciles statefulset manifests. The shape
// mirrors DeploymentHandler intentionally — same WriteEnv/EnvFilePath
// plumbing, same Store seam — but the reconcile loop is built around
// per-pod stable identity instead of interchangeable replicas:
//
//   - Each pod is named by its ordinal (`<scope>-<name>.0`,
//     `.1`, …) so DNS / volumes / logs all line up across restarts.
//   - Spawn order is bottom-up (0 → N-1); scale-down is top-down
//     (N-1 → 0); rolling restart is top-down so pod-0 (the
//     "primary" by convention for plugins like postgres) is the
//     last to swap.
//   - Per-pod aliases are registered alongside the shared aliases,
//     so clients can dial `pg-0.scope` for a specific replica or
//     `pg.scope` for round-robin.
//
// Volumes are deferred to M-S2; release records / rollback to
// M-S3. The handler is intentionally thinner than DeploymentHandler
// because statefulset workloads (databases, caches) don't need
// release-phase commands or build-mode source ingestion.
type StatefulsetHandler struct {
	Store Store
	Log   *log.Logger

	WriteEnv    func(app string, pairs []string) (bool, error)
	EnvFilePath func(app string) string

	Containers ContainerManager

	// ControllerURL flows from the API server's wiring down to the
	// per-pod env builder so every managed container can reach
	// /describe, /config, /plugin/... without operator-set env.
	// Empty string is fine — it just means VOODU_CONTROLLER_URL
	// won't be auto-injected; the entrypoint scripts that need it
	// detect and skip the callback path. Tests leave it empty.
	ControllerURL string

	// rolloutLocks serialises rolling restart per (scope, name).
	// Two concurrent reconciles for the same statefulset would
	// otherwise race on container creation order — mutex granularity
	// is per AppID, parallel statefulsets stay independent.
	rolloutLocks sync.Map
}

// statefulsetSpec is the package-local mirror of manifest.StatefulsetSpec
// — the controller only sees JSON, so this struct re-decodes what the
// reconciler cares about. Keeping it separate from manifest.StatefulsetSpec
// avoids a reverse import (manifest already imports controller for the
// wire Manifest type).
type statefulsetSpec struct {
	Image       string             `json:"image,omitempty"`
	Replicas    int                `json:"replicas,omitempty"`
	Command     []string           `json:"command,omitempty"`
	Env         map[string]string  `json:"env,omitempty"`
	Ports       []string           `json:"ports,omitempty"`
	Volumes     []string           `json:"volumes,omitempty"`
	Network     string             `json:"network,omitempty"`
	Networks    []string           `json:"networks,omitempty"`
	NetworkMode string             `json:"network_mode,omitempty"`
	Restart     string             `json:"restart,omitempty"`
	HealthCheck string             `json:"health_check,omitempty"`

	// EnvFrom mirrors JobSpec.EnvFrom — each entry is a "scope/name"
	// (or bare "name" for the current scope) ref to another resource
	// whose env file gets stacked under the statefulset's own env
	// via --env-file. Resolution + last-wins semantics are
	// identical to the job path. See manifest.StatefulsetSpec.EnvFrom
	// for the operator-facing contract.
	EnvFrom []string `json:"env_from,omitempty"`

	// VolumeClaims is the per-pod volume-template list. M-S2 wires
	// the docker volume creation; M-S1 ignores the field but keeps
	// the JSON shape stable so plugins authored against the early
	// schema don't need a re-spec.
	VolumeClaims []volumeClaim `json:"volume_claims,omitempty"`

	// Resources mirrors manifest.ResourcesSpec — kernel-level
	// CPU/memory caps via cgroups, applied to every replica pod.
	Resources *resourcesWireSpec `json:"resources,omitempty"`

	// AssetDigests is the apply-time-stamped sha256 map for asset
	// refs the consumer touches. Server-managed (StampAssetDigests
	// writes it post plugin-expand); operators don't author this
	// field. When non-empty, the hash function uses it directly;
	// otherwise it falls back to LookupAssetDigests (/status path)
	// for legacy manifests applied before stamping was wired up.
	AssetDigests map[string]string `json:"_asset_digests,omitempty"`
}

type volumeClaim struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
	Size      string `json:"size,omitempty"`
}

// buildFrozenSet turns the on-disk frozen-replica-IDs slice into
// the O(1)-lookup map every handler-internal path expects.
// Replica IDs are strings — for statefulsets they're the ordinal
// rendered as decimal ("0", "1"); for deployments they're the
// hex containers.NewReplicaID emits ("a3f9"). The set type is
// the lowest-common-denominator that fits both.
//
// nil/empty input returns nil — callers' `if frozen[id]` checks
// are nil-safe.
func buildFrozenSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}

	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}

	return out
}

// statefulsetReplicas mirrors effectiveReplicas for deployments —
// missing/zero replica counts default to 1. Statefulsets with
// replicas=0 have no meaning at the controller layer (no pods, no
// volumes, no DNS); operators scale to zero by deleting the manifest.
func statefulsetReplicas(spec statefulsetSpec) int {
	if spec.Replicas < 1 {
		return 1
	}

	return spec.Replicas
}

// volumeName composes the docker volume name for one (scope, name,
// claim, ordinal) tuple. Deterministic so reconcile-after-restart
// finds the same volume — the data attached to pod-N IS pod-N's
// data, identified solely by name.
//
// Shape: `voodu-<scope>-<name>-<claim>-<ordinal>` (or `voodu-<name>-
// <claim>-<ordinal>` for unscoped statefulsets, which don't exist
// today but the path stays clean if M-S4 introduces a database
// kind that elides scope).
//
// Docker volume names accept `[a-zA-Z0-9_.-]`; every input field
// in voodu is already restricted to that charset by the HCL
// parser, so no further sanitisation is needed.
func volumeName(scope, name, claim string, ordinal int) string {
	base := name
	if scope != "" {
		base = scope + "-" + name
	}

	return fmt.Sprintf("voodu-%s-%s-%d", base, claim, ordinal)
}

// volumeLabels stamps a created volume so describe / prune paths
// can find it later. The createdby umbrella matches the one used
// for containers — `docker volume ls --filter label=createdby=voodu`
// enumerates everything voodu owns, then per-(scope, name) labels
// narrow further.
func volumeLabels(scope, name, claim string, ordinal int) []string {
	labels := []string{
		containers.LabelCreatedBy + "=" + containers.LabelCreatedByValue,
		containers.LabelKind + "=" + containers.KindStatefulset,
		containers.LabelName + "=" + name,
		containers.LabelReplicaOrdinal + "=" + containers.OrdinalReplicaID(ordinal),
		"voodu.claim=" + claim,
	}

	if scope != "" {
		labels = append(labels, containers.LabelScope+"="+scope)
	}

	return labels
}

// ensureClaimsForOrdinal creates one docker volume per VolumeClaim
// before pod-N boots. Volume names are deterministic — calling
// twice for the same ordinal is a no-op (EnsureVolume is
// idempotent). Returns the slice of `<volume>:<mountpath>` mount
// strings ready to drop into ContainerSpec.Volumes.
//
// Failures abort the whole sequence: a partial volume set on a
// statefulset pod is worse than no pod at all (postgres replica
// trying to start with one of two expected mounts missing would
// crash-loop in confusing ways).
func (h *StatefulsetHandler) ensureClaimsForOrdinal(scope, name string, ordinal int, claims []volumeClaim) ([]string, error) {
	if h.Containers == nil || len(claims) == 0 {
		return nil, nil
	}

	mounts := make([]string, 0, len(claims))

	for _, c := range claims {
		if c.Name == "" || c.MountPath == "" {
			return nil, fmt.Errorf("statefulset/%s-%s ordinal %d: volume_claim needs both name and mount_path", scope, name, ordinal)
		}

		volName := volumeName(scope, name, c.Name, ordinal)

		if err := h.Containers.EnsureVolume(volName, volumeLabels(scope, name, c.Name, ordinal)); err != nil {
			return nil, fmt.Errorf("ensure volume %s: %w", volName, err)
		}

		mounts = append(mounts, volName+":"+c.MountPath)
	}

	return mounts, nil
}

func (h *StatefulsetHandler) Handle(ctx context.Context, ev WatchEvent) error {
	switch ev.Type {
	case WatchPut:
		return h.apply(ctx, ev)

	case WatchDelete:
		return h.remove(ctx, ev)
	}

	return nil
}

func (h *StatefulsetHandler) apply(ctx context.Context, ev WatchEvent) error {
	if ev.Manifest == nil {
		return fmt.Errorf("put event without manifest")
	}

	spec, err := decodeStatefulsetSpec(ev.Manifest)
	if err != nil {
		return err
	}

	app := AppID(ev.Scope, ev.Name)

	if err := applyStatefulsetSpecDefaults(&spec, app); err != nil {
		return err
	}

	if h.Containers == nil {
		return nil
	}

	// Hash MUST be computed BEFORE asset interpolation —
	// see deploymentSpecHash for the rationale on why
	// literals + content digests both fold into the hash.
	//
	// Digest source is the apply-time stamp (spec.AssetDigests)
	// when present; falls back to a /status lookup for legacy
	// manifests that pre-date the stamping pipeline.
	assetDigests := resolveStampedOrLookup(spec.AssetDigests, func() map[string]string {
		return LookupAssetDigests(ctx, h.Store, collectStatefulsetAssetRefs(spec))
	})
	hash := statefulsetSpecHash(spec, assetDigests)

	// Now safe to resolve `${asset.X.Y}` literals into host
	// paths so the ContainerSpec can wire docker bind mounts
	// to real files.
	if err := resolveStatefulsetSpecAssets(ctx, h.Store, &spec); err != nil {
		return err
	}

	envChanged, err := resolveAppEnv(ctx, h.Store, h.WriteEnv, h.logf, ev.Scope, ev.Name, app, spec.Env, "statefulset")
	if err != nil {
		return err
	}

	want := statefulsetReplicas(spec)

	live, err := h.Containers.ListByIdentity(string(KindStatefulset), ev.Scope, ev.Name)
	if err != nil {
		return fmt.Errorf("list statefulset %s replicas: %w", app, err)
	}

	// Frozen ordinals — pods the operator stopped via `vd stop --freeze`.
	// Loaded once per reconcile and threaded through ensureOrdinalsUp +
	// the rolling restart paths so a frozen ordinal stays offline across
	// every spawn / recreate trigger (env-change, spec-drift, image-id
	// drift, scale-up). Empty when no operator intent exists (the
	// majority case).
	frozen, _ := h.Store.GetFrozenReplicaIDs(ctx, KindStatefulset, ev.Scope, ev.Name)
	frozenSet := buildFrozenSet(frozen)

	// Detect release-worthy applies. Same shape as
	// DeploymentHandler — first-apply, spec drift, replica
	// count change, and image-id drift each mint a new
	// applyReleaseID. The release record at the end of apply()
	// is what makes `vd rollback statefulset/...` work: it
	// captures the spec snapshot so the rollback can re-Put
	// it verbatim.
	prevStatus, _ := h.loadStatus(ctx, app)
	specDrifted := prevStatus.SpecHash != "" && prevStatus.SpecHash != hash
	firstApply := prevStatus.SpecHash == ""

	// Replica-tracking gate matches deployment: zero-value
	// prev.Replicas is the upgrade-from-old-controller signal,
	// not "operator wants zero replicas" (statefulsetReplicas
	// clamps to 1 anyway). Avoid the phantom release on first
	// reconcile after upgrade.
	replicaCountChanged := prevStatus.SpecHash != "" &&
		prevStatus.Replicas != 0 &&
		prevStatus.Replicas != want

	imageIDDrift := false

	if !firstApply && !specDrifted && spec.Image != "" && len(live) > 0 {
		if differ, err := h.Containers.ImageIDsDiffer(live[0].Name, spec.Image); err == nil && differ {
			imageIDDrift = true
		}
	}

	var applyReleaseID string

	if firstApply || specDrifted || replicaCountChanged || imageIDDrift {
		applyReleaseID = newReleaseID()
	}

	// Scale-up first (bottom-up so a freshly minted pod-0 is up
	// before pod-1 tries to discover it). pruneOrdinalsAbove
	// follows the opposite order so scale-down peels off the
	// highest ordinals — pod-0 is the convention-bearer ("primary"
	// for postgres) and outlives the rest.
	if err := h.ensureOrdinalsUp(ctx, ev.Scope, ev.Name, app, live, want, spec, hash, applyReleaseID, frozenSet); err != nil {
		return err
	}

	if err := h.pruneOrdinalsAbove(ev.Scope, ev.Name, app, want); err != nil {
		h.logf("statefulset/%s scale-down failed: %v", ev.Name, err)
	}

	// Rolling restart on spec drift, top-down. The "skip on first
	// reconcile after upgrade" baseline path mirrors deployment:
	// without prior status, baseline silently and let the next
	// apply detect drift.
	specDriftRestarted, err := h.recreateOrdinalsIfSpecChanged(ctx, ev.Scope, ev.Name, app, spec, hash, applyReleaseID, frozenSet)
	if err != nil {
		return err
	}

	// Env-change rolling restart, top-down. Mirrors the
	// DeploymentHandler's `envChanged && !recreatedAny` branch —
	// docker reads --env-file at `docker run` time, so a config
	// bucket change (e.g. REDIS_MASTER_ORDINAL flipped by
	// `vd redis:failover`) only reaches the running redis-server
	// process if we recreate the pod. Without this, failover
	// writes the bucket but the wrapper script never sees the
	// new ordinal, leaving the cluster wedged on the old role
	// assignment.
	//
	// Skipped when the spec-drift recreate above already cycled
	// every pod — the new containers came up with the freshly-
	// linked env mounted, so a second restart in the same
	// reconcile would just churn.
	//
	// firstApply is the upgrade-from-old-controller signal — no
	// prior status means we just baseline-stamped, and the
	// current `envChanged=true` is reporting "the env file
	// existed before but the controller didn't track it",
	// which is not a real change.
	if envChanged && !specDriftRestarted && !firstApply {
		if err := h.rollingReplaceTopDown(ctx, ev.Scope, ev.Name, app, spec, hash, "", frozenSet); err != nil {
			h.logf("statefulset/%s env-change rolling restart failed: %v", ev.Name, err)
		}
	}

	if applyReleaseID != "" {
		now := time.Now().UTC()

		record := ReleaseRecord{
			ID:           applyReleaseID,
			SpecHash:     hash,
			Image:        spec.Image,
			Status:       ReleaseStatusSucceeded,
			StartedAt:    now,
			EndedAt:      now,
			SpecSnapshot: ev.Manifest.Spec,
		}

		if err := h.appendReleaseRecord(ctx, app, record); err != nil {
			h.logf("statefulset/%s release record persist failed: %v", ev.Name, err)
		}
	}

	return nil
}

// remove tears down every pod in the statefulset and clears the
// status blob. Volumes are LEFT in place — the operator's mental
// model is "delete manifest = stop running, but my data sticks
// around until I `vd delete --prune`". M-S2 will document the
// volume names in `vd describe` output so the operator can find
// them; M-S3 wires the prune flag through.
func (h *StatefulsetHandler) remove(ctx context.Context, ev WatchEvent) error {
	app := AppID(ev.Scope, ev.Name)

	if h.Containers != nil {
		slots, err := h.Containers.ListByIdentity(string(KindStatefulset), ev.Scope, ev.Name)
		if err != nil {
			return fmt.Errorf("list replicas: %w", err)
		}

		// Top-down removal: high ordinals first. Symmetric to the
		// scale-down path; lets pod-0 stay reachable longest in
		// case other plugins are draining connections.
		sortSlotsByOrdinalDesc(slots)

		for _, s := range slots {
			h.logf("statefulset/%s removing replica %s", ev.Name, s.Name)

			if err := h.Containers.Remove(s.Name); err != nil {
				return fmt.Errorf("remove %s: %w", s.Name, err)
			}
		}
	}

	if err := h.Store.DeleteStatus(ctx, KindStatefulset, app); err != nil {
		return fmt.Errorf("clear statefulset status: %w", err)
	}

	h.logf("statefulset/%s deleted (pods removed, volumes preserved)", ev.Name)

	return nil
}

// ensureOrdinalsUp spawns missing ordinals (0..want-1) bottom-up.
// Already-present ordinals are left alone — recreate-on-drift is a
// separate phase. Each new pod carries:
//
//   - voodu.kind=statefulset, voodu.replica_ordinal=<n>
//   - container name <scope>-<name>.<n>
//   - per-pod aliases (`<name>-<n>.<scope>`) AND shared aliases
//     (`<name>.<scope>`) so clients can dial either
//
// Spawn order is sequential with slotRolloutPause between pods so
// pod-N has time to reach a serving state before pod-(N+1) tries
// to bootstrap against it. Without health probes (M-S5), the sleep
// is the only synchronization we have.
func (h *StatefulsetHandler) ensureOrdinalsUp(_ context.Context, scope, name, app string, live []ContainerSlot, want int, spec statefulsetSpec, hash, releaseID string, frozen map[string]bool) error {
	if spec.Image == "" {
		return fmt.Errorf("statefulset/%s: image is required (statefulsets do not support build-mode in M-S1)", app)
	}

	present := make(map[int]bool, len(live))

	for _, s := range live {
		if n, ok := s.Identity.Ordinal(); ok {
			present[n] = true
		}
	}

	envFile := ""
	if h.EnvFilePath != nil {
		envFile = h.EnvFilePath(app)
	}

	// Resolve env_from refs to additional --env-file paths once per
	// reconcile (not per-ordinal — the resolution is identical for
	// every replica). Same shape + last-wins semantics as the job
	// path. When env_from is empty this is a nil slice and the
	// container creation skips the extra --env-file flags.
	extraEnvFiles, err := resolveEnvFromList(spec.EnvFrom, scope, h.logf)
	if err != nil {
		return fmt.Errorf("resolve env_from: %w", err)
	}

	for n := 0; n < want; n++ {
		if present[n] {
			continue
		}

		// Frozen ordinal — operator parked this pod via
		// `vd stop --freeze`. Don't spawn a fresh container
		// here; the freeze annotation persists until
		// `vd start` clears it. If the operator's intent
		// later includes this ordinal again, the unfreeze
		// path fires another reconcile that lands here
		// without the freeze flag.
		if frozen[containers.OrdinalReplicaID(n)] {
			h.logf("statefulset/%s ordinal %d frozen, skipping spawn", name, n)
			continue
		}

		cname := containers.ContainerName(scope, name, containers.OrdinalReplicaID(n))

		labels := containers.BuildLabels(containers.Identity{
			Kind:           containers.KindStatefulset,
			Scope:          scope,
			Name:           name,
			ReplicaID:      containers.OrdinalReplicaID(n),
			ReplicaOrdinal: n,
			ManifestHash:   hash,
			CreatedAt:      time.Now().UTC().Format(time.RFC3339),
			ReleaseID:      releaseID,
		})

		// Per-pod aliases first so the deterministic
		// `<name>-<n>.<scope>` form is the primary DNS name on
		// each network. The shared aliases follow — clients
		// that round-robin (`<name>.<scope>`) still find every
		// replica.
		aliases := append([]string(nil), BuildPodNetworkAliases(scope, name, n)...)
		aliases = append(aliases, BuildNetworkAliases(scope, name)...)

		// Materialise per-pod volume claims first — the docker
		// volume must exist before `docker run -v <volume>:<path>`
		// references it, otherwise the daemon creates an
		// unlabeled anonymous volume that diverges from voodu's
		// naming scheme and breaks the prune path.
		claimMounts, err := h.ensureClaimsForOrdinal(scope, name, n, spec.VolumeClaims)
		if err != nil {
			return err
		}

		// Append claim mounts AFTER the operator-declared `volumes`
		// so a misspelled claim (same path as a HCL `volumes` entry)
		// surfaces as the operator-intent winning, not the synth
		// volume silently overriding.
		mountedVolumes := append([]string(nil), spec.Volumes...)
		mountedVolumes = append(mountedVolumes, claimMounts...)

		// Per-pod identity env: VOODU_REPLICA_ORDINAL,
		// VOODU_REPLICA_ID, VOODU_SCOPE, VOODU_NAME. Plugin-
		// authored entrypoints (redis, postgres) read these
		// to pick a role at boot. Operator-supplied env from
		// the HCL `env { ... }` block (already in spec.Env at
		// this point) layers underneath — platform names win
		// on collision so a typo'd `env.VOODU_SCOPE = "x"`
		// doesn't poison the pod's identity.
		podEnv := MergePodEnv(
			BuildPlatformEnv(h.ControllerURL),
			MergePodEnv(
				BuildStatefulsetPodEnv(scope, name, n),
				spec.Env,
			),
		)

		cpu, memBytes, err := dockerResources(spec.Resources)
		if err != nil {
			return fmt.Errorf("statefulset/%s ordinal %d: %w", name, n, err)
		}

		_, err = h.Containers.Ensure(ContainerSpec{
			Name:             cname,
			Image:            spec.Image,
			Command:          spec.Command,
			Ports:            spec.Ports,
			Volumes:          mountedVolumes,
			Networks:         spec.Networks,
			NetworkMode:      spec.NetworkMode,
			NetworkAliases:   aliases,
			Restart:          spec.Restart,
			EnvFile:          envFile,
			ExtraEnvFiles:    extraEnvFiles,
			Env:              podEnv,
			Labels:           labels,
			CPULimit:         cpu,
			MemoryLimitBytes: memBytes,
		})
		if err != nil {
			return fmt.Errorf("ensure ordinal %d (%s): %w", n, cname, err)
		}

		h.logf("statefulset/%s ordinal %d ready (image=%s)", name, n, spec.Image)

		// Brief pause before spawning the next ordinal. Replaces
		// the readiness probe we don't have yet — pod-(N+1) often
		// connects to pod-N during init (postgres replica connects
		// to primary), and a 2s gap is enough for docker to wire
		// the network alias and the process to start listening.
		if n < want-1 {
			time.Sleep(slotRolloutPause)
		}
	}

	return nil
}

// pruneOrdinalsAbove removes any pod whose ordinal >= want. Top-down
// (high ordinals first) so pod-0 is preserved as long as possible —
// matches the postgres-style "primary lives at ordinal 0" convention.
//
// Volumes attached to pruned ordinals are LEFT in place (docker
// volume lifecycle is independent of container lifecycle by default).
// Operator runs `vd delete --prune` to wipe — M-S3 wires that knob.
func (h *StatefulsetHandler) pruneOrdinalsAbove(scope, name, app string, want int) error {
	if h.Containers == nil {
		return nil
	}

	live, err := h.Containers.ListByIdentity(string(KindStatefulset), scope, name)
	if err != nil {
		return err
	}

	sortSlotsByOrdinalDesc(live)

	for _, s := range live {
		ord, ok := s.Identity.Ordinal()
		if !ok {
			// Container without an ordinal label — shouldn't
			// happen (BuildLabels emits it for every statefulset
			// pod), but log and skip rather than spuriously
			// remove what could be a hand-spawned debug instance.
			h.logf("statefulset/%s replica %s missing ordinal label, skipping prune", app, s.Name)
			continue
		}

		if ord < want {
			continue
		}

		h.logf("statefulset/%s scale-down: removing ordinal %d (%s)", name, ord, s.Name)

		if err := h.Containers.Remove(s.Name); err != nil {
			return fmt.Errorf("remove %s: %w", s.Name, err)
		}
	}

	return nil
}

// recreateOrdinalsIfSpecChanged compares the current spec hash to
// the persisted one and, on drift, rolls each ordinal top-down.
// First-reconcile-after-upgrade (no persisted status) baselines the
// hash without churning live pods — same posture as DeploymentHandler.
//
// Top-down restart matters for the postgres convention: pod-0
// (primary) restarts last, after every replica has already swapped
// to the new image. Failover risk is minimised because the new
// followers can keep streaming from the old primary right up until
// the very end.
// recreateOrdinalsIfSpecChanged returns (restarted, err) — restarted=true
// signals that every pod was just cycled, so the caller should NOT
// fire a second rolling restart in the same reconcile (env-change
// path). Pre-existing single-return-value callers are gone; the
// tuple is the contract going forward.
func (h *StatefulsetHandler) recreateOrdinalsIfSpecChanged(ctx context.Context, scope, name, app string, spec statefulsetSpec, hash, releaseID string, frozen map[string]bool) (bool, error) {
	prev, err := h.loadStatus(ctx, app)
	if err != nil {
		return false, fmt.Errorf("read statefulset status: %w", err)
	}

	if prev.SpecHash == "" {
		// Baseline only.
		return false, h.writeStatus(ctx, app, spec.Image, hash, statefulsetReplicas(spec))
	}

	driftHash := prev.SpecHash != hash
	driftImage := false

	if !driftHash {
		live, _ := h.Containers.ListByIdentity(string(KindStatefulset), scope, name)
		if len(live) > 0 && spec.Image != "" {
			differ, err := h.Containers.ImageIDsDiffer(live[0].Name, spec.Image)
			if err == nil && differ {
				driftImage = true
			}
		}
	}

	if !driftHash && !driftImage {
		// Hash and image both stable — only the replica count
		// could have moved. Persist the current declared count
		// so scale-only changes don't get lost on the next replay.
		return false, h.writeStatus(ctx, app, spec.Image, hash, statefulsetReplicas(spec))
	}

	reason := fmt.Sprintf("spec drift (hash %s → %s)", shortHash(prev.SpecHash), shortHash(hash))
	if driftImage {
		reason = fmt.Sprintf("image id drift (tag %s rebuilt under same name)", spec.Image)
	}

	h.logf("statefulset/%s %s, rolling restart top-down", app, reason)

	if err := h.rollingReplaceTopDown(ctx, scope, name, app, spec, hash, releaseID, frozen); err != nil {
		return false, err
	}

	return true, h.writeStatus(ctx, app, spec.Image, hash, statefulsetReplicas(spec))
}

// rollingReplaceTopDown iterates ordinals from highest to lowest
// and replaces each pod in place. Per-(scope,name) lock prevents
// two concurrent reconciles from racing on the same fleet — a
// race would otherwise interleave Remove/Ensure calls and produce
// stranded ordinals.
func (h *StatefulsetHandler) rollingReplaceTopDown(_ context.Context, scope, name, app string, spec statefulsetSpec, hash, releaseID string, frozen map[string]bool) error {
	val, _ := h.rolloutLocks.LoadOrStore(app, &sync.Mutex{})

	mu, _ := val.(*sync.Mutex)
	if mu == nil {
		return fmt.Errorf("internal: rollout lock for %s missing", app)
	}

	mu.Lock()

	defer mu.Unlock()

	live, err := h.Containers.ListByIdentity(string(KindStatefulset), scope, name)
	if err != nil {
		return fmt.Errorf("list replicas: %w", err)
	}

	sortSlotsByOrdinalDesc(live)

	envFile := ""
	if h.EnvFilePath != nil {
		envFile = h.EnvFilePath(app)
	}

	// Same env_from resolution as ensureOrdinalsUp — runs once per
	// rolling-replace cycle, reused for every replica recreated in
	// this pass. Empty list when env_from isn't declared.
	extraEnvFiles, err := resolveEnvFromList(spec.EnvFrom, scope, h.logf)
	if err != nil {
		return fmt.Errorf("resolve env_from: %w", err)
	}

	for i, s := range live {
		ord, ok := s.Identity.Ordinal()
		if !ok {
			h.logf("statefulset/%s replica %s missing ordinal, skipping in restart", app, s.Name)
			continue
		}

		// Frozen ordinals stay parked through env-change /
		// spec-drift / image-id rolling restarts. Operator's
		// `vd stop --freeze` intent persists until they
		// explicitly `vd start` to bring the pod back.
		if frozen[containers.OrdinalReplicaID(ord)] {
			h.logf("statefulset/%s ordinal %d frozen, skipping in rolling restart", name, ord)
			continue
		}

		labels := containers.BuildLabels(containers.Identity{
			Kind:           containers.KindStatefulset,
			Scope:          scope,
			Name:           name,
			ReplicaID:      containers.OrdinalReplicaID(ord),
			ReplicaOrdinal: ord,
			ManifestHash:   hash,
			CreatedAt:      time.Now().UTC().Format(time.RFC3339),
			ReleaseID:      releaseID,
		})

		aliases := append([]string(nil), BuildPodNetworkAliases(scope, name, ord)...)
		aliases = append(aliases, BuildNetworkAliases(scope, name)...)

		newName := containers.ContainerName(scope, name, containers.OrdinalReplicaID(ord))

		// Same name as before — Remove + Ensure with the original
		// ordinal-derived container name. Docker named volumes
		// survive container removal, so the per-pod claims
		// re-attach to the same data: this is what makes the
		// "rolling restart preserves data" guarantee real.
		if err := h.Containers.Remove(s.Name); err != nil {
			return fmt.Errorf("remove %s during rolling restart: %w", s.Name, err)
		}

		// Re-ensure claims (idempotent — same volume names) so
		// new pods always see their attached storage even if a
		// claim was added between the original spawn and now.
		claimMounts, err := h.ensureClaimsForOrdinal(scope, name, ord, spec.VolumeClaims)
		if err != nil {
			return err
		}

		mountedVolumes := append([]string(nil), spec.Volumes...)
		mountedVolumes = append(mountedVolumes, claimMounts...)

		// Same identity env as the spawn path — ordinal is
		// preserved across rolling-restart (volumes do too),
		// so the role decision (master vs replica) doesn't
		// flip just because the pod was recreated.
		podEnv := MergePodEnv(
			BuildPlatformEnv(h.ControllerURL),
			MergePodEnv(
				BuildStatefulsetPodEnv(scope, name, ord),
				spec.Env,
			),
		)

		cpu, memBytes, err := dockerResources(spec.Resources)
		if err != nil {
			return fmt.Errorf("statefulset/%s respawn ordinal %d: %w", name, ord, err)
		}

		if _, err := h.Containers.Ensure(ContainerSpec{
			Name:             newName,
			Image:            spec.Image,
			Command:          spec.Command,
			Ports:            spec.Ports,
			Volumes:          mountedVolumes,
			Networks:         spec.Networks,
			NetworkMode:      spec.NetworkMode,
			NetworkAliases:   aliases,
			Restart:          spec.Restart,
			EnvFile:          envFile,
			ExtraEnvFiles:    extraEnvFiles,
			Env:              podEnv,
			Labels:           labels,
			CPULimit:         cpu,
			MemoryLimitBytes: memBytes,
		}); err != nil {
			return fmt.Errorf("respawn ordinal %d: %w", ord, err)
		}

		h.logf("statefulset/%s ordinal %d replaced", name, ord)

		if i < len(live)-1 {
			time.Sleep(slotRolloutPause)
		}
	}

	return nil
}

// resolveStatefulsetSpecAssets walks every operator-supplied
// string field of the spec and expands `${asset.<name>.<key>}`
// references into the materialised host paths. Mirror of
// resolveDeploymentSpecAssets — same fields covered, same
// posture: VolumeClaim mount_paths and Networks are left
// untouched (container-side / docker-bridge identifiers, not
// host paths).
func resolveStatefulsetSpecAssets(ctx context.Context, store Store, spec *statefulsetSpec) error {
	lookup := makeAssetPathLookup(ctx, store)

	var err error

	if spec.Image, err = InterpolateAssetRefs(spec.Image, lookup); err != nil {
		return err
	}

	if spec.Command, err = resolveAssetRefsInSlice(spec.Command, lookup); err != nil {
		return err
	}

	if spec.Volumes, err = resolveAssetRefsInSlice(spec.Volumes, lookup); err != nil {
		return err
	}

	if spec.Ports, err = resolveAssetRefsInSlice(spec.Ports, lookup); err != nil {
		return err
	}

	if spec.Env, err = resolveAssetRefsInMap(spec.Env, lookup); err != nil {
		return err
	}

	return nil
}

// applyStatefulsetSpecDefaults mirrors applyDeploymentSpecDefaults
// — image fallback (`<app>:latest` only when registry pull would
// be wrong; statefulsets without an explicit image error out per
// ensureOrdinalsUp), voodu0 auto-join, port loopback, restart
// policy. Build-mode is intentionally NOT supported on M-S1.
func applyStatefulsetSpecDefaults(spec *statefulsetSpec, app string) error {
	if spec.Image == "" {
		// Statefulsets are image-mode only on M-S1 — registry
		// images for postgres/redis/mongo. Empty Image is a real
		// authoring mistake (operator forgot to fill it), not an
		// invitation to build-from-source. Surface clearly.
		return fmt.Errorf("statefulset/%s: image is required", app)
	}

	switch spec.NetworkMode {
	case "":
		// Bridge — voodu0 auto-join below.
	case "host", "none":
		if len(spec.Networks) > 0 || spec.Network != "" {
			return fmt.Errorf("statefulset/%s: network_mode=%q is mutually exclusive with network/networks", app, spec.NetworkMode)
		}
	default:
		return fmt.Errorf("statefulset/%s: network_mode=%q not supported", app, spec.NetworkMode)
	}

	if spec.NetworkMode == "" {
		if len(spec.Networks) == 0 && spec.Network != "" {
			spec.Networks = []string{spec.Network}
		}

		if !slices.Contains(spec.Networks, "voodu0") {
			spec.Networks = append(spec.Networks, "voodu0")
		}
	}

	spec.Ports = normalizePorts(spec.Ports)

	if spec.Restart == "" {
		spec.Restart = "unless-stopped"
	}

	return nil
}

// statefulsetSpecHash mirrors deploymentSpecHash — sha256 of
// the runtime-shaping fields plus the asset content digests
// the spec depends on (see deploymentSpecHash for the
// rationale on why asset digests fold into the hash). Replicas
// is excluded for the same reason (scale shouldn't recreate
// already-running pods); volumes / volume_claims ARE in the
// hash because changing them changes what the pod is meant
// to be.
//
// Same ordering invariant as the deployment counterpart: hash
// MUST be called BEFORE resolveStatefulsetSpecAssets, so the
// literal `${asset.X.Y}` text is what gets hashed (alongside
// the corresponding digests).
func statefulsetSpecHash(spec statefulsetSpec, assetDigests map[string]string) string {
	nets := append([]string(nil), spec.Networks...)
	sort.Strings(nets)

	claims := append([]volumeClaim(nil), spec.VolumeClaims...)
	sort.Slice(claims, func(i, j int) bool { return claims[i].Name < claims[j].Name })

	input := struct {
		Image        string            `json:"image"`
		Command      []string          `json:"command"`
		Ports        []string          `json:"ports"`
		Volumes      []string          `json:"volumes"`
		Env          map[string]string `json:"env"`
		Networks     []string          `json:"networks"`
		NetworkMode  string            `json:"network_mode"`
		Restart      string            `json:"restart"`
		VolumeClaims []volumeClaim     `json:"volume_claims"`
		Assets       []string          `json:"assets,omitempty"`
	}{
		Image:        spec.Image,
		Command:      spec.Command,
		Ports:        spec.Ports,
		Volumes:      spec.Volumes,
		Env:          spec.Env,
		Networks:     nets,
		NetworkMode:  spec.NetworkMode,
		Restart:      spec.Restart,
		VolumeClaims: claims,
		Assets:       flattenAssetDigests(assetDigests),
	}

	b, _ := json.Marshal(input)
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// collectStatefulsetAssetRefs is the statefulset twin of
// collectDeploymentAssetRefs — walks every operator-supplied
// string in the spec for `${asset.<name>.<key>}` patterns.
// Same fields as deployment's, plus VolumeClaim mount paths
// are NOT walked because mount_path is a CONTAINER path, not
// an asset reference.
func collectStatefulsetAssetRefs(spec statefulsetSpec) []assetRef {
	out := collectAssetRefs(spec.Image)

	out = append(out, collectAssetRefs(spec.Command...)...)
	out = append(out, collectAssetRefs(spec.Volumes...)...)
	out = append(out, collectAssetRefs(spec.Ports...)...)

	for _, v := range spec.Env {
		out = append(out, collectAssetRefs(v)...)
	}

	return out
}

// loadStatus / writeStatus reuse DeploymentStatus as the on-disk
// shape — fields are byte-identical (Image / SpecHash / Replicas /
// Releases). M-S3 introduces a dedicated StatefulsetStatus if the
// shapes diverge; until then the same blob serves both kinds with
// no migration cost.
func (h *StatefulsetHandler) loadStatus(ctx context.Context, app string) (DeploymentStatus, error) {
	raw, err := h.Store.GetStatus(ctx, KindStatefulset, app)
	if err != nil || raw == nil {
		return DeploymentStatus{}, err
	}

	var st DeploymentStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return DeploymentStatus{}, err
	}

	return st, nil
}

func (h *StatefulsetHandler) writeStatus(ctx context.Context, app, image, hash string, replicas int) error {
	prev, _ := h.loadStatus(ctx, app)

	prev.Image = image
	prev.SpecHash = hash
	prev.Replicas = replicas

	blob, err := json.Marshal(prev)
	if err != nil {
		return err
	}

	return h.Store.PutStatus(ctx, KindStatefulset, app, blob)
}

// decodeStatefulsetSpec parses the manifest's Spec JSON. Empty spec
// is rejected — statefulset without an image is meaningless (the
// pod has nothing to run), and the field validation in
// applyStatefulsetSpecDefaults catches that. This decode just
// guards against malformed JSON / wrong field shape.
func decodeStatefulsetSpec(m *Manifest) (statefulsetSpec, error) {
	var spec statefulsetSpec

	if len(m.Spec) == 0 {
		return spec, fmt.Errorf("statefulset/%s: empty spec", m.Name)
	}

	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return spec, fmt.Errorf("decode statefulset spec: %w", err)
	}

	return spec, nil
}

// sortSlotsByOrdinalDesc orders by ordinal descending. Slots with
// no recoverable ordinal (`Identity.Ordinal()` returns false)
// sort last so the rolling-restart loop reaches them after all
// well-formed ordinals — they get logged and skipped per the
// safety-net comment in pruneOrdinalsAbove.
func sortSlotsByOrdinalDesc(slots []ContainerSlot) {
	sort.Slice(slots, func(i, j int) bool {
		ai, oki := slots[i].Identity.Ordinal()
		aj, okj := slots[j].Identity.Ordinal()

		switch {
		case oki && okj:
			return ai > aj
		case oki:
			return true
		case okj:
			return false
		default:
			return false
		}
	})
}

func (h *StatefulsetHandler) logf(format string, args ...any) {
	if h.Log == nil {
		return
	}

	h.Log.Printf(format, args...)
}

// Restart performs an imperative rolling restart of every live pod
// — the statefulset twin of DeploymentHandler.Restart. Used by
// `vd restart statefulset/<scope>/<name>`. Top-down ordering
// preserved.
func (h *StatefulsetHandler) Restart(ctx context.Context, scope, name string) error {
	app := AppID(scope, name)

	manifest, err := h.Store.Get(ctx, KindStatefulset, scope, name)
	if err != nil {
		return fmt.Errorf("read statefulset manifest: %w", err)
	}

	if manifest == nil {
		return fmt.Errorf("statefulset/%s/%s not found", scope, name)
	}

	spec, err := decodeStatefulsetSpec(manifest)
	if err != nil {
		return err
	}

	if err := applyStatefulsetSpecDefaults(&spec, app); err != nil {
		return err
	}

	assetDigests := resolveStampedOrLookup(spec.AssetDigests, func() map[string]string {
		return LookupAssetDigests(ctx, h.Store, collectStatefulsetAssetRefs(spec))
	})
	hash := statefulsetSpecHash(spec, assetDigests)

	if err := resolveStatefulsetSpecAssets(ctx, h.Store, &spec); err != nil {
		return err
	}

	live, err := h.Containers.ListByIdentity(string(KindStatefulset), scope, name)
	if err != nil {
		return fmt.Errorf("list replicas: %w", err)
	}

	if len(live) == 0 {
		return fmt.Errorf("statefulset/%s has no live replicas to restart", app)
	}

	if _, err := resolveAppEnv(ctx, h.Store, h.WriteEnv, h.logf, scope, name, app, spec.Env, "statefulset"); err != nil {
		return fmt.Errorf("link env: %w", err)
	}

	h.logf("statefulset/%s rolling restart of %d replica(s) requested", app, len(live))

	// Frozen ordinals stay parked on imperative restarts too — the
	// operator's `vd stop --freeze` intent is more durable than a
	// `vd restart` invocation. To bring a frozen pod back into the
	// fleet, the operator runs `vd start` first.
	frozen, _ := h.Store.GetFrozenReplicaIDs(ctx, KindStatefulset, scope, name)

	// Imperative restart — operator-driven, no release context
	// to inherit. Pods spawn with empty release_id (matches
	// DeploymentHandler.Restart's posture).
	return h.rollingReplaceTopDown(ctx, scope, name, app, spec, hash, "", buildFrozenSet(frozen))
}

// appendReleaseRecord prepends a record to the statefulset's
// release history, capped at maxReleaseHistory. Mirrors the
// deployment helper but writes under KindStatefulset so the two
// histories stay independent — describing a deployment never
// surfaces statefulset releases and vice versa.
//
// The status blob shape is reused from DeploymentStatus (Image,
// SpecHash, Replicas, Releases) — fields are byte-identical, so
// no separate type avoids decode drift.
func (h *StatefulsetHandler) appendReleaseRecord(ctx context.Context, app string, r ReleaseRecord) error {
	prev, _ := h.loadStatus(ctx, app)

	prev.Releases = append([]ReleaseRecord{r}, prev.Releases...)

	if len(prev.Releases) > maxReleaseHistory {
		prev.Releases = prev.Releases[:maxReleaseHistory]
	}

	raw, err := json.Marshal(prev)
	if err != nil {
		return err
	}

	return h.Store.PutStatus(ctx, KindStatefulset, app, raw)
}

// Rollback re-applies a past release's spec snapshot. Critical
// difference from deployment rollback: VOLUMES ARE PRESERVED.
// Each ordinal's volume name is derived from (scope, name, claim,
// ordinal), and rolling-replace reuses the same name on respawn —
// so the old data flows back into the rolled-back pod
// automatically. No image retag is needed (statefulset is
// image-mode, the spec.Image is just a registry tag).
//
// Scale-down to a smaller snapshot (e.g. rolling from 5 → 3
// replicas) drops pods 3 and 4 but their volumes are LEFT in
// place. A future scale-up to 5 would re-attach them — operator
// recovers data without doing anything special. `vd delete
// --prune` is the only path that destroys volumes.
//
// targetID="" picks the second-most-recent succeeded release
// (heroku-style "rollback to previous"); explicit ID errors out
// when not found or not Succeeded. Returns the new release ID
// the rollback was assigned.
func (h *StatefulsetHandler) Rollback(ctx context.Context, scope, name, targetID string) (string, error) {
	app := AppID(scope, name)

	current, err := h.Store.Get(ctx, KindStatefulset, scope, name)
	if err != nil {
		return "", fmt.Errorf("read current manifest: %w", err)
	}

	if current == nil {
		return "", fmt.Errorf("statefulset/%s/%s not found", scope, name)
	}

	prev, err := h.loadStatus(ctx, app)
	if err != nil {
		return "", fmt.Errorf("read status: %w", err)
	}

	target, err := pickRollbackTarget(prev.Releases, targetID)
	if err != nil {
		return "", err
	}

	rollback := *current
	rollback.Spec = target.SpecSnapshot

	if _, err := h.Store.Put(ctx, &rollback); err != nil {
		return "", fmt.Errorf("apply rollback manifest: %w", err)
	}

	newID := newReleaseID()

	rollbackRecord := ReleaseRecord{
		ID:             newID,
		RolledBackFrom: target.ID,
		SpecHash:       target.SpecHash,
		Image:          target.Image,
		Status:         ReleaseStatusSucceeded,
		StartedAt:      time.Now().UTC(),
		EndedAt:        time.Now().UTC(),
		SpecSnapshot:   target.SpecSnapshot,
	}

	if err := h.appendReleaseRecord(ctx, app, rollbackRecord); err != nil {
		h.logf("statefulset/%s rollback record persist failed: %v", app, err)
	}

	if err := h.rolloutRollback(ctx, scope, name, app, &rollback, target, newID); err != nil {
		h.logf("statefulset/%s rollback rolling restart failed: %v", app, err)
		return newID, fmt.Errorf("rollback rolling restart: %w", err)
	}

	h.logf("statefulset/%s rolled back to %s (new release %s)", app, target.ID, newID)

	return newID, nil
}

// rolloutRollback brings the running fleet to the snapshot's shape:
//
//   1. Scale down: drop ordinals above the snapshot's want.
//      VOLUMES OF DROPPED PODS STAY — that's the contract
//      separating statefulset rollback from deployment rollback.
//      Operator recovers data via re-scale or `vd delete --prune`.
//   2. Rolling-replace top-down: each surviving ordinal swaps to
//      the rolled-back spec. Pod-0 is the last to swap (postgres
//      primary stays serving longest).
//   3. Scale up: if snapshot wants MORE replicas than currently
//      live, spawn the missing ordinals. Their volumes — if they
//      existed from a prior peak — re-attach automatically.
//   4. Re-stamp release_id: any pod whose label still points at
//      the rolled-back-FROM release gets recycled so `vd describe`
//      shows the new release_id on every replica. Mirror of the
//      deployment rollback's Phase 4.
func (h *StatefulsetHandler) rolloutRollback(ctx context.Context, scope, name, app string, rollback *Manifest, target *ReleaseRecord, newReleaseID string) error {
	if h.Containers == nil {
		return nil
	}

	spec, err := decodeStatefulsetSpec(rollback)
	if err != nil {
		return fmt.Errorf("decode rollback spec: %w", err)
	}

	if err := applyStatefulsetSpecDefaults(&spec, app); err != nil {
		return fmt.Errorf("apply defaults: %w", err)
	}

	if err := resolveStatefulsetSpecAssets(ctx, h.Store, &spec); err != nil {
		return fmt.Errorf("resolve asset refs: %w", err)
	}

	want := statefulsetReplicas(spec)

	// Rollback honours frozen ordinals — operator's `vd stop`
	// intent is preserved. Snapshot's spec defines what role each
	// pod runs after the rollback; freeze decides whether THIS
	// reconcile cycle re-spawns the pod or leaves it parked for
	// `vd start` to revive later.
	frozen, _ := h.Store.GetFrozenReplicaIDs(ctx, KindStatefulset, scope, name)
	frozenSet := buildFrozenSet(frozen)

	// Phase 1: scale down. pruneOrdinalsAbove already drops
	// containers top-down; volumes survive because we never call
	// RemoveVolume here (only `vd delete --prune` does).
	if err := h.pruneOrdinalsAbove(scope, name, app, want); err != nil {
		return fmt.Errorf("scale down for rollback: %w", err)
	}

	live, err := h.Containers.ListByIdentity(string(KindStatefulset), scope, name)
	if err != nil {
		return fmt.Errorf("list replicas (post scale-down): %w", err)
	}

	// Phase 2: rolling-replace surviving ordinals so every pod
	// runs the rolled-back image/command. Same name → same
	// volume re-mounts, data preserved.
	if len(live) > 0 {
		if err := h.rollingReplaceTopDown(ctx, scope, name, app, spec, target.SpecHash, newReleaseID, frozenSet); err != nil {
			return err
		}
	}

	// Phase 3: scale up if snapshot wants more pods than live.
	live, err = h.Containers.ListByIdentity(string(KindStatefulset), scope, name)
	if err != nil {
		return fmt.Errorf("list replicas (post replace): %w", err)
	}

	if len(live) < want {
		if err := h.ensureOrdinalsUp(ctx, scope, name, app, live, want, spec, target.SpecHash, newReleaseID, frozenSet); err != nil {
			return fmt.Errorf("scale up for rollback: %w", err)
		}
	}

	// Phase 4: re-stamp any pod still on the old release_id.
	// Concurrent reconciler apply() (woken by the manifest Put)
	// can spawn pods with its own minted releaseID; the rollback
	// is the end-state authority, so we sweep mismatched labels
	// at the end. Same posture as DeploymentHandler.rolloutRollback.
	final, err := h.Containers.ListByIdentity(string(KindStatefulset), scope, name)
	if err != nil {
		return fmt.Errorf("list replicas (post scale): %w", err)
	}

	stale := filterSlots(final, func(s ContainerSlot) bool {
		return s.Identity.ReleaseID != newReleaseID
	})

	if len(stale) > 0 {
		h.logf("statefulset/%s rollback re-stamping %d replica(s) to release_id=%s",
			app, len(stale), newReleaseID)

		if err := h.rollingReplaceTopDown(ctx, scope, name, app, spec, target.SpecHash, newReleaseID, frozenSet); err != nil {
			return fmt.Errorf("re-stamp replicas: %w", err)
		}
	}

	if err := h.writeStatus(ctx, app, spec.Image, target.SpecHash, want); err != nil {
		h.logf("statefulset/%s rollback status persist failed: %v", app, err)
	}

	return nil
}

// Volumes lists the docker named volumes this statefulset owns —
// one per (claim, ordinal). Read-only counterpart to PruneVolumes;
// `vd describe statefulset/...` calls this to render the volume
// section without exposing the full ContainerManager seam to the
// API layer.
func (h *StatefulsetHandler) Volumes(scope, name string) ([]string, error) {
	if h.Containers == nil {
		return nil, nil
	}

	filters := []string{
		containers.LabelCreatedBy + "=" + containers.LabelCreatedByValue,
		containers.LabelKind + "=" + containers.KindStatefulset,
		containers.LabelName + "=" + name,
	}

	if scope != "" {
		filters = append(filters, containers.LabelScope+"="+scope)
	}

	return h.Containers.ListVolumesByLabels(filters)
}

// PruneVolumes drops every volume claimed by this statefulset
// (across all ordinals). Wired into `vd delete --prune` so the
// operator opts into data destruction explicitly. Volumes belong
// to the (scope, name) tuple — every claim, every ordinal that
// ever existed (including those scaled down out of) gets the axe.
//
// Returns the names of volumes actually removed for surface in
// the CLI's confirmation log. Errors are best-effort: a volume
// stuck in use by an external container surfaces an error but
// the prune loop continues with the rest.
func (h *StatefulsetHandler) PruneVolumes(scope, name string) ([]string, error) {
	if h.Containers == nil {
		return nil, nil
	}

	filters := []string{
		containers.LabelCreatedBy + "=" + containers.LabelCreatedByValue,
		containers.LabelKind + "=" + containers.KindStatefulset,
		containers.LabelName + "=" + name,
	}

	if scope != "" {
		filters = append(filters, containers.LabelScope+"="+scope)
	}

	vols, err := h.Containers.ListVolumesByLabels(filters)
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}

	removed := make([]string, 0, len(vols))

	for _, v := range vols {
		if err := h.Containers.RemoveVolume(v); err != nil {
			h.logf("statefulset/%s-%s prune volume %s failed: %v", scope, name, v, err)
			continue
		}

		removed = append(removed, v)
	}

	return removed, nil
}
