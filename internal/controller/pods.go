package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.voodu.clowk.in/internal/containers"
	"go.voodu.clowk.in/internal/docker"
)

// Pod is a controller-side, kind-aware view of a single voodu-managed
// container. Returned by /pods so `voodu get pods` can render a table
// without re-deriving labels client-side.
//
// Identity matches the structured voodu.* labels stamped at create
// time; Status mirrors `docker ps` columns trimmed to what an operator
// actually scans (running vs stopped, the runtime status string,
// when it was created).
//
// "Pod" here is a deliberate borrow from k8s parlance to give
// operators a familiar mental model — it does NOT imply a sidecar
// model or shared network namespace. A voodu pod is one container
// the controller knows about.
type Pod struct {
	Name string `json:"name"`

	Kind         string `json:"kind"`
	Scope        string `json:"scope,omitempty"`
	ResourceName string `json:"resource_name"`
	ReplicaID    string `json:"replica_id,omitempty"`

	// Role is the high-level category from voodu.role label.
	// Defaults to Kind when set; specific paths override (e.g.
	// "release", "backup"). `vd get pd` groups output by this
	// value.
	Role string `json:"role,omitempty"`

	// ReleaseID correlates this pod to the deployment-release
	// record it was spawned from. Empty when the pod was created
	// outside a release orchestration (initial replica creation,
	// non-release-block deployments). Renderers display "-" in
	// that case.
	ReleaseID string `json:"release_id,omitempty"`

	Image     string `json:"image"`
	Status    string `json:"status"`
	Running   bool   `json:"running"`
	CreatedAt string `json:"created_at,omitempty"`
}

// PodsLister enumerates voodu-managed containers on the host. Behind
// an interface so tests can stub the docker call without faking the
// CLI itself, and so a future remote-node aggregator can swap the
// production lister for a multi-host implementation.
type PodsLister interface {
	ListPods() ([]Pod, error)
}

// DegradedResource is a deployment / statefulset whose latest
// reconcile attempt failed. Surfaced alongside the pods list so
// operators see "this resource exists in the manifest but isn't
// running because of X" without having to cross-reference describe
// for every kind they applied.
//
// Populated server-side from DeploymentStatus.LastReconcileError —
// only kinds that share DeploymentStatus participate today
// (deployment, statefulset). When the error clears on the next
// successful reconcile, the entry drops from the next /pods response.
type DegradedResource struct {
	Kind       string `json:"kind"`
	Scope      string `json:"scope,omitempty"`
	Name       string `json:"name"`
	Reason     string `json:"reason"`
	At         string `json:"at,omitempty"`         // RFC3339 timestamp of the reconcile attempt
	ReplicaIDs []string `json:"replica_ids,omitempty"` // running replicas of this resource (may be empty)
}

// DockerPodsLister is the production implementation. Lists every
// container labeled createdby=voodu, then re-inspects each one to
// recover its structured identity. Pre-M0 containers (umbrella label
// only, no voodu.kind) come back with empty Kind — the renderer
// surfaces those under "legacy" so operators can see what still
// needs a re-apply to migrate.
type DockerPodsLister struct{}

func (DockerPodsLister) ListPods() ([]Pod, error) {
	infos, err := docker.ListContainers(true)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]Pod, 0, len(infos))

	for _, c := range infos {
		raw := c.Names
		if raw == "" {
			raw = c.Name
		}

		name := strings.TrimPrefix(strings.TrimSpace(raw), "/")
		if name == "" {
			continue
		}

		labels, err := docker.InspectLabels(name)
		if err != nil {
			// Inspection failure on one container shouldn't poison
			// the whole listing — log-and-skip is the safer choice
			// during a partial docker outage. Skip silently so the
			// next refresh picks it up.
			continue
		}

		id, _ := containers.ParseLabels(labels)

		running, _ := docker.IsRunning(name)

		out = append(out, Pod{
			Name:         name,
			Kind:         id.Kind,
			Scope:        id.Scope,
			ResourceName: id.Name,
			ReplicaID:    id.ReplicaID,
			ReleaseID:    id.ReleaseID,
			Role:         id.Role,
			Image:        c.Image,
			Status:       c.Status,
			Running:      running,
			CreatedAt:    id.CreatedAt,
		})
	}

	sortPods(out)

	return out, nil
}

