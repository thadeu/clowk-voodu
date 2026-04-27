package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"go.voodu.clowk.in/internal/containers"
)

// cronJobSpec mirrors manifest.CronJobSpec for the same import-cycle
// reason jobSpec mirrors JobSpec. The embedded job carries the
// container fields a tick will run with.
type cronJobSpec struct {
	Schedule          string  `json:"schedule"`
	Job               jobSpec `json:"job"`
	ConcurrencyPolicy string  `json:"concurrency_policy,omitempty"`
	Timezone          string  `json:"timezone,omitempty"`
	Suspend           bool    `json:"suspend,omitempty"`

	SuccessfulHistoryLimit int `json:"successful_history_limit,omitempty"`
	FailedHistoryLimit     int `json:"failed_history_limit,omitempty"`
}

// CronJobHandler reconciles cronjob manifests. Apply validates the
// schedule + spec and persists a baseline CronJobStatus. The internal
// scheduler (started by the server alongside the reconciler) walks the
// declared cronjobs once a minute and calls Tick when a schedule is
// due. Tick spawns a one-shot container — same shape as
// JobHandler.RunOnce — but labeled KindCronJob so `voodu get pods
// --kind cronjob` filters cleanly.
type CronJobHandler struct {
	Store      Store
	Log        *log.Logger
	Containers ContainerManager

	EnvFilePath func(app string) string
	WriteEnv    func(app string, pairs []string) (changed bool, err error)

	// inFlight tracks (scope, name) that have a running tick. Used by
	// the Forbid concurrency policy to skip overlapping ticks. Allow
	// policies ignore it. Map is sync'd because the scheduler may
	// trigger multiple cronjobs concurrently.
	mu       sync.Mutex
	inFlight map[string]bool
}

// CronJobStatus is persisted at /status/cronjobs/<app>. Holds the
// resolved schedule string (so an operator can `voodu describe` it
// without re-decoding the manifest) plus the run history bucket the
// scheduler appends to. NextRun is purely informational — the
// in-memory scheduler doesn't read it; restarts re-derive Next from
// the schedule.
type CronJobStatus struct {
	Schedule string     `json:"schedule"`
	Image    string     `json:"image,omitempty"`
	NextRun  *time.Time `json:"next_run,omitempty"`
	LastRun  *time.Time `json:"last_run,omitempty"`
	History  []JobRun   `json:"history,omitempty"`
}

// Concurrency policies. Strings keep parity with k8s CronJob and
// produce readable etcd dumps.
const (
	ConcurrencyAllow  = "Allow"
	ConcurrencyForbid = "Forbid"
)

// Handle dispatches WatchEvent → apply / remove. Tick is reached
// through the scheduler, not the watch loop.
func (h *CronJobHandler) Handle(ctx context.Context, ev WatchEvent) error {
	switch ev.Type {
	case WatchPut:
		return h.apply(ctx, ev)

	case WatchDelete:
		return h.remove(ctx, ev)
	}

	return nil
}

func (h *CronJobHandler) apply(ctx context.Context, ev WatchEvent) error {
	if ev.Manifest == nil {
		return fmt.Errorf("put event without manifest")
	}

	spec, err := decodeCronJobSpec(ev.Manifest)
	if err != nil {
		return err
	}

	app := AppID(ev.Scope, ev.Name)

	// Validate the schedule expression up-front so the operator gets a
	// crisp error on apply rather than a silent no-fire later. Reject
	// the manifest entirely if the schedule is malformed — the
	// scheduler can't safely fall back to "run never" without
	// surfacing the misconfig.
	if _, err := ParseSchedule(spec.Schedule, spec.Timezone); err != nil {
		return fmt.Errorf("cronjob/%s: schedule: %w", app, err)
	}

	// Concurrency policy is the small enum that makes the scheduler
	// simpler. Empty defaults to Allow; anything outside the supported
	// set errors loud rather than silently degrading to a default the
	// operator didn't ask for.
	switch spec.ConcurrencyPolicy {
	case "", ConcurrencyAllow, ConcurrencyForbid:
		// supported
	default:
		return fmt.Errorf("cronjob/%s: concurrency_policy %q not supported (want %q or %q)",
			app, spec.ConcurrencyPolicy, ConcurrencyAllow, ConcurrencyForbid)
	}

	if spec.Job.Image == "" {
		spec.Job.Image = app + ":latest"
	}

	if err := validateJobNetwork(ev.Name, &spec.Job); err != nil {
		return fmt.Errorf("cronjob/%s: %w", app, err)
	}

	prev, err := h.loadStatus(ctx, app)
	if err != nil {
		return err
	}

	status := CronJobStatus{
		Schedule: spec.Schedule,
		Image:    spec.Job.Image,
	}

	if prev != nil {
		status.History = prev.History
		status.LastRun = prev.LastRun
	}

	if err := h.saveStatus(ctx, app, status); err != nil {
		return err
	}

	h.logf("cronjob/%s registered (schedule=%q, image=%s, suspend=%t)",
		app, spec.Schedule, spec.Job.Image, spec.Suspend)

	return nil
}

