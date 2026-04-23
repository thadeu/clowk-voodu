package controller

import (
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

func (DockerContainerManager) Ensure(spec ContainerSpec) (bool, error) {
	if docker.ContainerExists(spec.Name) {
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
