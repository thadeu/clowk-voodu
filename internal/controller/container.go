package controller

import (
	"fmt"
	"strings"

	"go.voodu.clowk.in/internal/docker"
	"go.voodu.clowk.in/internal/paths"
)

// ContainerManager is the reconciler's view of the host's container
// runtime. Kept as an interface so tests can stub it and so the
// docker-CLI implementation stays swappable (podman, nerdctl) without
// touching handlers. The handler owns *when* to spawn/restart; the
// manager owns *how*.
type ContainerManager interface {
	Exists(name string) (bool, error)

	// Ensure creates the container when it doesn't already exist.
	// Returns true when it actually created one — the handler uses
	// that to skip a redundant restart right after spawn.
	Ensure(spec ContainerSpec) (created bool, err error)

	// Restart recreates the currently-active container so the updated
	// env file takes effect. Safe to call even on a just-ensured
	// container (idempotent), but callers should avoid redundant calls.
	Restart(name string) error

	// Image returns the image tag of the running container named `name`,
	// or "" if none. Used by the drift detector — if the manifest's
	// Image differs, the handler asks for a Recreate.
	Image(name string) (string, error)

	// ImageIDsDiffer reports whether the container was created from a
	// different image ID than the tag currently resolves to. Build-mode
	// rewrites `<app>:latest` on every push; the tag stays the same
	// but the image ID underneath changes. Comparing IDs is the only
	// way to detect "rebuild happened, container still on old image"
	// — spec-hash drift can't see it because the manifest text is
	// identical.
	//
	// Returns (false, nil) for any ambiguous state (container missing,
	// image not locally resolvable) so the caller can fall back to
	// the spec-hash path without special-casing "unknown".
	ImageIDsDiffer(container, tag string) (bool, error)

	// Recreate stops-and-removes the existing container (if any) and
	// starts a fresh one from spec. Distinct from Ensure because we
	// want a *different* image/runtime config, not a no-op.
	Recreate(spec ContainerSpec) error

	// Remove stops and deletes the named container. Used by the
	// reconciler on scale-down (slots above the desired replica count)
	// and to prune legacy non-indexed containers after the switch to
	// `<app>-<index>` naming. Safe to call on a non-existent name.
	Remove(name string) error

	// ListByAppPrefix returns every voodu-managed container name whose
	// name is exactly `<app>` or `<app>-<N>` — i.e. the legacy flat
	// name plus the new indexed slots. Any other container (e.g.
	// `<app>-foo` for a sidecar that shares the prefix accidentally)
	// is filtered out, so callers can scale down / prune without fear
	// of touching unrelated containers.
	ListByAppPrefix(app string) ([]string, error)
}

// ContainerSpec is the subset of a deployment manifest the manager
// needs. It is deliberately a narrow wire type — the handler decodes
// the full manifest and hands over only what the runtime cares about.
type ContainerSpec struct {
	Name    string
	Image   string
	Command []string
	Ports   []string
	Volumes []string

	// Networks is the set of docker bridges the container joins. The
	// first entry is used as the creation-time --network (docker run
	// only accepts one); subsequent entries are attached post-create
	// via `docker network connect`. Handler guarantees at least one in
	// bridge mode.
	Networks []string

	// NetworkMode, when set to "host" or "none", bypasses docker's
	// bridge stack entirely and takes precedence over Networks. The
	// handler validates mutual exclusivity so the manager can trust
	// the two fields to be consistent.
	NetworkMode string

	Restart string

	// EnvFile points at the app's .env file written by secrets.Set.
	// The manager passes it to docker via --env-file so env changes
	// only require restart, not recreate.
	EnvFile string
}

// DockerContainerManager is the production ContainerManager backed by
// the existing internal/docker package. It intentionally does NOT use
// docker.DeployContainer: that path is shaped for the git-push build
// flow (hardcoded image name, /app working dir, release volumes). The
// manifest-driven path runs pre-built images from a registry and needs
// a plainer CreateContainer call.
type DockerContainerManager struct{}

func (DockerContainerManager) Exists(name string) (bool, error) {
	return docker.ContainerExists(name), nil
}

