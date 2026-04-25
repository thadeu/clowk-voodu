package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/containers"
)

// TestCronJobHandler_ApplyPersistsBaselineStatus mirrors the job
// handler's contract: apply registers the schedule but doesn't fire
// the workload. The first tick is the responsibility of the
// scheduler, not Handle().
func TestCronJobHandler_ApplyPersistsBaselineStatus(t *testing.T) {
	store := newMemStore()
	cm := &fakeContainers{}

	h := &CronJobHandler{Store: store, Log: quietLogger(), Containers: cm}

	ev := putEvent(t, KindCronJob, "purge", map[string]any{
		"schedule": "*/5 * * * *",
		"job": map[string]any{
			"image":   "ghcr.io/acme/api:1.0.0",
			"command": []string{"./purge.sh"},
		},
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// Apply must not touch the runtime — the scheduler does that on tick.
	if len(cm.recreates)+len(cm.waits)+len(cm.removes) != 0 {
		t.Errorf("apply must not touch the runtime, got recreates=%d waits=%d removes=%d",
			len(cm.recreates), len(cm.waits), len(cm.removes))
	}

	raw, _ := store.GetStatus(context.Background(), KindCronJob, "test-purge")
	if raw == nil {
		t.Fatal("apply did not persist baseline status")
	}

	var st CronJobStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}

	if st.Schedule != "*/5 * * * *" {
		t.Errorf("schedule: got %q", st.Schedule)
	}

	if st.Image != "ghcr.io/acme/api:1.0.0" {
		t.Errorf("baseline image: got %q", st.Image)
	}

	if len(st.History) != 0 {
		t.Errorf("baseline must not carry history, got %+v", st.History)
	}
}

// TestCronJobHandler_ApplyRejectsBadSchedule keeps the operator out
// of "silently never fires" land. A malformed schedule must produce a
// loud apply-time error so `voodu apply` exits non-zero.
func TestCronJobHandler_ApplyRejectsBadSchedule(t *testing.T) {
	store := newMemStore()

	h := &CronJobHandler{Store: store, Log: quietLogger(), Containers: &fakeContainers{}}

	ev := putEvent(t, KindCronJob, "broken", map[string]any{
		"schedule": "not a real cron",
		"job": map[string]any{
			"image": "img:1",
		},
	})

	err := h.Handle(context.Background(), ev)
	if err == nil {
		t.Fatal("expected error for malformed schedule")
	}

	if !strings.Contains(err.Error(), "schedule") {
		t.Errorf("error should mention schedule, got %q", err.Error())
	}
}

// TestCronJobHandler_ApplyRejectsUnsupportedConcurrency locks in the
// concurrency-policy enum. "Replace" is reserved for a later milestone
// — silent acceptance would degrade to Allow without the operator
// noticing.
func TestCronJobHandler_ApplyRejectsUnsupportedConcurrency(t *testing.T) {
	store := newMemStore()

	h := &CronJobHandler{Store: store, Log: quietLogger(), Containers: &fakeContainers{}}

	ev := putEvent(t, KindCronJob, "purge", map[string]any{
		"schedule":           "*/5 * * * *",
		"concurrency_policy": "Replace",
		"job": map[string]any{
			"image": "img:1",
		},
	})

	err := h.Handle(context.Background(), ev)
	if err == nil {
		t.Fatal("expected error for unsupported concurrency policy")
	}

	if !strings.Contains(err.Error(), "concurrency_policy") {
		t.Errorf("error should mention concurrency_policy, got %q", err.Error())
	}
}

// TestCronJobHandler_TickSpawnsContainerAndRecordsSuccess is the M4
// happy path: scheduler calls Tick, which fetches the manifest,
// spawns a container labeled KindCronJob, blocks on Wait, and
// persists a succeeded JobRun.
func TestCronJobHandler_TickSpawnsContainerAndRecordsSuccess(t *testing.T) {
	store := newMemStore()

	spec := map[string]any{
		"schedule": "*/5 * * * *",
		"job": map[string]any{
			"image":   "ghcr.io/acme/api:1.0.0",
			"command": []string{"./purge.sh"},
		},
	}
	seedManifest(t, store, KindCronJob, "purge", spec)

	apply := &CronJobHandler{Store: store, Log: quietLogger(), Containers: &fakeContainers{}}
	if err := apply.Handle(context.Background(), putEvent(t, KindCronJob, "purge", spec)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	cm := &fakeContainers{}

	h := &CronJobHandler{Store: store, Log: quietLogger(), Containers: cm}

	run, err := h.Tick(context.Background(), "test", "purge")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if run.Status != JobStatusSucceeded {
		t.Errorf("status: got %q, want %q", run.Status, JobStatusSucceeded)
	}

	if len(cm.recreates) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(cm.recreates))
	}

	got := cm.recreates[0]
	if !strings.HasPrefix(got.Name, "test-purge.") {
		t.Errorf("container name: got %q, want test-purge.<run_id>", got.Name)
	}

	if got.AutoRemove {
		t.Errorf("cronjob container must run with AutoRemove=false so docker keeps the stopped container (and its logs) for `voodu logs cronjob`")
	}

	id := identityFromSpec(got)
	if id.Kind != containers.KindCronJob || id.Scope != "test" || id.Name != "purge" {
		t.Errorf("identity labels wrong: %+v (want kind=cronjob)", id)
	}

	raw, _ := store.GetStatus(context.Background(), KindCronJob, "test-purge")

	var st CronJobStatus
	_ = json.Unmarshal(raw, &st)

	if len(st.History) != 1 || st.History[0].Status != JobStatusSucceeded {
		t.Errorf("history did not record success: %+v", st.History)
	}

	if st.Schedule != "*/5 * * * *" {
		t.Errorf("schedule not persisted in status: %q", st.Schedule)
	}
}

