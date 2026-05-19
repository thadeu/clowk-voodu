// init_containers.go is the controller-side runner for the M5
// init-container surface. Init containers are kubelet-style one-
// shot prep steps that run BEFORE a replica's main container
// starts: each must exit 0, in declared order, before the main
// pod is allowed to spawn.
//
// Lifecycle (per replica spawn):
//
//	for each init in spec.InitContainers:
//	    for attempt 0..Retries:
//	        Recreate(container)
//	        Wait(container) bounded by Timeout
//	        if exit 0: success → next init
//	        if exit !=0 or timeout: retry after 2s
//	    if all attempts exhausted: abort replica creation
//	main container spawn (existing Ensure path)
//
// Sharing semantics: init containers inherit the deployment's
// env file, env_from, networks, volumes, extra_hosts, cap_add.
// Per-init overrides are deliberately narrow (image, command,
// resources, timeout, retries) — the contract is "same env as the
// main pod, just a different command running first." This is what
// makes the common case ("rails db:migrate") trivial: the init sees
// the same DATABASE_URL the main pod will see.
//
// Failure isolation: a failed init does NOT remove its container
// — it stays around (stopped) so the operator can run
// `docker logs <init-container-name>` to debug. The next successful
// reconcile garbage-collects them via gcFailedInitContainers.

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.voodu.clowk.in/internal/containers"
)

// defaultInitContainerTimeout caps each init attempt at 10 minutes
// when the manifest doesn't say otherwise. Matches the release
// command default — same posture: long enough for slow migrations
// on big tables, short enough that a stuck step can't hang a
// rollout indefinitely.
const defaultInitContainerTimeout = 10 * time.Minute

// initRetryBackoff is the fixed wait between retry attempts.
// Linear (not exponential) on purpose: init containers run
// deterministic local commands, not flaky network requests —
// the failure mode we're guarding against is "migration script
// raced with the DB warming up", not "DNS hiccup," so a constant
// short pause is the right shape.
//
// Var (not const) so tests can dial it down to a millisecond
// when exercising the retry path — production stays at 2s.
var initRetryBackoff = 2 * time.Second

// initContainerRunner is the shared seam both DeploymentHandler
// and StatefulsetHandler use to execute the init container chain.
// Lives as a free struct so the same logic doesn't have to be
// duplicated across the two handler types — and so tests can
// instantiate it with fake Containers / Status writers without
// pulling in the full handler.
//
// Status is optional — pass nil for callers that don't track
// init failures (e.g. tests that only care about the exec path).
type initContainerRunner struct {
	Containers ContainerManager
	Status     initFailureRecorder
	logf       func(format string, args ...any)
}

// initFailureRecorder is the slim interface initContainerRunner
// uses to surface failures to the deployment / statefulset status
// blob. Implemented by both handlers via small adapter methods —
// keeping the interface narrow lets the runner remain unaware of
// the kind-specific status shapes.
type initFailureRecorder interface {
	RecordInitFailure(ctx context.Context, app string, failure InitFailure)
	ClearInitFailures(ctx context.Context, app string, replicaID string)
}

// InitFailure is one captured init-container failure exposed in
// DeploymentStatus.InitFailures (and the statefulset equivalent).
// `vd describe` renders these so an operator can see "replica X
// is blocked on init step Y exit Z" without grepping container
// logs.
//
// Capped at 10 entries per status blob (see RecordInitFailure
// implementations) — a chronically broken migration shouldn't
// bloat the etcd value.
type InitFailure struct {
	// ReplicaID identifies which replica's init flow failed. For
	// deployments this is the 4-char hex from NewReplicaID; for
	// statefulsets it's the ordinal as a decimal string ("0", "1").
	ReplicaID string `json:"replica_id"`

	// InitName is the operator-supplied name of the init step that
	// failed. Lines up with the HCL block label so the operator
	// can find the offending block immediately.
	InitName string `json:"init_name"`

	// ExitCode is the docker-reported exit code from the final
	// attempt. -1 indicates "container didn't run" (image pull
	// failure, timeout before exit, ctx cancel).
	ExitCode int `json:"exit_code"`

	// Error is the controller-side error message that aborted
	// the run — typically the docker-wait error or the wrapped
	// "container exited N" string. Free-form; intended for
	// operator eyes.
	Error string `json:"error,omitempty"`

	// Attempts is the number of attempts made (1 + retries used)
	// before giving up. 1 means "first try failed, no retries
	// configured."
	Attempts int `json:"attempts"`

	// RecordedAt is the controller-side timestamp at which the
	// failure was finalised. UTC, RFC3339 via time.Time JSON.
	RecordedAt time.Time `json:"recorded_at"`
}

