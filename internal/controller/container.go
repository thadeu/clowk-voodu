package controller

import (
	"fmt"
	"io"
	"strings"

	"go.voodu.clowk.in/internal/containers"
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

	// ListByIdentity returns every voodu-managed container whose
	// labels match the (kind, scope, name) tuple. Replaces the old
	// ListByAppPrefix which parsed names — labels are the source of
	// truth post-M0. The returned slice carries enough information
	// for callers to make decisions without a second docker call:
	// container name, replica id, image, running/stopped.
	//
	// Includes both running and stopped containers; the caller
	// filters as needed (the reconciler treats "stopped" as a slot
	// to be revived; `voodu get pods` shows everything).
	ListByIdentity(kind, scope, name string) ([]ContainerSlot, error)

	// ListLegacyByApp finds pre-M0 containers (no voodu.* labels)
	// whose docker name matches `<app>` or `<app>-<digits>`. Used by
	// the reconciler at apply time to detect containers from older
	// releases that need replacement under the new naming scheme.
	// Returns just the names since these have no structured identity
	// to surface.
	ListLegacyByApp(app string) ([]string, error)

	// Wait blocks until the named container exits and returns the
	// process exit code. Used by the job runner: jobs are containers
	// the controller spawns and then polls to completion. Returns an
	// error when the container can't be waited on (already removed,
	// docker daemon hiccup); callers treat that as a failed run.
	//
	// Long-running deployments don't call this — Wait would block
	// forever — but the interface keeps it on the same surface so a
	// future scheduler can dispatch via one ContainerManager handle.
	Wait(name string) (exitCode int, err error)

	// Logs returns a stream of stdout+stderr from the named container.
	// Works on running AND stopped containers (the docker json-file
	// driver keeps logs around until the container is removed). The
	// caller is responsible for Close()ing the returned reader to reap
	// the underlying docker process — leaving it open leaks a process.
	//
	// Used by the /pods/{name}/logs endpoint. Job and cronjob runs are
	// kept post-exit (no AutoRemove) so this is the operator's window
	// into "why did the last run fail?".
	Logs(name string, opts LogsOptions) (io.ReadCloser, error)

	// Exec runs a command inside an already-running container —
	// kubectl-exec semantics. Streams (stdin/stdout/stderr) come from
	// the caller via opts so the API handler can wire them to a
	// hijacked HTTP connection. Returns the child's exit code.
	Exec(name string, command []string, opts ExecOptions) (int, error)
}

// LogsOptions tunes the docker logs invocation. Both fields are
// optional — zero values mean "all logs, no follow".
type LogsOptions struct {
	// Follow asks for a streaming tail (`docker logs -f`). The reader
	// stays open until the container exits or the caller closes it.
	Follow bool

	// Tail caps the number of trailing lines returned. Zero means
	// unlimited (whole log). Negative values are treated as zero.
	Tail int
}