// PodDetail is the rich-inspect view returned by GET /pods/{name} —
// the runtime counterpart to the manifest+status `voodu describe`
// renders for declared resources. Embeds Pod so the basic identity
// columns the CLI already knows how to render are reused, and adds
// the docker-inspect fields (state details, networks, mounts, env,
// command) that only make sense per-replica.
type PodDetail struct {
	Pod

	// State carries the precise running/exited bookkeeping including
	// exit code and start/finish timestamps. The Pod.Running field is
	// a duplicate of State.Running kept for table-rendering ergonomics.
	State docker.ContainerState `json:"state"`

	Command    []string                           `json:"command,omitempty"`
	Entrypoint []string                           `json:"entrypoint,omitempty"`
	WorkingDir string                             `json:"working_dir,omitempty"`
	Env        map[string]string                  `json:"env,omitempty"`
	Labels     map[string]string                  `json:"labels,omitempty"`
	Networks   map[string]docker.ContainerNetwork `json:"networks,omitempty"`
	Mounts     []docker.ContainerMount            `json:"mounts,omitempty"`
	Ports      []docker.ContainerPort             `json:"ports,omitempty"`

	RestartPolicy string `json:"restart_policy,omitempty"`

	// ID is the docker container ID — useful for `docker logs <id>`
	// when the operator wants to escape voodu and debug at the daemon
	// level.
	ID string `json:"id,omitempty"`

	// Stats carries the live CPU/memory snapshot joined from the
	// StatsCollector when /pods?detail=true is called against a
	// controller that has stats wired. nil (and omitted from JSON)
	// in three cases:
	//
	//   1. detail=false (the compact list never carries stats)
	//   2. controller wasn't built with a StatsCollector (test
	//      setups, plugins running in isolation)
	//   3. this specific pod wasn't in the docker stats batch (race
	//      with delete, container transitioning, orphan filtered
	//      out by the collector)
	//
	// Callers reading detail responses MUST handle nil — the field's
	// presence is a hint, not a guarantee.
	Stats *PodStatsSnapshot `json:"stats,omitempty"`
}

// PodDescriber is the seam GET /pods/{name} dispatches through. The
// production implementation is DockerPodsLister (its GetPod method
// delegates to docker.InspectContainer); tests substitute a fake to
// avoid shelling out.
//
// Returns (nil, nil) when the named container doesn't exist — distinct
// from (nil, err) which is a real inspect failure (docker daemon down,
// permissions, etc.). The handler maps the nil-detail case to 404.
type PodDescriber interface {
	GetPod(name string) (*PodDetail, error)
}

