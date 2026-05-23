// stats.go is the controller-side join of two data sources:
//
//   1. Live runtime usage from docker (CPU%, mem bytes) via
//      internal/docker.StatsClient — cgroup-accurate, sampled fresh
//      on every call.
//   2. Configured limits from the manifest store (resources.limits
//      block) — the operator's declared intent at apply time.
//
// The point of joining them in one shape (PodStats) is so callers —
// `vd stats`, the future SDK, dashboards, alerting — see "is this pod
// approaching its limit?" without two roundtrips and two filters.
//
// Keep this package free of CLI concerns (no flags, no rendering).
// Public surface is StatsCollector + the typed result; everything
// else (HTTP endpoint, text table) wraps this.

package controller

import (
	"context"
	"encoding/json"
	"strings"

	"go.voodu.clowk.in/internal/docker"
)

// PodStats is the joined runtime+configured view of one running
// container. Fields are deliberately nullable-friendly (zero values
// are meaningful) so the JSON wire shape stays clean across the
// orphan / unbounded-limit / partial-data cases.
type PodStats struct {
	// Identity carries the structured voodu.* labels — kind, scope,
	// name, replica id. For orphan pods (no labels, or no matching
	// manifest), Identity is best-effort populated from whatever the
	// container reveals; the Orphan flag is the canonical signal.
	Identity StatsIdentity `json:"identity"`

	// ContainerName is the docker container name (without leading
	// slash). The natural key when correlating with `docker logs` or
	// `vd logs`.
	ContainerName string `json:"container_name"`

	// Usage is the live runtime measurement — what the container is
	// consuming RIGHT NOW. Always populated for running pods (we
	// filter stopped ones out of the result entirely).
	Usage UsageStats `json:"usage"`

	// Limits is the manifest's declared resources.limits, parsed and
	// normalised. Empty/zero when no `resources {}` block was
	// declared, or when this pod is an orphan (no manifest).
	Limits LimitStats `json:"limits"`

	// DesiredReplicas mirrors the manifest's replicas field at the
	// time of the call. Zero when the kind doesn't model replicas
	// (job, cronjob, asset, ingress) or the pod is orphan (no
	// manifest to read from). Same value across siblings of one
	// resource — callers aggregating per (kind, scope, name) can
	// trust any pod in the group to carry the right number.
	DesiredReplicas int `json:"desired_replicas,omitempty"`

	// Orphan is true when this pod can't be joined back to a manifest
	// — either it lacks voodu identity labels (pre-M0 legacy) or the
	// manifest was deleted while the container kept running (leak).
	// The CLI's --orphans flag controls whether these are surfaced.
	Orphan bool `json:"orphan,omitempty"`
}

// StatsIdentity mirrors the structured voodu.* labels in the
// containers package, but lives here so the wire shape is local to
// the stats API (no cyclic concerns) and we control which fields
// the JSON contract exposes.
type StatsIdentity struct {
	Kind      string `json:"kind"`
	Scope     string `json:"scope,omitempty"`
	Name      string `json:"name"`
	ReplicaID string `json:"replica_id,omitempty"`
}

// UsageStats carries the runtime numbers. Mirrors docker.ContainerStats
// minus the raw line — exposing only what callers consume.
//
// MemoryLimitBytes is docker's view of the cgroup limit. When the
// operator didn't set a limit, docker reports the host's total
// memory — semantically "unbounded." The Limits field below is the
// authoritative source for "what was configured", because it comes
// from the manifest, not docker's runtime view.
type UsageStats struct {
	CPUPercent       float64 `json:"cpu_percent"`
	MemoryUsageBytes uint64  `json:"memory_usage_bytes"`
	MemoryLimitBytes uint64  `json:"memory_limit_bytes"`
	MemoryPercent    float64 `json:"memory_percent"`
	PIDs             int     `json:"pids,omitempty"`
}

// LimitStats is the manifest-declared resource ceiling. CPU and
// Memory carry the operator's verbatim string ("0.4", "500m", "254Mi")
// so the renderer can echo what was written; MemoryBytes is the
// parsed numeric for programmatic comparison.
//
// All-zero/empty means no `resources {}` block was declared.
type LimitStats struct {
	CPU         string `json:"cpu,omitempty"`
	Memory      string `json:"memory,omitempty"`
	MemoryBytes int64  `json:"memory_bytes,omitempty"`
}

// StatsFilter narrows the result set. Empty filter = all running
// pods (minus orphans unless Orphans is true). Matches the field
// vocabulary of /pods so a future caller can pass through the same
// query params.
type StatsFilter struct {
	Kind  string
	Scope string
	Name  string

	// Orphans, when true, includes containers that can't be joined
	// to a manifest (no voodu.kind label OR manifest deleted but
	// container running). Default false matches the steady-state
	// expectation that every running container is managed.
	Orphans bool
}