func (h *CronJobHandler) remove(ctx context.Context, ev WatchEvent) error {
	app := AppID(ev.Scope, ev.Name)

	if h.Containers != nil {
		slots, err := h.Containers.ListByIdentity(string(KindCronJob), ev.Scope, ev.Name)
		if err != nil {
			return fmt.Errorf("list cronjob containers: %w", err)
		}

		for _, s := range slots {
			h.logf("cronjob/%s removing container %s", app, s.Name)

			if err := h.Containers.Remove(s.Name); err != nil {
				return fmt.Errorf("remove %s: %w", s.Name, err)
			}
		}
	}

	if err := h.Store.DeleteStatus(ctx, KindCronJob, app); err != nil {
		return fmt.Errorf("clear cronjob status: %w", err)
	}

	h.logf("cronjob/%s deleted (history cleared)", app)

	return nil
}

// Tick spawns one cronjob run. Invoked by the scheduler when the
// cronjob's Next() time is up. The flow mirrors JobHandler.RunOnce:
// fetch manifest, build labels (with KindCronJob), Recreate, Wait,
// record JobRun in CronJobStatus.History.
//
// Returns the JobRun even when an error is returned so the caller
// (scheduler) has the exit code + duration to log.
func (h *CronJobHandler) Tick(ctx context.Context, scope, name string) (JobRun, error) {
	app := AppID(scope, name)
	key := scope + "/" + name

	manifest, err := h.Store.Get(ctx, KindCronJob, scope, name)
	if err != nil {
		return JobRun{}, fmt.Errorf("read cronjob manifest: %w", err)
	}

	if manifest == nil {
		return JobRun{}, fmt.Errorf("cronjob/%s not found", app)
	}

	spec, err := decodeCronJobSpec(manifest)
	if err != nil {
		return JobRun{}, err
	}

	if spec.Suspend {
		// Suspended cronjobs stay parsed and validated, but no tick
		// fires. The scheduler still calls Tick — it doesn't peek at
		// Suspend — so we filter here. Cheap idempotent return.
		return JobRun{}, nil
	}

	if spec.ConcurrencyPolicy == ConcurrencyForbid && h.markInFlight(key, true) {
		// Already running. Skip silently — an operator wanting to know
		// missed ticks reads the scheduler logs.
		h.logf("cronjob/%s skipped (Forbid: previous tick still running)", app)
		return JobRun{}, nil
	}

	defer h.markInFlight(key, false)

	if spec.Job.Image == "" {
		spec.Job.Image = app + ":latest"
	}

	if err := validateJobNetwork(name, &spec.Job); err != nil {
		return JobRun{}, err
	}

	if h.Containers == nil {
		return JobRun{}, fmt.Errorf("cronjob runner has no container manager configured")
	}

	envFile, err := h.linkEnv(ctx, scope, name, app, spec.Job.Env)
	if err != nil {
		return JobRun{}, err
	}

	runID := containers.NewReplicaID()
	cname := containers.ContainerName(scope, name, runID)

	startedAt := time.Now().UTC()

	labels := containers.BuildLabels(containers.Identity{
		Kind:      containers.KindCronJob,
		Scope:     scope,
		Name:      name,
		ReplicaID: runID,
		CreatedAt: startedAt.Format(time.RFC3339),
	})

	run := JobRun{
		RunID:     runID,
		StartedAt: startedAt,
		Status:    JobStatusRunning,
	}

	successCap, failureCap := historyLimits(spec)

	if err := h.appendRun(ctx, app, spec.Schedule, spec.Job.Image, run, successCap, failureCap); err != nil {
		h.logf("cronjob/%s status persist failed (pre-run): %v", app, err)
	}

	if err := h.Containers.Recreate(ContainerSpec{
		Name:           cname,
		Image:          spec.Job.Image,
		Command:        spec.Job.Command,
		Volumes:        spec.Job.Volumes,
		Networks:       spec.Job.Networks,
		NetworkMode:    spec.Job.NetworkMode,
		NetworkAliases: BuildNetworkAliases(scope, name),
		EnvFile:        envFile,
		Labels:         labels,
		// AutoRemove is intentionally false: docker keeps the stopped
		// container (and its json-file logs) so `voodu logs cronjob
		// <name>` can read them post-tick. The runner GCs old run
		// containers past successful_history_limit /
		// failed_history_limit below.
		AutoRemove: false,
	}); err != nil {
		run.Status = JobStatusFailed
		run.EndedAt = time.Now().UTC()
		run.Error = fmt.Sprintf("spawn: %v", err)

		_ = h.appendRun(ctx, app, spec.Schedule, spec.Job.Image, run, successCap, failureCap)

		return run, fmt.Errorf("spawn cronjob container %s: %w", cname, err)
	}

	h.logf("cronjob/%s tick %s started (image=%s)", app, runID, spec.Job.Image)

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

	if err := h.appendRun(ctx, app, spec.Schedule, spec.Job.Image, run, successCap, failureCap); err != nil {
		h.logf("cronjob/%s status persist failed (post-run): %v", app, err)
	}

	// GC stopped run containers past the spec's history caps. Mirrors
	// JobHandler.RunOnce — keep set comes from the freshly-persisted
	// history, errors don't fail the tick.
	if err := h.gcRuns(ctx, scope, name); err != nil {
		h.logf("cronjob/%s container gc failed: %v", app, err)
	}

	if waitErr != nil {
		h.logf("cronjob/%s tick %s wait failed: %v", app, runID, waitErr)
		return run, waitErr
	}

	if exit != 0 {
		h.logf("cronjob/%s tick %s exited %d", app, runID, exit)
		return run, fmt.Errorf("cronjob/%s tick %s exited %d", app, runID, exit)
	}

	h.logf("cronjob/%s tick %s succeeded", app, runID)

	return run, nil
}