// TestCronJobHandler_TickRespectsForbidConcurrency makes sure a
// Forbid-policy cronjob doesn't spawn a second container when the
// first is still in flight. Without this the scheduler's
// "advance-then-dispatch" path produces an overlap during slow ticks.
func TestCronJobHandler_TickRespectsForbidConcurrency(t *testing.T) {
	store := newMemStore()

	spec := map[string]any{
		"schedule":           "* * * * *",
		"concurrency_policy": ConcurrencyForbid,
		"job": map[string]any{
			"image": "img:1",
		},
	}
	seedManifest(t, store, KindCronJob, "purge", spec)

	cm := &fakeContainers{}

	h := &CronJobHandler{Store: store, Log: quietLogger(), Containers: cm}

	// Pre-mark the cronjob as in-flight to simulate a long-running
	// previous tick. The next Tick must skip without touching the
	// container manager.
	h.markInFlight("test/purge", true)

	run, err := h.Tick(context.Background(), "test", "purge")
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if run.RunID != "" {
		t.Errorf("forbidden tick should return zero JobRun, got %+v", run)
	}

	if len(cm.recreates) != 0 {
		t.Errorf("forbidden tick must not spawn, got %+v", cm.recreates)
	}
}

// TestCronJobHandler_TickSuspendIsNoOp covers the operator's "stop
// running this for now" knob. Suspended cronjobs stay declared but
// produce no runs.
func TestCronJobHandler_TickSuspendIsNoOp(t *testing.T) {
	store := newMemStore()

	spec := map[string]any{
		"schedule": "* * * * *",
		"suspend":  true,
		"job": map[string]any{
			"image": "img:1",
		},
	}
	seedManifest(t, store, KindCronJob, "purge", spec)

	cm := &fakeContainers{}

	h := &CronJobHandler{Store: store, Log: quietLogger(), Containers: cm}

	if _, err := h.Tick(context.Background(), "test", "purge"); err != nil {
		t.Fatalf("Tick on suspended cronjob: %v", err)
	}

	if len(cm.recreates) != 0 {
		t.Errorf("suspended cronjob must not spawn, got %+v", cm.recreates)
	}
}

// TestCronJobHandler_TickRecordsFailureOnNonZeroExit makes sure a
// failing tick is recorded with status=failed and the exit code,
// matching the JobHandler's contract.
func TestCronJobHandler_TickRecordsFailureOnNonZeroExit(t *testing.T) {
	store := newMemStore()

	spec := map[string]any{
		"schedule": "* * * * *",
		"job": map[string]any{
			"image": "img:1",
		},
	}
	seedManifest(t, store, KindCronJob, "purge", spec)

	wrapper := &exitCodeContainers{fakeContainers: &fakeContainers{}, exit: 17}

	h := &CronJobHandler{Store: store, Log: quietLogger(), Containers: wrapper}

	run, err := h.Tick(context.Background(), "test", "purge")
	if err == nil {
		t.Fatal("expected non-zero exit to surface as Tick error")
	}

	if run.Status != JobStatusFailed || run.ExitCode != 17 {
		t.Errorf("run: got status=%q exit=%d, want failed/17", run.Status, run.ExitCode)
	}

	raw, _ := store.GetStatus(context.Background(), KindCronJob, "test-purge")

	var st CronJobStatus
	_ = json.Unmarshal(raw, &st)

	if len(st.History) != 1 || st.History[0].Status != JobStatusFailed {
		t.Errorf("history did not record failure: %+v", st.History)
	}
}