// StatsCollector is the wire-up of pod identity + docker runtime
// stats + manifest limits. Production wires (Pods=DockerPodsLister,
// Stats=docker.DockerStatsClient, Store=EtcdStore); tests inject
// fakes for each of the three to assert join behaviour without
// docker on the box.
type StatsCollector struct {
	// Pods enumerates voodu-managed (and legacy) containers on the
	// host. Same seam /pods uses. Returns Pod entries with
	// kind/scope/name parsed from labels.
	Pods PodsLister

	// Stats fetches live CPU/mem/IO from the docker daemon. Single-
	// shot; the collector calls once per request. Tests pass a fake
	// that returns canned ContainerStats keyed by name.
	Stats docker.StatsClient

	// Store is the manifest source for limit lookups. Nil-tolerant:
	// when nil (rare — tests that don't care about limits), all
	// results have empty Limits and are marked Orphan=true.
	Store Store
}

// Collect joins pods + docker stats + manifest limits and returns
// the filtered list. Order is whatever Pods.ListPods returned
// (already stable per sortPods on the production path), so the CLI
// can render the table directly.
//
// Errors:
//   - Pods.ListPods failure → error (can't proceed without identity)
//   - Stats.ContainerStats failure → error (no runtime data is no result)
//   - Per-pod manifest lookup failures are NOT fatal — the pod is
//     marked Orphan and surfaces only when filter.Orphans is true.
//     A controller hiccup on one key shouldn't blank the whole table.
func (c *StatsCollector) Collect(ctx context.Context, filter StatsFilter) ([]PodStats, error) {
	pods, err := c.Pods.ListPods()
	if err != nil {
		return nil, err
	}

	// Filter first (cheap, in-memory) so we only ask docker for
	// stats on the names that match. Reduces the daemon's CPU
	// sampling work proportionally on hosts with many containers.
	pods = filterPods(pods, filter)
	if len(pods) == 0 {
		return nil, nil
	}

	names := make([]string, 0, len(pods))

	for _, p := range pods {
		if !p.Running {
			// Only-running per design — stopped pods have no cgroup
			// to sample, including them as "0% CPU" would be
			// misleading rather than informative.
			continue
		}

		names = append(names, p.Name)
	}

	if len(names) == 0 {
		return nil, nil
	}

	rawStats, err := c.Stats.ContainerStats(names)
	if err != nil {
		return nil, err
	}

	statsByName := make(map[string]docker.ContainerStats, len(rawStats))
	for _, s := range rawStats {
		statsByName[s.Name] = s
	}

	out := make([]PodStats, 0, len(names))

	for _, p := range pods {
		if !p.Running {
			continue
		}

		runtime, ok := statsByName[p.Name]
		if !ok {
			// Pod appeared in ListPods (running=true) but docker
			// stats didn't return it. Common race: container
			// transitioned mid-call. Skip rather than emit a
			// zeroed row — the next refresh will catch it.
			continue
		}

		meta, manifestFound := c.lookupManifestMeta(ctx, p)

		// Orphan = container lacks structured identity OR the
		// referenced manifest was deleted. Both are conditions an
		// operator should be able to see (--orphans) but shouldn't
		// be the default (cluttered table with mostly-junk).
		orphan := p.Kind == "" || !manifestFound

		if orphan && !filter.Orphans {
			continue
		}

		out = append(out, PodStats{
			Identity: StatsIdentity{
				Kind:      p.Kind,
				Scope:     p.Scope,
				Name:      p.ResourceName,
				ReplicaID: p.ReplicaID,
			},
			ContainerName: p.Name,
			Usage: UsageStats{
				CPUPercent:       runtime.CPUPercent,
				MemoryUsageBytes: runtime.MemUsageBytes,
				MemoryLimitBytes: runtime.MemLimitBytes,
				MemoryPercent:    runtime.MemPercent,
				PIDs:             runtime.PIDs,
			},
			Limits:          meta.Limits,
			DesiredReplicas: meta.Desired,
			Orphan:          orphan,
		})
	}

	return out, nil
}

// filterPods applies kind/scope/name narrowing. Exported as a free
// function (not a method) so the same shape can drive /pods if we
// ever DRY that handler against this one.
//
// Empty filter fields are wildcards. The OR-of-empties model
// matches how /pods already works — operators can build up filters
// incrementally without flag interaction surprises.
//
// Returns a NEW slice — never mutates the caller's backing array.
// This matters because PodsLister implementations are free to share
// the slice they return across calls (the test fake does); a
// pods[:0]-style in-place filter would corrupt that shared state
// on the second invocation.
func filterPods(pods []Pod, f StatsFilter) []Pod {
	if f.Kind == "" && f.Scope == "" && f.Name == "" {
		return pods
	}

	out := make([]Pod, 0, len(pods))

	for _, p := range pods {
		if f.Kind != "" && p.Kind != f.Kind {
			continue
		}

		if f.Scope != "" && p.Scope != f.Scope {
			continue
		}

		if f.Name != "" && p.ResourceName != f.Name {
			continue
		}

		out = append(out, p)
	}

	return out
}