// markInFlight is a small CAS for the Forbid concurrency policy. When
// `value` is true, returns true iff the key was already in flight (and
// leaves the map state set). When false, clears the entry.
func (h *CronJobHandler) markInFlight(key string, value bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.inFlight == nil {
		h.inFlight = map[string]bool{}
	}

	if !value {
		delete(h.inFlight, key)
		return false
	}

	if h.inFlight[key] {
		return true
	}

	h.inFlight[key] = true
	return false
}

// gcRuns prunes stopped run containers whose replica id no longer
// appears in the persisted history. Mirrors JobHandler.gcRuns; the
// only difference is the kind label and the status type.
func (h *CronJobHandler) gcRuns(ctx context.Context, scope, name string) error {
	if h.Containers == nil {
		return nil
	}

	st, err := h.loadStatus(ctx, AppID(scope, name))
	if err != nil {
		return err
	}

	keep := map[string]bool{}

	if st != nil {
		for _, r := range st.History {
			keep[r.RunID] = true
		}
	}

	return gcRunContainers(h.Containers, string(KindCronJob), scope, name, keep)
}

// gcRunContainers walks the container slots for the given identity
// and removes any stopped one whose replica id isn't in keepRunIDs.
// Running containers are skipped — the next pass after they exit will
// catch them. Used by both JobHandler and CronJobHandler post-run GC.
func gcRunContainers(cm ContainerManager, kind, scope, name string, keepRunIDs map[string]bool) error {
	slots, err := cm.ListByIdentity(kind, scope, name)
	if err != nil {
		return fmt.Errorf("list %s containers: %w", kind, err)
	}

	for _, s := range slots {
		if s.Running {
			continue
		}

		if keepRunIDs[s.Identity.ReplicaID] {
			continue
		}

		if err := cm.Remove(s.Name); err != nil {
			return fmt.Errorf("remove %s: %w", s.Name, err)
		}
	}

	return nil
}

// historyLimits returns the (success, failure) bounds for this
// cronjob's run history, defaulting unset fields the same way k8s
// does (3 / 1).
func historyLimits(spec cronJobSpec) (successCap, failureCap int) {
	successCap = spec.SuccessfulHistoryLimit

	if successCap <= 0 {
		successCap = 3
	}

	failureCap = spec.FailedHistoryLimit

	if failureCap <= 0 {
		failureCap = 1
	}

	return successCap, failureCap
}