// GetPod fetches the rich inspect view of a single voodu-managed
// container. Returns (nil, nil) when the container isn't on this
// host — operators sometimes typo a name and the 404 is much friendlier
// than a stack trace.
//
// The container does NOT need to carry voodu labels — `voodu describe
// pod` is also useful as an escape hatch for diagnosing legacy
// containers that pre-date the structured-label rollout. When labels
// are present we surface the parsed identity; when they aren't, the
// embedded Pod fields are left at their zero values and the operator
// still gets the rich state/network/mount detail.
func (DockerPodsLister) GetPod(name string) (*PodDetail, error) {
	det, err := docker.InspectContainer(name)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", name, err)
	}

	if det == nil {
		return nil, nil
	}

	out := &PodDetail{
		Pod: Pod{
			Name:    det.Name,
			Image:   det.Image,
			Status:  det.State.Status,
			Running: det.State.Running,
		},
		ID:            det.ID,
		State:         det.State,
		Command:       det.Command,
		Entrypoint:    det.Entrypoint,
		WorkingDir:    det.WorkingDir,
		Env:           det.Env,
		Labels:        det.Labels,
		Networks:      det.Networks,
		Mounts:        det.Mounts,
		Ports:         det.Ports,
		RestartPolicy: det.RestartPolicy,
	}

	// Layer on the structured voodu identity when present. Pre-M0 /
	// non-voodu containers fall through with empty identity fields —
	// the renderer surfaces that as "(no voodu identity labels)".
	if id, ok := containers.ParseLabels(det.Labels); ok {
		out.Pod.Kind = id.Kind
		out.Pod.Scope = id.Scope
		out.Pod.ResourceName = id.Name
		out.Pod.ReplicaID = id.ReplicaID
		out.Pod.CreatedAt = id.CreatedAt
		out.Pod.Role = id.Role
	}

	return out, nil
}

// sortPods orders pods so the CLI renders a deterministic table:
// scope first (with empty scope last so unscoped kinds group at the
// bottom), then resource name, then replica id. Anything missing a
// kind sinks to the end so legacy containers are visually distinct
// from the structured M0+ rows.
func sortPods(pods []Pod) {
	sort.SliceStable(pods, func(i, j int) bool {
		a, b := pods[i], pods[j]

		// Pods missing a kind label are pre-M0 / non-voodu — push to
		// the bottom so the structured rows render cleanly first.
		if (a.Kind == "") != (b.Kind == "") {
			return a.Kind != ""
		}

		if a.Scope != b.Scope {
			// Unscoped (empty scope) sinks below scoped rows so the
			// scope-grouping reads top-to-bottom alphabetically.
			if a.Scope == "" {
				return false
			}

			if b.Scope == "" {
				return true
			}

			return a.Scope < b.Scope
		}

		if a.ResourceName != b.ResourceName {
			return a.ResourceName < b.ResourceName
		}

		return a.ReplicaID < b.ReplicaID
	})
}

// enrichPodsConcurrency caps the in-flight docker inspects when the
// /pods?detail=true handler is fanning out. Picked as the sweet spot
// between "all serial" (slow on a 100-pod host) and "all parallel"
// (docker daemon goes unhappy past ~16 concurrent inspects on small
// VMs). Pure number — change if you ever profile and find a better
// knee.
const enrichPodsConcurrency = 8