// Ensure brings the container up. Three cases:
//
//  1. Container missing → create it.
//  2. Container exists and is running → no-op (the common steady-state
//     replay case; returns created=false so the handler skips a
//     redundant restart).
//  3. Container exists but is stopped → start it. Host reboots with a
//     restart="no" policy land here, as do old containers created
//     before voodu started defaulting to "unless-stopped".
//
// In case 3 we also repair the restart policy in place so the NEXT
// reboot survives without needing a manual apply. `docker update
// --restart` is cheap and idempotent, so we run it whenever the spec
// declares a policy — no drift detection, the reconciler is the
// source of truth.
func (DockerContainerManager) Ensure(spec ContainerSpec) (bool, error) {
	if docker.ContainerExists(spec.Name) {
		if spec.Restart != "" {
			// Non-fatal: an operator running an ancient docker might not
			// support `update --restart`. Log-only fits the reconciler's
			// "best-effort on replay" philosophy — the container is
			// already there, we just couldn't improve its future.
			_ = docker.UpdateRestartPolicy(spec.Name, spec.Restart)
		}

		running, err := docker.IsRunning(spec.Name)
		if err != nil {
			return false, fmt.Errorf("inspect %s: %w", spec.Name, err)
		}

		if !running {
			if err := docker.StartContainer(spec.Name); err != nil {
				return false, fmt.Errorf("start stopped container %s: %w", spec.Name, err)
			}
		}

		return false, nil
	}

	cfg := docker.ContainerConfig{
		Name:          spec.Name,
		Image:         spec.Image,
		Command:       spec.Command,
		Ports:         spec.Ports,
		Volumes:       spec.Volumes,
		NetworkMode:   spec.NetworkMode,
		Networks:      spec.Networks,
		RestartPolicy: spec.Restart,
		EnvFile:       spec.EnvFile,
	}

	if err := docker.CreateContainer(cfg); err != nil {
		return false, err
	}

	return true, nil
}

func (DockerContainerManager) Restart(name string) error {
	return docker.RecreateActiveContainer(name, paths.AppEnvFile(name), paths.AppCurrentLink(name))
}

func (DockerContainerManager) Image(name string) (string, error) {
	if !docker.ContainerExists(name) {
		return "", nil
	}

	return docker.GetContainerImage(name)
}

func (DockerContainerManager) ImageIDsDiffer(container, tag string) (bool, error) {
	containerID, err := docker.GetContainerImageID(container)
	if err != nil || containerID == "" {
		return false, err
	}

	tagID, err := docker.GetImageID(tag)
	if err != nil || tagID == "" {
		// Image not locally resolvable (not yet pulled/built). Caller
		// falls back to spec-hash — treating "can't check" as "no
		// drift" avoids spurious recreates on startup races.
		return false, nil
	}

	return containerID != tagID, nil
}

func (DockerContainerManager) Remove(name string) error {
	if !docker.ContainerExists(name) {
		return nil
	}

	if err := docker.StopContainer(name); err != nil {
		return err
	}

	return docker.RemoveContainer(name, true)
}

// ListByAppPrefix scans voodu-labeled containers and returns those whose
// docker name is either the bare app name (legacy pre-slot) or the
// indexed `<app>-<N>` shape. Names are normalised to drop the leading
// '/' docker prepends in its JSON output.
func (DockerContainerManager) ListByAppPrefix(app string) ([]string, error) {
	containers, err := docker.ListContainers(true)
	if err != nil {
		return nil, err
	}

	var out []string

	for _, c := range containers {
		raw := c.Names
		if raw == "" {
			raw = c.Name
		}

		name := strings.TrimPrefix(strings.TrimSpace(raw), "/")
		if name == "" {
			continue
		}

		if name == app {
			out = append(out, name)
			continue
		}

		if rest, ok := strings.CutPrefix(name, app+"-"); ok {
			if isAllDigits(rest) {
				out = append(out, name)
			}
		}
	}

	return out, nil
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

func (DockerContainerManager) Recreate(spec ContainerSpec) error {
	if docker.ContainerExists(spec.Name) {
		if err := docker.StopContainer(spec.Name); err != nil {
			return err
		}

		if err := docker.RemoveContainer(spec.Name, true); err != nil {
			return err
		}
	}

	cfg := docker.ContainerConfig{
		Name:          spec.Name,
		Image:         spec.Image,
		Command:       spec.Command,
		Ports:         spec.Ports,
		Volumes:       spec.Volumes,
		NetworkMode:   spec.NetworkMode,
		Networks:      spec.Networks,
		RestartPolicy: spec.Restart,
		EnvFile:       spec.EnvFile,
	}

	return docker.CreateContainer(cfg)
}
