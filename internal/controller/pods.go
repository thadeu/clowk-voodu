package controller

import (
	"fmt"
	"sort"
	"strings"

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
