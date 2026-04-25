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
			Image:        c.Image,
			Status:       c.Status,
			Running:      running,
			CreatedAt:    id.CreatedAt,
		})
	}

	sortPods(out)

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