// manifestMeta packs the fields lookupManifestMeta extracts from the
// manifest in one call — avoids two store roundtrips and keeps the
// "what does the manifest declare for this pod?" question on a
// single seam.
type manifestMeta struct {
	Limits  LimitStats
	Desired int
}

// lookupManifestMeta fetches the manifest for one pod and extracts
// its resources.limits + replicas count. Returns (zero, false) when:
//
//   - Store is nil (test wiring without a store)
//   - Pod has no kind label (legacy / orphan)
//   - Manifest not found (deleted while container kept running)
//   - Manifest exists but has no resources block (Limits empty; the
//     bool is still true so the pod isn't flagged orphan)
//
// The bool signals "we found a manifest" specifically — empty
// limits with a found manifest is NOT an orphan, it's a valid
// "operator declared no caps" state.
func (c *StatsCollector) lookupManifestMeta(ctx context.Context, p Pod) (manifestMeta, bool) {
	if c.Store == nil || p.Kind == "" {
		return manifestMeta{}, false
	}

	kind, err := ParseKind(p.Kind)
	if err != nil {
		return manifestMeta{}, false
	}

	m, err := c.Store.Get(ctx, kind, p.Scope, p.ResourceName)
	if err != nil || m == nil {
		return manifestMeta{}, false
	}

	return manifestMeta{
		Limits:  extractLimits(kind, m.Spec),
		Desired: extractDesiredReplicas(kind, m.Spec),
	}, true
}

// extractDesiredReplicas reads the manifest's `replicas` field for
// kinds that model it (deployment, statefulset). Returns 0 for
// kinds without a replicas concept (job, cronjob, asset, ingress) —
// the CLI uses that to decide whether to render "running/desired"
// or just "running" in the REPLICAS column.
func extractDesiredReplicas(kind Kind, spec json.RawMessage) int {
	if kind != KindDeployment && kind != KindStatefulset {
		return 0
	}

	var r struct {
		Replicas int `json:"replicas,omitempty"`
	}

	if err := json.Unmarshal(spec, &r); err != nil {
		return 0
	}

	return r.Replicas
}

// extractLimits decodes the manifest spec just deep enough to find
// `resources.limits` regardless of which kind owns it. Deployment /
// statefulset / job have it at root; cronjob has it nested under
// `job`. We use a permissive map[string]any decode so this stays
// resilient to future spec changes — we're not validating, just
// looking up two paths.
func extractLimits(kind Kind, spec json.RawMessage) LimitStats {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(spec, &raw); err != nil {
		return LimitStats{}
	}

	// Cronjob: dig into job.resources. Everything else: spec.resources.
	resourcesRaw, ok := raw["resources"]
	if !ok && kind == KindCronJob {
		jobRaw, jobOk := raw["job"]
		if !jobOk {
			return LimitStats{}
		}

		var jobMap map[string]json.RawMessage
		if err := json.Unmarshal(jobRaw, &jobMap); err != nil {
			return LimitStats{}
		}

		resourcesRaw, ok = jobMap["resources"]
	}

	if !ok {
		return LimitStats{}
	}

	var res struct {
		Limits *struct {
			CPU    string `json:"cpu,omitempty"`
			Memory string `json:"memory,omitempty"`
		} `json:"limits,omitempty"`
	}

	if err := json.Unmarshal(resourcesRaw, &res); err != nil {
		return LimitStats{}
	}

	if res.Limits == nil {
		return LimitStats{}
	}

	out := LimitStats{
		CPU:    strings.TrimSpace(res.Limits.CPU),
		Memory: strings.TrimSpace(res.Limits.Memory),
	}

	// Parse memory once so callers don't have to. CPU stays string
	// because the docker-runtime CPU% is host-relative — comparing
	// to a manifest "500m" needs context the caller renders, not a
	// direct numeric.
	if out.Memory != "" {
		if bytes, err := parseMemoryBytesForStats(out.Memory); err == nil {
			out.MemoryBytes = bytes
		}
	}

	return out
}

// parseMemoryBytesForStats wraps the manifest's k8svalues parser
// behind a name local to this file. We don't import k8svalues here
// to avoid pulling its full validation cost into the hot stats
// path; instead we delegate to the same parser the manifest layer
// uses (via the resources translation helper that's already in
// the controller package).
//
// The wrapper exists for one reason: the resources.go path uses
// (cpu, memBytes, err) and we only want memBytes here.
func parseMemoryBytesForStats(s string) (int64, error) {
	_, mem, err := dockerResources(&resourcesWireSpec{
		Limits: &resourceLimitsWireSpec{Memory: s},
	})

	return mem, err
}