// ContainerSlot is a snapshot of one running deployment replica (or
// job/cronjob run) that the reconciler needs to make scaling and
// drift decisions. Pulled from `docker ps` + `docker inspect`. Kept
// narrow on purpose — adding a field here is a deliberate widening
// of the manager/handler contract.
type ContainerSlot struct {
	// Name is the docker container name (e.g. "softphone-web.a3f9").
	Name string

	// Identity is the parsed voodu.* labels. Always carries a Kind,
	// Scope, Name (those are the filter criteria); ReplicaID and
	// other fields are populated when present on the container.
	Identity containers.Identity

	// Image is the resolved tag (`<app>:latest` for build-mode,
	// pull-spec for registry-mode). Used by ingress upstream
	// resolution and drift detection.
	Image string

	// Running is true when the container is in the running state.
	// Distinct from Exists — stopped containers count as slots that
	// need to be started, not slots that need to be created.
	Running bool
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

	// Labels are the structured voodu.* identity flags appended to
	// docker run. Handler builds these from containers.Identity so
	// the next ListByIdentity call can find the container without
	// parsing names. Always includes voodu.kind/scope/name plus a
	// fresh voodu.replica_id (or run id for jobs).
	Labels []string

	// NetworkAliases are DNS names other containers can resolve to
	// reach this one over Docker's embedded resolver. Built by the
	// handler from (scope, name) — typically `<name>.<scope>` and
	// `<name>.<scope>.voodu`. When multiple replicas share the same
	// (scope, name), Docker DNS round-robins between them, giving
	// voodu a Service-like abstraction without an external registry.
	// Empty for unscoped resources or host/none network mode.
	NetworkAliases []string

	// AutoRemove sets `--rm` on docker run. Set by the job runner so
	// completed runs disappear from `docker ps -a` automatically;
	// long-running deployments leave it false (default).
	AutoRemove bool

	// TTY allocates a pseudo-terminal for the container (`docker run
	// -t`). Off by default — long-running deployments don't need it
	// and a TTY costs a tiny bit of kernel state per replica. The
	// release runner sets it true so user-language stdout (Ruby,
	// Node, Bun, Python) gets line-buffered: piped stdout would full-
	// buffer until the process exits, defeating realtime log
	// streaming. Operators see migration output flow live instead of
	// in one final dump.
	TTY bool
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
		Name:           spec.Name,
		Image:          spec.Image,
		Command:        spec.Command,
		Ports:          spec.Ports,
		Volumes:        spec.Volumes,
		NetworkMode:    spec.NetworkMode,
		Networks:       spec.Networks,
		NetworkAliases: spec.NetworkAliases,
		RestartPolicy:  spec.Restart,
		EnvFile:        spec.EnvFile,
		Labels:         spec.Labels,
		AutoRemove:     spec.AutoRemove,
		TTY:            spec.TTY,
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

// ListByIdentity asks docker for every container matching the
// (kind, scope, name) tuple via the structured voodu.* labels.
// Includes stopped containers — the reconciler may need to revive
// them after a host reboot. Labels are read in a second pass via
// docker inspect because `docker ps --format json` doesn't include
// them.
//
// The list returned is unsorted — replicas are interchangeable, so
// callers must not depend on order. If you find yourself wanting
// "the first slot" you're probably reaching for old slot-index
// thinking; use the labels to filter to a specific replica id.
func (DockerContainerManager) ListByIdentity(kind, scope, name string) ([]ContainerSlot, error) {
	filters := []string{
		containers.LabelKind + "=" + kind,
		containers.LabelName + "=" + name,
	}

	if scope != "" {
		filters = append(filters, containers.LabelScope+"="+scope)
	}

	infos, err := docker.ListContainersFiltered(true, filters)
	if err != nil {
		return nil, fmt.Errorf("list containers (kind=%s scope=%s name=%s): %w", kind, scope, name, err)
	}

	out := make([]ContainerSlot, 0, len(infos))

	for _, c := range infos {
		raw := c.Names
		if raw == "" {
			raw = c.Name
		}

		cname := strings.TrimPrefix(strings.TrimSpace(raw), "/")
		if cname == "" {
			continue
		}

		labels, err := docker.InspectLabels(cname)
		if err != nil {
			// Inspection failure on one container shouldn't poison
			// the rest of the list — log-and-skip is the safer
			// choice during a partial docker outage. We can't log
			// from here without dragging a logger into the manager,
			// so the caller (handlers) sees a shorter list and the
			// next reconcile retries.
			continue
		}

		id, ok := containers.ParseLabels(labels)
		if !ok {
			// Filter said this carried voodu labels but inspect
			// disagrees — race with a delete, skip cleanly.
			continue
		}

		// docker filter is OR-permissive when a label key is
		// missing on a container; double-check here so we don't
		// pick up unrelated voodu containers that happen to share
		// a different (kind, scope, name) tuple.
		if !id.Matches(kind, scope, name) {
			continue
		}

		running, _ := docker.IsRunning(cname)

		out = append(out, ContainerSlot{
			Name:     cname,
			Identity: id,
			Image:    extractImage(c),
			Running:  running,
		})
	}

	return out, nil
}

// extractImage normalises the image field from `docker ps --format
// json`. It can come back as a tag (`vd-web:latest`), a digest, or
// an ID — all are valid identifiers but the reconciler prefers the
// tag when available.
func extractImage(c docker.ContainerInfo) string {
	return c.Image
}

// ListLegacyByApp finds pre-M0 containers (no voodu.* labels) by
// docker name pattern, exactly the way ListByAppPrefix used to.
// Kept narrow on purpose — the only caller is the handler's
// migration sweep, which removes anything this returns and lets
// the normal reconcile path recreate it under M0 naming.
func (DockerContainerManager) ListLegacyByApp(app string) ([]string, error) {
	infos, err := docker.ListContainers(true)
	if err != nil {
		return nil, err
	}

	var out []string

	for _, c := range infos {
		raw := c.Names
		if raw == "" {
			raw = c.Name
		}

		name := strings.TrimPrefix(strings.TrimSpace(raw), "/")
		if name == "" {
			continue
		}

		// Only legacy if the container's labels DON'T already
		// declare an M0 identity. Re-inspect once: the umbrella
		// createdby filter let everything through.
		labels, err := docker.InspectLabels(name)
		if err != nil {
			continue
		}

		if _, ok := containers.ParseLabels(labels); ok {
			// ParseLabels returns ok when createdby=voodu is set;
			// the M0 fields may all be empty but we can't tell
			// the difference between "M0 with empty Kind" and
			// "legacy with no Kind". Treat any container that has
			// the umbrella label as voodu-managed; require the
			// kind label to consider it M0 — absence of kind is
			// the legacy signal.
			if labels[containers.LabelKind] != "" {
				continue
			}
		}

		if containers.LegacyContainerName(name, app) {
			out = append(out, name)
		}
	}

	return out, nil
}

// Wait blocks until the container exits, then returns its exit code.
// Thin shim over docker.WaitContainer — the abstraction lives on
// ContainerManager so the job runner can be tested with a fake.
func (DockerContainerManager) Wait(name string) (int, error) {
	return docker.WaitContainer(name)
}

// Logs is a thin shim over docker.LogsStream. The interface lives on
// ContainerManager so the API handler stays testable with a fake.
func (DockerContainerManager) Logs(name string, opts LogsOptions) (io.ReadCloser, error) {
	return docker.LogsStream(name, opts.Follow, opts.Tail)
}

// Exec is a thin shim over docker.ExecContainer. Translates the
// controller-side ExecOptions to docker.ExecOptions (same shape but
// lives in a different package so callers don't have to import
// internal/docker for one struct).
func (DockerContainerManager) Exec(name string, command []string, opts ExecOptions) (int, error) {
	return docker.ExecContainer(name, command, docker.ExecOptions{
		TTY:         opts.TTY,
		Interactive: opts.Interactive,
		WorkingDir:  opts.WorkingDir,
		User:        opts.User,
		Env:         opts.Env,
		Cols:        opts.Cols,
		Rows:        opts.Rows,
		Stdin:       opts.Stdin,
		Stdout:      opts.Stdout,
		Stderr:      opts.Stderr,
	})
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
		Name:           spec.Name,
		Image:          spec.Image,
		Command:        spec.Command,
		Ports:          spec.Ports,
		Volumes:        spec.Volumes,
		NetworkMode:    spec.NetworkMode,
		Networks:       spec.Networks,
		NetworkAliases: spec.NetworkAliases,
		RestartPolicy:  spec.Restart,
		EnvFile:        spec.EnvFile,
		Labels:         spec.Labels,
		AutoRemove:     spec.AutoRemove,
		TTY:            spec.TTY,
	}

	return docker.CreateContainer(cfg)
}
