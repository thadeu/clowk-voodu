package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"time"

	"go.voodu.clowk.in/internal/containers"
)

// jobSpec mirrors the manifest's JobSpec shape — same duplication trick
// the deployment handler uses (avoid an import cycle: manifest already
// depends on controller, so controller can't import manifest back). Only
// the fields the runtime actually cares about are listed; everything
// else (lang, build inputs) flows to receive-pack at build time and is
// invisible to the controller.
type jobSpec struct {
	Image       string            `json:"image,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Volumes     []string          `json:"volumes,omitempty"`
	Network     string            `json:"network,omitempty"`
	Networks    []string          `json:"networks,omitempty"`
	NetworkMode string            `json:"network_mode,omitempty"`
	Timeout     string            `json:"timeout,omitempty"`
}

// JobHandler reconciles job manifests. Unlike DeploymentHandler, the
// apply path does NOT auto-execute the workload — declaring a job
// registers a runnable spec; running it is an explicit, imperative
// `voodu run job <scope>/<name>` call (or, in M4, a cron tick).
//
// Three responsibilities, in order of frequency:
//
//   1. apply — validate the spec, persist a baseline JobStatus so the
//      runner can find image/command/env later. Idempotent on replay.
//
//   2. RunOnce — the one imperative entry point. Pulls the manifest off
//      the store (so the run reflects current desired state, not a
//      snapshot taken at apply time), spawns a one-shot container with
//      `--rm`, blocks on docker wait, and records the exit code +
//      duration in the status's history. Removes the container itself
//      on success; on Wait error we leave the container around for the
//      operator to inspect.
//
//   3. remove — torch any historical job containers (rare, since
//      AutoRemove cleans them up post-run, but a long-running job that
//      hasn't exited yet would still be live). Status blob clears so
//      the next apply baselines from scratch.
//
// JobHandler does not own scheduling. M4's cronjob handler will share
// the RunOnce method (or a near twin) so cron ticks dispatch through
// the same code path.
type JobHandler struct {
	Store      Store
	Log        *log.Logger
	Containers ContainerManager

	// EnvFilePath, when set, is the path to the .env file mounted into
	// the job container. Jobs share the deployment env layout — a job
	// named "migrate" in scope "api" reads /opt/voodu/apps/api-migrate/.env
	// — so secrets set via `voodu config set api-migrate K=v` are
	// available to job runs without extra plumbing. Optional in tests.
	EnvFilePath func(app string) string

	// WriteEnv persists the spec's Env to the app's env file before the
	// run, mirroring the deployment handler. Optional — when nil, jobs
	// rely entirely on prior `voodu config set` invocations for env.
	WriteEnv func(app string, pairs []string) (changed bool, err error)
}

// JobStatus is persisted at /status/jobs/<app> after every apply and
// after every run. The shape carries enough to render `voodu describe
// job <name>` without re-reading the manifest:
//
//   - Image: the resolved image the next run will use (mirrors the
//     deployment status field).
//   - History: bounded list of recent runs (newest first), each with
//     run id, exit code, start/end times. Bounded so the status blob
//     doesn't grow unbounded over months of cron ticks; the oldest
//     entries fall off when the cap is hit.
type JobStatus struct {
	Image   string     `json:"image,omitempty"`
	History []JobRun   `json:"history,omitempty"`
	LastRun *time.Time `json:"last_run,omitempty"`
}

// JobRun is one historical execution. Status is "running" between
// spawn and Wait return, "succeeded" on exit 0, "failed" on non-zero
// or any docker-side error during the run. The runner captures any
// Wait error in Error so the operator sees why we couldn't tell what
// happened (container removed mid-run, daemon hiccup, ...).
type JobRun struct {
	RunID     string    `json:"run_id"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	ExitCode  int       `json:"exit_code"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
}

// historyCap bounds JobStatus.History so a long-lived job doesn't grow
// the status blob without limit. 50 is enough for an operator looking
// at `voodu describe job foo` to see the recent trend; older runs are
// gone — voodu logs (M5) can replace the audit trail.
const historyCap = 50

// JobRunStatus values. Strings (not iota) so etcd-stored history is
// readable in raw etcdctl dumps.
const (
	JobStatusRunning   = "running"
	JobStatusSucceeded = "succeeded"
	JobStatusFailed    = "failed"
)

// Handle dispatches WatchEvent → apply / remove. Mirrors the other
// handlers; the imperative RunOnce is reachable only through the
// /jobs/run HTTP endpoint, never the watch loop.
func (h *JobHandler) Handle(ctx context.Context, ev WatchEvent) error {
	switch ev.Type {
	case WatchPut:
		return h.apply(ctx, ev)

	case WatchDelete:
		return h.remove(ctx, ev)
	}

	return nil
}

func (h *JobHandler) apply(ctx context.Context, ev WatchEvent) error {
	if ev.Manifest == nil {
		return fmt.Errorf("put event without manifest")
	}

	spec, err := decodeJobSpec(ev.Manifest)
	if err != nil {
		return err
	}

	app := AppID(ev.Scope, ev.Name)

	// Build-mode default mirrors the deployment handler: an empty image
	// resolves to <app>:latest, which receive-pack's build pipeline
	// produces. Operators who push a registry image just set Image
	// explicitly.
	if spec.Image == "" {
		spec.Image = app + ":latest"
	}

	if err := validateJobNetwork(ev.Name, &spec); err != nil {
		return err
	}

	// Persist the baseline status so RunOnce can fetch it without
	// re-decoding the manifest. Preserve any existing history so a
	// re-apply after several runs doesn't wipe the audit trail.
	prev, err := h.loadStatus(ctx, app)
	if err != nil {
		return err
	}

	status := JobStatus{Image: spec.Image}
	if prev != nil {
		status.History = prev.History
		status.LastRun = prev.LastRun
	}

	if err := h.saveStatus(ctx, app, status); err != nil {
		return err
	}

	h.logf("job/%s registered (image=%s)", app, spec.Image)

	return nil
}

func (h *JobHandler) remove(ctx context.Context, ev WatchEvent) error {
	app := AppID(ev.Scope, ev.Name)

	if h.Containers != nil {
		// Torch any in-flight or stuck containers. Successful runs
		// should have AutoRemove'd already, but a still-running job at
		// delete time gets killed.
		slots, err := h.Containers.ListByIdentity(string(KindJob), ev.Scope, ev.Name)
		if err != nil {
			return fmt.Errorf("list job containers: %w", err)
		}

		for _, s := range slots {
			h.logf("job/%s removing container %s", app, s.Name)

			if err := h.Containers.Remove(s.Name); err != nil {
				return fmt.Errorf("remove %s: %w", s.Name, err)
			}
		}
	}

	if err := h.Store.DeleteStatus(ctx, KindJob, app); err != nil {
		return fmt.Errorf("clear job status: %w", err)
	}

	h.logf("job/%s deleted (history cleared)", app)

	return nil
}

// RunOnce executes the job synchronously and records the result. The
// caller (HTTP /jobs/run, future cron tick) gets the JobRun back so
// the response can carry exit code + duration without another
// round-trip to the store.
//
// Synchronous-on-purpose: the HTTP handler holds the connection open
// until the job completes. Long-running jobs would benefit from a
// "kick off + poll status" two-step, but that's M5+; today the simpler
// shape is fine for the migrations / one-off scripts the kind targets.
func (h *JobHandler) RunOnce(ctx context.Context, scope, name string) (JobRun, error) {
	app := AppID(scope, name)

	manifest, err := h.Store.Get(ctx, KindJob, scope, name)
	if err != nil {
		return JobRun{}, fmt.Errorf("read job manifest: %w", err)
	}

	if manifest == nil {
		return JobRun{}, fmt.Errorf("job/%s not found — apply it first", app)
	}

	spec, err := decodeJobSpec(manifest)
	if err != nil {
		return JobRun{}, err
	}

	if spec.Image == "" {
		spec.Image = app + ":latest"
	}

	if err := validateJobNetwork(name, &spec); err != nil {
		return JobRun{}, err
	}

	if h.Containers == nil {
		return JobRun{}, fmt.Errorf("job runner has no container manager configured")
	}

	envFile, err := h.linkEnv(app, spec.Env)
	if err != nil {
		return JobRun{}, err
	}

	runID := containers.NewReplicaID()
	cname := containers.ContainerName(scope, name, runID)

	startedAt := time.Now().UTC()

	labels := containers.BuildLabels(containers.Identity{
		Kind:      containers.KindJob,
		Scope:     scope,
		Name:      name,
		ReplicaID: runID,
		CreatedAt: startedAt.Format(time.RFC3339),
	})

	// Record the in-flight run BEFORE spawning so an operator hitting
	// /status mid-execution sees the run is happening. The Wait branch
	// updates the same entry in place.
	run := JobRun{
		RunID:     runID,
		StartedAt: startedAt,
		Status:    JobStatusRunning,
	}

	if err := h.appendRun(ctx, app, spec.Image, run); err != nil {
		// Status persist failure isn't fatal — the run can still
		// proceed. Log and carry on.
		h.logf("job/%s status persist failed (pre-run): %v", app, err)
	}

	if err := h.Containers.Recreate(ContainerSpec{
		Name:        cname,
		Image:       spec.Image,
		Command:     spec.Command,
		Volumes:     spec.Volumes,
		Networks:    spec.Networks,
		NetworkMode: spec.NetworkMode,
		EnvFile:     envFile,
		Labels:      labels,
		AutoRemove:  true,
	}); err != nil {
		// Recreate covers the rare "previous run with the same id is
		// still around" case. Failure here means we never even started
		// — fail the run synchronously and record it.
		run.Status = JobStatusFailed
		run.EndedAt = time.Now().UTC()
		run.Error = fmt.Sprintf("spawn: %v", err)

		_ = h.appendRun(ctx, app, spec.Image, run)

		return run, fmt.Errorf("spawn job container %s: %w", cname, err)
	}

	h.logf("job/%s run %s started (image=%s)", app, runID, spec.Image)

	exit, waitErr := h.Containers.Wait(cname)

	run.EndedAt = time.Now().UTC()
	run.ExitCode = exit

	switch {
	case waitErr != nil:
		run.Status = JobStatusFailed
		run.Error = waitErr.Error()
	case exit == 0:
		run.Status = JobStatusSucceeded
	default:
		run.Status = JobStatusFailed
	}

	if err := h.appendRun(ctx, app, spec.Image, run); err != nil {
		h.logf("job/%s status persist failed (post-run): %v", app, err)
	}

	if waitErr != nil {
		h.logf("job/%s run %s wait failed: %v", app, runID, waitErr)
		return run, waitErr
	}

	if exit != 0 {
		h.logf("job/%s run %s exited %d", app, runID, exit)
		return run, fmt.Errorf("job/%s run %s exited %d", app, runID, exit)
	}

	h.logf("job/%s run %s succeeded", app, runID)

	return run, nil
}

// linkEnv writes the job's static Env block to its app env file when a
// WriteEnv hook is configured. Returns the env file path the container
// should mount; empty when no hook is wired (tests, env-less jobs).
func (h *JobHandler) linkEnv(app string, env map[string]string) (string, error) {
	if h.WriteEnv == nil {
		if h.EnvFilePath == nil {
			return "", nil
		}

		return h.EnvFilePath(app), nil
	}

	pairs := envMapToPairs(env)

	if _, err := h.WriteEnv(app, pairs); err != nil {
		return "", fmt.Errorf("write job env: %w", err)
	}

	if h.EnvFilePath == nil {
		return "", nil
	}

	return h.EnvFilePath(app), nil
}

// loadStatus is a small helper that decodes the persisted status. nil
// status (no prior apply, no prior run) returns (nil, nil).
func (h *JobHandler) loadStatus(ctx context.Context, app string) (*JobStatus, error) {
	raw, err := h.Store.GetStatus(ctx, KindJob, app)
	if err != nil {
		return nil, err
	}

	if raw == nil {
		return nil, nil
	}

	var st JobStatus

	if err := json.Unmarshal(raw, &st); err != nil {
		// Treat a corrupt status as missing — the next apply re-baselines.
		// Surface a log so the operator can investigate without losing
		// the run.
		h.logf("job/%s status decode failed, treating as missing: %v", app, err)
		return nil, nil
	}

	return &st, nil
}

// saveStatus marshals + writes. Centralised so future schema bumps
// touch one path.
func (h *JobHandler) saveStatus(ctx context.Context, app string, status JobStatus) error {
	blob, err := json.Marshal(status)
	if err != nil {
		return err
	}

	return h.Store.PutStatus(ctx, KindJob, app, blob)
}

// appendRun upserts the given run into the status history and writes
// the result back. The run is matched by RunID — pre-run records get
// updated in place when post-run finalises. Newest run sits at the
// front (history[0]) so the common "show me the last run" query is
// O(1).
func (h *JobHandler) appendRun(ctx context.Context, app, image string, run JobRun) error {
	st, err := h.loadStatus(ctx, app)
	if err != nil {
		return err
	}

	if st == nil {
		st = &JobStatus{}
	}

	if image != "" {
		st.Image = image
	}

	updated := false

	for i := range st.History {
		if st.History[i].RunID == run.RunID {
			st.History[i] = run
			updated = true
			break
		}
	}

	if !updated {
		// Prepend so the newest run is always history[0]. Slice grows
		// at the front via append-and-rotate; no allocation surprise
		// at this size.
		st.History = append([]JobRun{run}, st.History...)
	}

	if len(st.History) > historyCap {
		st.History = st.History[:historyCap]
	}

	if !run.EndedAt.IsZero() {
		t := run.EndedAt
		st.LastRun = &t
	} else {
		t := run.StartedAt
		st.LastRun = &t
	}

	return h.saveStatus(ctx, app, *st)
}

// validateJobNetwork enforces the same host/none vs networks
// exclusivity the deployment handler does, then defaults to voodu0
// like deployments do (most jobs need to reach the platform's database
// containers, which live on voodu0). Mutates spec in place — caller
// receives the network block ready to ship to the runtime.
func validateJobNetwork(name string, spec *jobSpec) error {
	switch spec.NetworkMode {
	case "":
		// fallthrough to bridge defaulting
	case "host", "none":
		if len(spec.Networks) > 0 || spec.Network != "" {
			return fmt.Errorf("job/%s: network_mode=%q is mutually exclusive with network/networks", name, spec.NetworkMode)
		}

		return nil
	default:
		return fmt.Errorf("job/%s: network_mode=%q not supported (want \"host\" or \"none\"; omit for bridge mode)", name, spec.NetworkMode)
	}

	if len(spec.Networks) == 0 && spec.Network != "" {
		spec.Networks = []string{spec.Network}
	}

	if !slices.Contains(spec.Networks, "voodu0") {
		spec.Networks = append(spec.Networks, "voodu0")
	}

	return nil
}

// decodeJobSpec re-decodes the wire JSON. Same shape as the manifest's
// JobSpec; we only need a subset of fields here.
func decodeJobSpec(m *Manifest) (jobSpec, error) {
	var spec jobSpec

	if len(m.Spec) == 0 {
		return spec, fmt.Errorf("job/%s: empty spec", m.Name)
	}

	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return spec, fmt.Errorf("decode job spec: %w", err)
	}

	return spec, nil
}

func (h *JobHandler) logf(format string, args ...any) {
	if h.Log == nil {
		return
	}

	h.Log.Printf(format, args...)
}