// enrichPods turns a list of compact Pod records into the rich
// PodDetail shape, in parallel. Used by GET /pods?detail=true so the
// CLI and WebUI don't have to N+1 their way through /pods/{name}
// client-side — the server pays the inspect cost once and ships the
// joined response.
//
// Semantics:
//
//   - Best-effort: an inspect failure on one pod degrades that slot
//     to "just the basic Pod info" (the embedded Pod fields stay
//     populated, the rich extras are zero-valued and omit from
//     JSON via `omitempty`). The list as a whole still returns
//     and downstreams can render partial data.
//   - Order preserved: outputs[i] always corresponds to inputs[i].
//   - Bounded concurrency: at most `enrichPodsConcurrency` inspects
//     in flight, so a hundred-pod host doesn't fork a hundred
//     `docker inspect` children at once.
//   - Stats join: when `stats` is non-nil, enrichPods asks for a
//     single docker stats batch (covering ALL running pods on the
//     host — fixed cost regardless of N matches) and attaches the
//     per-pod CPU/Mem usage + declared limits to each output's
//     `.Stats` field. Failures or non-matching containers leave
//     `.Stats` nil; the JSON omits it via omitempty.
//
// describer is the same PodDescriber interface GET /pods/{name}
// uses; stats is the optional StatsCollector — passing both
// explicitly keeps this function testable without the api.go
// plumbing.
func enrichPods(pods []Pod, describer PodDescriber, stats *StatsCollector) []PodDetail {
	out := make([]PodDetail, len(pods))
	sem := make(chan struct{}, enrichPodsConcurrency)

	// Stats fan-in: one docker stats call before the per-pod inspect
	// loop. Daemon returns every running container's CPU/Mem in a
	// single sample, so the cost is fixed at one daemon call no
	// matter how many pods we're enriching. We pre-build the
	// name→snapshot map so the inspect goroutines just do an O(1)
	// lookup.
	statsByName := collectStatsByName(stats)

	var wg sync.WaitGroup
	for i := range pods {
		i := i
		p := pods[i]
		wg.Add(1)

		sem <- struct{}{} // acquire slot
		go func() {
			defer wg.Done()
			defer func() { <-sem }() // release slot

			det, err := describer.GetPod(p.Name)
			if err != nil || det == nil {
				// Inspect failed (daemon hiccup, race with delete) or
				// the container vanished between list and inspect.
				// Fall back to the compact Pod so the row still
				// renders something useful.
				out[i] = PodDetail{Pod: p}
				attachStats(&out[i], statsByName)
				return
			}

			// describer.GetPod re-derives identity from labels and may
			// leave Scope/ResourceName/ReplicaID empty for pre-M0
			// containers. The list-time identity (already in `p`) is
			// the trustworthy one, so layer it back on top.
			det.Pod.Kind = p.Kind
			det.Pod.Scope = p.Scope
			det.Pod.ResourceName = p.ResourceName
			det.Pod.ReplicaID = p.ReplicaID
			det.Pod.ReleaseID = p.ReleaseID
			det.Pod.Role = p.Role
			det.Pod.CreatedAt = p.CreatedAt

			out[i] = *det
			attachStats(&out[i], statsByName)
		}()
	}

	wg.Wait()

	return out
}

// collectStatsByName returns a name → snapshot lookup map for the
// enrichment loop. We READ from the StatsCollector's cached
// in-memory snapshot (refreshed every ~15s by the metrics sampler)
// instead of shelling out to `docker stats` on every /pods?detail=true
// request — that endpoint is polled aggressively by the WebUI and the
// per-request docker call dominated controller CPU.
//
// Fallback: when the snapshot hasn't been populated yet (first-boot,
// pre-first-sampler-tick) we fall back to a live Collect so detail
// mode still works during the warmup window. nil collector or any
// error degrades to nil; the caller's `attachStats` then leaves every
// PodDetail.Stats nil and the JSON omits it.
//
// We ask for Orphans=true so containers without voodu labels still
// surface numbers — better to show a real CPU% than to drop the row.
func collectStatsByName(stats *StatsCollector) map[string]PodStatsSnapshot {
	if stats == nil {
		return nil
	}

	rows, _, ok := stats.SnapshotPods(StatsFilter{Orphans: true})
	if !ok {
		// Snapshot not populated yet — fall back to live Collect so
		// detail mode works during the warmup window before the
		// sampler's first tick.
		live, err := stats.Collect(context.Background(), StatsFilter{Orphans: true})
		if err != nil || len(live) == 0 {
			return nil
		}

		rows = live
	}

	if len(rows) == 0 {
		return nil
	}

	out := make(map[string]PodStatsSnapshot, len(rows))
	for _, r := range rows {
		out[r.ContainerName] = PodStatsSnapshot{Usage: r.Usage, Limits: r.Limits}
	}

	return out
}

// attachStats writes the matching snapshot into pd.Stats if the map
// has a row for pd.Name. No-op when the map is nil/empty (collector
// off or failed) or when the pod's name isn't represented (orphan
// stopped between list and stats sample). Caller already wrote the
// embedded Pod identity, so we don't touch anything else.
func attachStats(pd *PodDetail, statsByName map[string]PodStatsSnapshot) {
	if statsByName == nil {
		return
	}

	if snap, ok := statsByName[pd.Name]; ok {
		snap := snap
		pd.Stats = &snap
	}
}