// runInitChain executes spec.InitContainers in declared order for
// one replica. Returns (failedIdx, err) where failedIdx is the
// index of the first init that exhausted its retries (or
// len(spec.InitContainers) on full success).
//
// `app` is the AppID used for status recording. `replicaID` is the
// replica's identity string — passed through to status records so
// describe can show "ordinal 2 blocked" or "replica a3f9 blocked"
// depending on kind.
//
// Inheritance: the init container inherits ALL the network /
// volume / env shape from `parentSpec`, with init-level overrides
// applied on top. envFile / extraEnvFiles are resolved once by the
// caller and threaded through here so each init in the chain sees
// the same merged env (matching what the main pod will see).
func (r *initContainerRunner) runInitChain(
	ctx context.Context,
	app string,
	kind string,
	scope, name, replicaID string,
	inits []initContainerWireSpec,
	parent initContainerParent,
) (failedIdx int, err error) {
	if r == nil || r.Containers == nil || len(inits) == 0 {
		return len(inits), nil
	}

	logf := r.logf
	if logf == nil {
		logf = func(format string, args ...any) {}
	}

	for i, ic := range inits {
		// Per-attempt timeout — defaults to 10m on empty / invalid.
		// We re-parse on each iteration so a malformed value
		// doesn't poison the whole chain.
		timeout := defaultInitContainerTimeout
		if ic.Timeout != "" {
			if d, perr := time.ParseDuration(ic.Timeout); perr == nil && d > 0 {
				timeout = d
			}
		}

		attempts := 1 + ic.Retries
		cname := initContainerName(scope, name, replicaID, ic.Name)

		var lastErr error
		var lastExit int

		for attempt := 1; attempt <= attempts; attempt++ {
			lastExit, lastErr = r.runOne(ctx, kind, scope, name, replicaID, ic, parent, cname, timeout)
			if lastErr == nil {
				break
			}

			logf("%s/%s init %q attempt %d/%d failed: %v", kind, app, ic.Name, attempt, attempts, lastErr)

			// Last attempt? Don't backoff — fall through to the
			// failure-recording branch below.
			if attempt < attempts {
				select {
				case <-ctx.Done():
					return i, ctx.Err()
				case <-time.After(initRetryBackoff):
				}
			}
		}

		if lastErr != nil {
			if r.Status != nil {
				r.Status.RecordInitFailure(ctx, app, InitFailure{
					ReplicaID:  replicaID,
					InitName:   ic.Name,
					ExitCode:   lastExit,
					Error:      lastErr.Error(),
					Attempts:   attempts,
					RecordedAt: time.Now().UTC(),
				})
			}

			return i, fmt.Errorf("init container %q failed after %d attempts: %w", ic.Name, attempts, lastErr)
		}

		// Success — clear any stale failure record for this
		// (replica, init) pair so describe doesn't show ghosts
		// from a previous reconcile.
	}

	// All inits succeeded — clear any stale failures for this
	// replica. Successful init flow erases the prior failures
	// recorded under the same replicaID, mirroring kubelet's
	// "passing now" reset.
	if r.Status != nil {
		r.Status.ClearInitFailures(ctx, app, replicaID)
	}

	return len(inits), nil
}