// TestCronJobHandler_TickRecordsFailureOnWaitError covers the
// docker-side failure: the container started but Wait can't observe
// its exit. The error message is preserved in JobRun.Error.
func TestCronJobHandler_TickRecordsFailureOnWaitError(t *testing.T) {
	store := newMemStore()

	spec := map[string]any{
		"schedule": "* * * * *",
		"job":      map[string]any{"image": "img:1"},
	}
	seedManifest(t, store, KindCronJob, "purge", spec)

	wrapper := &exitCodeContainers{fakeContainers: &fakeContainers{}, waitErr: errors.New("daemon hiccup")}

	h := &CronJobHandler{Store: store, Log: quietLogger(), Containers: wrapper}

	run, err := h.Tick(context.Background(), "test", "purge")
	if err == nil {
		t.Fatal("expected wait error to surface")
	}

	if run.Status != JobStatusFailed {
		t.Errorf("status: got %q", run.Status)
	}

	if !strings.Contains(run.Error, "daemon hiccup") {
		t.Errorf("error should mention wait failure, got %q", run.Error)
	}
}

// TestCronJobHandler_HistoryCapsRespected makes sure the per-cronjob
// history limits prune older successes/failures. Without the cap, a
// cron firing every 5 minutes would balloon JobStatus over months.
func TestCronJobHandler_HistoryCapsRespected(t *testing.T) {
	successes := []JobRun{
		{RunID: "a", Status: JobStatusSucceeded},
		{RunID: "b", Status: JobStatusSucceeded},
		{RunID: "c", Status: JobStatusSucceeded},
		{RunID: "d", Status: JobStatusSucceeded},
		{RunID: "e", Status: JobStatusSucceeded},
	}

	got := capHistory(successes, 3, 1)
	if len(got) != 3 {
		t.Errorf("success cap: got %d, want 3", len(got))
	}

	if got[0].RunID != "a" || got[2].RunID != "c" {
		t.Errorf("expected newest-first survivors a..c, got %+v", got)
	}

	mixed := []JobRun{
		{RunID: "1", Status: JobStatusSucceeded},
		{RunID: "2", Status: JobStatusFailed},
		{RunID: "3", Status: JobStatusFailed},
		{RunID: "4", Status: JobStatusSucceeded},
		{RunID: "5", Status: JobStatusFailed},
	}

	got = capHistory(mixed, 2, 1)

	// successes 1, 4 (cap=2). failures 2 (cap=1). Order preserved.
	wantIDs := []string{"1", "2", "4"}

	if len(got) != len(wantIDs) {
		t.Fatalf("mixed cap: got %d entries, want %d (%+v)", len(got), len(wantIDs), got)
	}

	for i, id := range wantIDs {
		if got[i].RunID != id {
			t.Errorf("mixed cap[%d]: got %q, want %q", i, got[i].RunID, id)
		}
	}
}

// TestCronJobHandler_RemoveTearsDownContainersAndStatus mirrors the
// job handler's delete contract.
func TestCronJobHandler_RemoveTearsDownContainersAndStatus(t *testing.T) {
	store := newMemStore()

	pre, _ := json.Marshal(CronJobStatus{Schedule: "*/5 * * * *", Image: "img:1"})
	_ = store.PutStatus(context.Background(), KindCronJob, "test-purge", pre)

	cm := &fakeContainers{}
	cm.seedSlot(ContainerSlot{
		Name:  containers.ContainerName("test", "purge", "abcd"),
		Image: "img:1",
		Identity: containers.Identity{
			Kind:      containers.KindCronJob,
			Scope:     "test",
			Name:      "purge",
			ReplicaID: "abcd",
		},
		Running: true,
	})

	h := &CronJobHandler{Store: store, Log: quietLogger(), Containers: cm}

	err := h.Handle(context.Background(), WatchEvent{
		Type:  WatchDelete,
		Kind:  KindCronJob,
		Scope: "test",
		Name:  "purge",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(cm.removes) != 1 || cm.removes[0] != "test-purge.abcd" {
		t.Errorf("expected single remove of test-purge.abcd, got %+v", cm.removes)
	}

	if raw, _ := store.GetStatus(context.Background(), KindCronJob, "test-purge"); raw != nil {
		t.Errorf("status not cleared after delete: %s", raw)
	}
}
