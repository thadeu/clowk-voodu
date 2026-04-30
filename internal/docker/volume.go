package docker

import (
	"fmt"
	"os/exec"
	"strings"
)

// EnsureVolume guarantees a named docker volume exists. Idempotent:
// `docker volume create <name>` succeeds whether or not the volume
// already exists, returning the name on stdout in both cases. Used
// by the statefulset handler to materialise per-pod claims before
// the pod boots.
//
// Labels are stamped on creation so `docker volume ls --filter
// label=createdby=voodu` enumerates voodu-managed volumes for
// describe / prune paths. The labels passed in here flow straight
// to docker — caller composes the (createdby, voodu.scope,
// voodu.name, voodu.claim, voodu.replica_ordinal) tuple.
//
// On an already-existing volume docker IGNORES the new labels
// (the create call returns the existing name without touching
// metadata). This is intentional: a re-apply with the same claim
// shape is a no-op, not a metadata update. Operators who need to
// re-tag a volume run `docker volume rm + create` manually.
func EnsureVolume(name string, labels []string) error {
	if name == "" {
		return fmt.Errorf("ensure volume: name is required")
	}

	args := []string{"volume", "create"}

	for _, lbl := range labels {
		args = append(args, "--label", lbl)
	}

	args = append(args, name)

	cmd := exec.Command("docker", args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker volume create %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}

	return nil
}

// RemoveVolume drops a named volume. Used by `vd delete --prune` on
// statefulsets to clean per-pod data after the operator opts in.
// Idempotent against a missing volume — docker returns exit 1 with
// "no such volume" which we swallow so the prune loop keeps moving.
//
// Errors are surfaced for everything else (volume in use by another
// container, daemon unreachable). The caller decides whether to
// abort the prune or log-and-continue.
func RemoveVolume(name string) error {
	if name == "" {
		return fmt.Errorf("remove volume: name is required")
	}

	out, err := exec.Command("docker", "volume", "rm", name).CombinedOutput()
	if err == nil {
		return nil
	}

	msg := strings.ToLower(strings.TrimSpace(string(out)))

	// "Error response from daemon: get : no such volume" — swallow.
	if strings.Contains(msg, "no such volume") {
		return nil
	}

	return fmt.Errorf("docker volume rm %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
}

// ListVolumesByLabels enumerates voodu-managed volumes filtered by
// a label set. Each filter is `key=value` — docker AND-combines
// multiple --filter args, so the result is the intersection.
//
// Returns just the names — the consumer can re-inspect for
// deeper metadata. Used by `vd describe statefulset/...` to show
// per-pod claims, and by the prune path to know what to remove.
func ListVolumesByLabels(filters []string) ([]string, error) {
	args := []string{"volume", "ls", "--quiet"}

	for _, f := range filters {
		args = append(args, "--filter", "label="+f)
	}

	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("docker volume ls: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")

	names := make([]string, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		names = append(names, line)
	}

	return names, nil
}