// runOne executes a single init container attempt and returns
// (exitCode, error). exitCode = -1 means the run couldn't even
// produce one (Recreate failure, context cancel before Wait).
// A successful run returns (0, nil).
//
// The container is left in place on failure (no Remove) so an
// operator can `docker logs <name>` after the fact. Success path
// removes the container immediately — we don't need it around
// once it exited 0.
func (r *initContainerRunner) runOne(
	ctx context.Context,
	kind, scope, name, replicaID string,
	ic initContainerWireSpec,
	parent initContainerParent,
	cname string,
	timeout time.Duration,
) (int, error) {
	// Image inherits from parent when the init didn't override —
	// the common case for "run a one-liner with my app's image."
	image := ic.Image
	if image == "" {
		image = parent.Image
	}

	cpu, memBytes, err := dockerResources(ic.Resources)
	if err != nil {
		return -1, fmt.Errorf("resources: %w", err)
	}

	logMaxSize, logMaxFiles := effectiveLogs(parent.Logs)

	labels := containers.BuildLabels(containers.Identity{
		Kind:         kind,
		Scope:        scope,
		Name:         name,
		ReplicaID:    replicaID,
		ManifestHash: parent.Hash,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		Role:         "init",
	})

	spec := ContainerSpec{
		Name:             cname,
		Image:            image,
		Command:          ic.Command,
		Volumes:          parent.Volumes,
		Networks:         parent.Networks,
		NetworkMode:      parent.NetworkMode,
		NetworkAliases:   parent.NetworkAliases,
		EnvFile:          parent.EnvFile,
		ExtraEnvFiles:    parent.ExtraEnvFiles,
		Env:              parent.Env,
		Labels:           labels,
		ExtraHosts:       parent.ExtraHosts,
		CapAdd:           parent.CapAdd,
		CPULimit:         cpu,
		MemoryLimitBytes: memBytes,
		LogMaxSize:       logMaxSize,
		LogMaxFiles:      logMaxFiles,
		// AutoRemove=false: we need Wait to read the exit code
		// after the process exits. The release container path uses
		// the same posture for the same reason.
		AutoRemove: false,
		// TTY=true forces line-buffered stdout so `docker logs`
		// post-failure shows useful output. Mirrors the release
		// runner — same operator-debug goal.
		TTY: true,
	}

	// Recreate is the right verb: we WANT a clean container per
	// attempt. A retry on a stopped container would replay the
	// docker exit code; Recreate gives us a fresh run.
	if err := r.Containers.Recreate(spec); err != nil {
		return -1, fmt.Errorf("spawn: %w", err)
	}

	// Per-attempt timeout via a child context. The Wait goroutine
	// reads exit code from the daemon directly; we observe via
	// either the wait or the timeout, whichever fires first.
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan struct {
		code int
		err  error
	}, 1)

	go func() {
		code, werr := r.Containers.Wait(cname)
		done <- struct {
			code int
			err  error
		}{code, werr}
	}()

	select {
	case res := <-done:
		// Don't remove the container on failure — leaving it
		// around is the only way an operator can `docker logs`
		// the failed run. The next successful reconcile cleans
		// up via gcFailedInitContainers (or scale-up overwrites
		// the name via Recreate).
		if res.err != nil {
			return res.code, fmt.Errorf("wait: %w", res.err)
		}

		if res.code != 0 {
			return res.code, fmt.Errorf("container exited %d", res.code)
		}

		// Success — remove the container so the next reconcile's
		// stale-init GC doesn't have to track it. Ignored on error;
		// a leftover succeeded container is harmless.
		_ = r.Containers.Remove(cname)

		return 0, nil

	case <-waitCtx.Done():
		// Context expired OR parent ctx cancelled. Either way we
		// drop the container (best-effort) so the next attempt
		// gets a clean slate. Distinguish parent cancel from our
		// own timeout for the error message.
		if ctx.Err() != nil {
			_ = r.Containers.Remove(cname)
			return -1, ctx.Err()
		}

		_ = r.Containers.Remove(cname)

		return -1, fmt.Errorf("timed out after %s", timeout)
	}
}

// initContainerParent carries the parent (deployment / statefulset)
// fields the init runner needs to construct each child spec. Lives
// as a struct so adding a new propagated field doesn't churn the
// caller signature.
//
// Fields mirror ContainerSpec slots — the runner copies them
// verbatim into the init's ContainerSpec, then overlays the
// init-level overrides on top.
type initContainerParent struct {
	Image          string
	Hash           string
	Volumes        []string
	Networks       []string
	NetworkMode    string
	NetworkAliases []string
	EnvFile        string
	ExtraEnvFiles  []string
	Env            map[string]string
	ExtraHosts     []string
	CapAdd         []string
	Logs           *logsWireSpec
}

// initContainerName composes the docker container name for one
// init step. Shape: `<scope>-<name>-init-<initName>-<replicaID>`
// (or `<name>-init-<initName>-<replicaID>` when scope is empty).
//
// We slot "init" between the resource identity and the init's
// own name so `docker ps --filter name=-init-` reveals every
// init container across the host at once. Different from the
// main container shape (`<scope>-<name>.<replicaID>`) so an
// operator can grep / filter cleanly without confusing the two.
func initContainerName(scope, name, replicaID, initName string) string {
	base := name
	if scope != "" {
		base = scope + "-" + name
	}

	return strings.Join([]string{base, "init", initName, replicaID}, "-")
}