// linkEnv merges controller-managed config with the cronjob's
// static Env block. Same shape and precedence as JobHandler.linkEnv —
// scope config → app config → manifest spec.Job.Env (last wins).
func (h *CronJobHandler) linkEnv(ctx context.Context, scope, name, app string, env map[string]string) (string, error) {
	merged := map[string]string{}

	if h.Store != nil {
		ctrlConfig, err := h.Store.ResolveConfig(ctx, scope, name)
		if err == nil {
			for k, v := range ctrlConfig {
				merged[k] = v
			}
		}
	}

	for k, v := range env {
		merged[k] = v
	}

	if h.WriteEnv == nil {
		if h.EnvFilePath == nil {
			return "", nil
		}

		return h.EnvFilePath(app), nil
	}

	pairs := envMapToPairs(merged)

	if _, err := h.WriteEnv(app, pairs); err != nil {
		return "", fmt.Errorf("write cronjob env: %w", err)
	}

	if h.EnvFilePath == nil {
		return "", nil
	}

	return h.EnvFilePath(app), nil
}

func (h *CronJobHandler) loadStatus(ctx context.Context, app string) (*CronJobStatus, error) {
	raw, err := h.Store.GetStatus(ctx, KindCronJob, app)
	if err != nil {
		return nil, err
	}

	if raw == nil {
		return nil, nil
	}

	var st CronJobStatus

	if err := json.Unmarshal(raw, &st); err != nil {
		h.logf("cronjob/%s status decode failed, treating as missing: %v", app, err)
		return nil, nil
	}

	return &st, nil
}

func (h *CronJobHandler) saveStatus(ctx context.Context, app string, status CronJobStatus) error {
	blob, err := json.Marshal(status)
	if err != nil {
		return err
	}

	return h.Store.PutStatus(ctx, KindCronJob, app, blob)
}

// appendRun upserts the given run by RunID, then prunes the history
// down to the cronjob's success/failure caps. Newest-first ordering
// matches JobHandler so `voodu describe cronjob` renders the same way.
func (h *CronJobHandler) appendRun(ctx context.Context, app, schedule, image string, run JobRun, successCap, failureCap int) error {
	st, err := h.loadStatus(ctx, app)
	if err != nil {
		return err
	}

	if st == nil {
		st = &CronJobStatus{}
	}

	st.Schedule = schedule

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
		st.History = append([]JobRun{run}, st.History...)
	}

	st.History = capHistory(st.History, successCap, failureCap)

	if !run.EndedAt.IsZero() {
		t := run.EndedAt
		st.LastRun = &t
	} else {
		t := run.StartedAt
		st.LastRun = &t
	}

	return h.saveStatus(ctx, app, *st)
}

// capHistory keeps at most successCap successful + failureCap failed
// runs, walking the history newest-first and dropping entries beyond
// the per-status caps. Running entries always survive — the operator
// expects the in-flight tick to be visible.
func capHistory(history []JobRun, successCap, failureCap int) []JobRun {
	out := make([]JobRun, 0, len(history))

	successes := 0
	failures := 0

	for _, r := range history {
		switch r.Status {
		case JobStatusSucceeded:
			if successes >= successCap {
				continue
			}

			successes++

		case JobStatusFailed:
			if failures >= failureCap {
				continue
			}

			failures++
		}

		out = append(out, r)
	}

	return out
}

// decodeCronJobSpec re-decodes the wire JSON. Same shape as the
// manifest's CronJobSpec; we only need a subset of fields here.
func decodeCronJobSpec(m *Manifest) (cronJobSpec, error) {
	var spec cronJobSpec

	if len(m.Spec) == 0 {
		return spec, fmt.Errorf("cronjob/%s: empty spec", m.Name)
	}

	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		return spec, fmt.Errorf("decode cronjob spec: %w", err)
	}

	if spec.Schedule == "" {
		return spec, fmt.Errorf("cronjob/%s: schedule is required", m.Name)
	}

	return spec, nil
}

// historyHas is a small helper for tests — not used in production.
// Returning whether the run shows up in the persisted history blob.
//
//nolint:unused // exported via testing only
func historyHas(st *CronJobStatus, runID string) bool {
	if st == nil {
		return false
	}

	return slices.ContainsFunc(st.History, func(r JobRun) bool { return r.RunID == runID })
}

func (h *CronJobHandler) logf(format string, args ...any) {
	if h.Log == nil {
		return
	}

	h.Log.Printf(format, args...)
}
