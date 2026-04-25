package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/containers"
)

// TestJobHandler_ApplyPersistsBaselineStatus locks in the M3 contract
// that apply registers the spec but does NOT auto-execute. A successful
// apply should leave a JobStatus with the resolved image and an empty
// history — running is reserved for the imperative path.
func TestJobHandler_ApplyPersistsBaselineStatus(t *testing.T) {
	store := newMemStore()
	cm := &fakeContainers{}

	h := &JobHandler{Store: store, Log: quietLogger(), Containers: cm}

	ev := putEvent(t, KindJob, "migrate", map[string]any{
		"image":   "ghcr.io/acme/api:1.0.0",
		"command": []string{"./migrate.sh"},
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// No container side effects on apply — that's the whole point of
	// the imperative split.
	if len(cm.ensures)+len(cm.recreates)+len(cm.waits)+len(cm.removes) != 0 {
		t.Errorf("apply must not touch the runtime, got ensures=%d recreates=%d waits=%d removes=%d",
			len(cm.ensures), len(cm.recreates), len(cm.waits), len(cm.removes))
	}

	raw, _ := store.GetStatus(context.Background(), KindJob, "test-migrate")
	if raw == nil {
		t.Fatal("apply did not persist baseline status")
	}

	var st JobStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}

	if st.Image != "ghcr.io/acme/api:1.0.0" {
		t.Errorf("baseline image: got %q, want ghcr.io/acme/api:1.0.0", st.Image)
	}

	if len(st.History) != 0 {
		t.Errorf("baseline must not carry history, got %+v", st.History)
	}
}

// TestJobHandler_ApplyDefaultsToAppLatest mirrors the deployment
// handler's build-mode default: an image-less spec resolves to
// <app>:latest so receive-pack's build artefact lands in the right
// place when the operator later runs the job.
func TestJobHandler_ApplyDefaultsToAppLatest(t *testing.T) {
	store := newMemStore()

	h := &JobHandler{Store: store, Log: quietLogger(), Containers: &fakeContainers{}}

	ev := putEvent(t, KindJob, "migrate", map[string]any{})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("handle: %v", err)
	}

	raw, _ := store.GetStatus(context.Background(), KindJob, "test-migrate")

	var st JobStatus
	_ = json.Unmarshal(raw, &st)

	if st.Image != "test-migrate:latest" {
		t.Errorf("image-less spec should default to <app>:latest, got %q", st.Image)
	}
}

// TestJobHandler_ApplyPreservesHistoryOnReapply checks that re-applying
// a job after several runs doesn't wipe the audit trail. Operators
// re-apply often (`voodu apply` is idempotent and runs every push);
// losing run history would destroy the value of `voodu describe job`.
func TestJobHandler_ApplyPreservesHistoryOnReapply(t *testing.T) {
	store := newMemStore()

	pre, _ := json.Marshal(JobStatus{
		Image: "ghcr.io/acme/api:1.0.0",
		History: []JobRun{
			{RunID: "abcd", Status: JobStatusSucceeded, ExitCode: 0},
		},
	})
	_ = store.PutStatus(context.Background(), KindJob, "test-migrate", pre)

	h := &JobHandler{Store: store, Log: quietLogger(), Containers: &fakeContainers{}}

	ev := putEvent(t, KindJob, "migrate", map[string]any{
		"image": "ghcr.io/acme/api:1.0.0",
	})

	if err := h.Handle(context.Background(), ev); err != nil {
		t.Fatalf("handle: %v", err)
	}

	raw, _ := store.GetStatus(context.Background(), KindJob, "test-migrate")

	var st JobStatus
	_ = json.Unmarshal(raw, &st)

	if len(st.History) != 1 || st.History[0].RunID != "abcd" {
		t.Errorf("re-apply wiped history, got %+v", st.History)
	}
}

// TestJobHandler_RemoveTearsDownContainersAndStatus simulates a delete
// while a job container is still around (long-running exec, stuck
// run). Both the live container and the persisted status must go.
func TestJobHandler_RemoveTearsDownContainersAndStatus(t *testing.T) {
	store := newMemStore()

	pre, _ := json.Marshal(JobStatus{Image: "img:1"})
	_ = store.PutStatus(context.Background(), KindJob, "test-migrate", pre)

	cm := &fakeContainers{}
	cm.seedSlot(ContainerSlot{
		Name:  containers.ContainerName("test", "migrate", "abcd"),
		Image: "img:1",
		Identity: containers.Identity{
			Kind:      containers.KindJob,
			Scope:     "test",
			Name:      "migrate",
			ReplicaID: "abcd",
		},
		Running: true,
	})

	h := &JobHandler{Store: store, Log: quietLogger(), Containers: cm}

	err := h.Handle(context.Background(), WatchEvent{
		Type:  WatchDelete,
		Kind:  KindJob,
		Scope: "test",
		Name:  "migrate",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(cm.removes) != 1 || cm.removes[0] != "test-migrate.abcd" {
		t.Errorf("expected single remove of test-migrate.abcd, got %+v", cm.removes)
	}

	if raw, _ := store.GetStatus(context.Background(), KindJob, "test-migrate"); raw != nil {
		t.Errorf("status not cleared after delete: %s", raw)
	}
}

// TestJobHandler_RunOnceSpawnsContainerAndRecordsSuccess is the M3
// happy path: RunOnce fetches the manifest, spawns a container with
// AutoRemove=true, blocks on Wait, and persists a succeeded JobRun in
// the status history.
func TestJobHandler_RunOnceSpawnsContainerAndRecordsSuccess(t *testing.T) {
	store := newMemStore()

	// Apply once so the runner has a manifest to fetch. In production
	// the /apply HTTP handler persists the manifest before the watch
	// event fires; in tests we mimic that with seedManifest, then run
	// the watch-side apply to lay down the JobStatus baseline.
	spec := map[string]any{
		"image":   "ghcr.io/acme/api:1.0.0",
		"command": []string{"./migrate.sh"},
	}
	seedManifest(t, store, KindJob, "migrate", spec)

	apply := &JobHandler{Store: store, Log: quietLogger(), Containers: &fakeContainers{}}
	applyEv := putEvent(t, KindJob, "migrate", spec)

	if err := apply.Handle(context.Background(), applyEv); err != nil {
		t.Fatalf("apply: %v", err)
	}

	cm := &fakeContainers{}

	h := &JobHandler{Store: store, Log: quietLogger(), Containers: cm}

	run, err := h.RunOnce(context.Background(), "test", "migrate")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if run.Status != JobStatusSucceeded {
		t.Errorf("status: got %q, want %q", run.Status, JobStatusSucceeded)
	}

	if run.ExitCode != 0 {
		t.Errorf("exit code: got %d, want 0", run.ExitCode)
	}

	if run.RunID == "" || len(run.RunID) != 4 {
		t.Errorf("run id should be a 4-char hex, got %q", run.RunID)
	}

	if len(cm.recreates) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(cm.recreates))
	}

	got := cm.recreates[0]
	if !strings.HasPrefix(got.Name, "test-migrate.") {
		t.Errorf("container name: got %q, want test-migrate.<run_id>", got.Name)
	}

	if got.Image != "ghcr.io/acme/api:1.0.0" {
		t.Errorf("image: got %q", got.Image)
	}

	if got.AutoRemove {
		t.Errorf("job container must run with AutoRemove=false so docker keeps the stopped container (and its logs) for `voodu logs job`")
	}

	id := identityFromSpec(got)
	if id.Kind != containers.KindJob || id.Scope != "test" || id.Name != "migrate" {
		t.Errorf("identity labels wrong: %+v", id)
	}

	if len(cm.waits) != 1 || cm.waits[0] != got.Name {
		t.Errorf("Wait must block on the spawned container, got %+v", cm.waits)
	}

	// Status reflects the run.
	raw, _ := store.GetStatus(context.Background(), KindJob, "test-migrate")

	var st JobStatus
	_ = json.Unmarshal(raw, &st)

	if len(st.History) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(st.History))
	}

	if st.History[0].Status != JobStatusSucceeded {
		t.Errorf("history status: got %q", st.History[0].Status)
	}

	if st.History[0].RunID != run.RunID {
		t.Errorf("history run id mismatch: %q vs %q", st.History[0].RunID, run.RunID)
	}

	if st.LastRun == nil {
		t.Errorf("LastRun should be populated after a run")
	}
}

// TestJobHandler_RunOnceGCsOldRunContainers locks in the GC contract
// added when AutoRemove was dropped: once the freshly-finished run
// pushes the success cap past its limit, the runner removes stopped
// containers whose replica id no longer appears in history. Running
// containers (and the just-finished one, since it's still in history)
// must be left alone.
func TestJobHandler_RunOnceGCsOldRunContainers(t *testing.T) {
	store := newMemStore()

	// successful_history_limit=1, failed_history_limit=0 (defaults to 1
	// internally, but we only seed one stale success so the cap is the
	// driver here).
	spec := map[string]any{
		"image":                    "img:1",
		"successful_history_limit": 1,
	}
	seedManifest(t, store, KindJob, "migrate", spec)

	apply := &JobHandler{Store: store, Log: quietLogger(), Containers: &fakeContainers{}}
	_ = apply.Handle(context.Background(), putEvent(t, KindJob, "migrate", spec))

	cm := &fakeContainers{}

	// Stale stopped container from a previous successful run — should
	// be GC'd. Same kind/scope/name identity, different replica id.
	stale := containers.ContainerName("test", "migrate", "old1")
	cm.seedSlot(ContainerSlot{
		Name:    stale,
		Image:   "img:1",
		Running: false,
		Identity: containers.Identity{
			Kind:      containers.KindJob,
			Scope:     "test",
			Name:      "migrate",
			ReplicaID: "old1",
		},
	})

	// Running container from a separate prior run — must NOT be touched
	// even if it isn't in history (the next pass after it exits will
	// pick it up).
	live := containers.ContainerName("test", "migrate", "live")
	cm.seedSlot(ContainerSlot{
		Name:    live,
		Image:   "img:1",
		Running: true,
		Identity: containers.Identity{
			Kind:      containers.KindJob,
			Scope:     "test",
			Name:      "migrate",
			ReplicaID: "live",
		},
	})

	h := &JobHandler{Store: store, Log: quietLogger(), Containers: cm}

	if _, err := h.RunOnce(context.Background(), "test", "migrate"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(cm.removes) != 1 || cm.removes[0] != stale {
		t.Errorf("expected stale stopped container %q removed, got removes=%v", stale, cm.removes)
	}
}

// TestJobHistoryLimitsDefaults guards the k8s-style 3/1 defaults the
// runner falls back to when the manifest leaves the limits unset.
func TestJobHistoryLimitsDefaults(t *testing.T) {
	cases := []struct {
		name string
		spec jobSpec
		s, f int
	}{
		{"unset → 3 / 1", jobSpec{}, 3, 1},
		{"explicit success", jobSpec{SuccessfulHistoryLimit: 5}, 5, 1},
		{"explicit failure", jobSpec{FailedHistoryLimit: 4}, 3, 4},
		{"both explicit", jobSpec{SuccessfulHistoryLimit: 7, FailedHistoryLimit: 2}, 7, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, f := jobHistoryLimits(tc.spec)
			if s != tc.s || f != tc.f {
				t.Errorf("got (%d, %d), want (%d, %d)", s, f, tc.s, tc.f)
			}
		})
	}
}

// TestJobHandler_RunOnceRecordsFailureOnNonZeroExit covers the failure
// branch: container exits 17, run is recorded as failed with the exit
// code preserved, and RunOnce returns an error so the HTTP layer
// surfaces non-200.
func TestJobHandler_RunOnceRecordsFailureOnNonZeroExit(t *testing.T) {
	store := newMemStore()

	spec := map[string]any{"image": "img:1"}
	seedManifest(t, store, KindJob, "migrate", spec)

	apply := &JobHandler{Store: store, Log: quietLogger(), Containers: &fakeContainers{}}
	applyEv := putEvent(t, KindJob, "migrate", spec)
	_ = apply.Handle(context.Background(), applyEv)

	cm := &fakeContainers{
		// Wait reports exit 17 — typical "I errored cleanly" for a
		// bash-driven migration.
		waitExits: map[string]int{},
	}

	// We don't know the run-generated container name in advance, so
	// hook the wait map after the spawn happens. Easier: use a custom
	// Wait by pre-populating a glob via the existing "default" path —
	// here we set waitExits with a single magic key. The fake's Wait
	// looks up by exact name; instead patch it by making the spawn
	// trigger update the map. Simpler: make every wait return 17 by
	// shimming the fakeContainers default. Use a wrapper struct.

	wrapper := &exitCodeContainers{fakeContainers: cm, exit: 17}

	h := &JobHandler{Store: store, Log: quietLogger(), Containers: wrapper}

	run, err := h.RunOnce(context.Background(), "test", "migrate")
	if err == nil {
		t.Fatal("expected RunOnce to return an error for non-zero exit")
	}

	if run.Status != JobStatusFailed {
		t.Errorf("status: got %q, want %q", run.Status, JobStatusFailed)
	}

	if run.ExitCode != 17 {
		t.Errorf("exit code: got %d, want 17", run.ExitCode)
	}

	raw, _ := store.GetStatus(context.Background(), KindJob, "test-migrate")

	var st JobStatus
	_ = json.Unmarshal(raw, &st)

	if len(st.History) != 1 || st.History[0].Status != JobStatusFailed {
		t.Errorf("history did not record failure: %+v", st.History)
	}
}

// TestJobHandler_RunOnceRecordsFailureOnWaitError covers the docker-
// side failure: the container started but Wait can't observe its
// exit. Run is marked failed with the wait error preserved in
// JobRun.Error.
func TestJobHandler_RunOnceRecordsFailureOnWaitError(t *testing.T) {
	store := newMemStore()

	spec := map[string]any{"image": "img:1"}
	seedManifest(t, store, KindJob, "migrate", spec)

	apply := &JobHandler{Store: store, Log: quietLogger(), Containers: &fakeContainers{}}
	_ = apply.Handle(context.Background(), putEvent(t, KindJob, "migrate", spec))

	wrapper := &exitCodeContainers{fakeContainers: &fakeContainers{}, waitErr: errors.New("daemon hiccup")}

	h := &JobHandler{Store: store, Log: quietLogger(), Containers: wrapper}

	run, err := h.RunOnce(context.Background(), "test", "migrate")
	if err == nil {
		t.Fatal("expected RunOnce to return the wait error")
	}

	if run.Status != JobStatusFailed {
		t.Errorf("status: got %q", run.Status)
	}

	if run.Error == "" || !strings.Contains(run.Error, "daemon hiccup") {
		t.Errorf("error should mention the wait failure, got %q", run.Error)
	}
}

// TestJobHandler_RunOnceMissingManifestErrors makes the imperative
// run fail fast when nothing has been applied. Without this, the
// runner would happily spawn a container with image "<app>:latest"
// that doesn't exist locally — slower failure, worse error message.
func TestJobHandler_RunOnceMissingManifestErrors(t *testing.T) {
	store := newMemStore()

	h := &JobHandler{Store: store, Log: quietLogger(), Containers: &fakeContainers{}}

	_, err := h.RunOnce(context.Background(), "test", "ghost")
	if err == nil {
		t.Fatal("expected error for unknown job")
	}

	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got %q", err.Error())
	}
}

// TestValidateJobNetwork_DefaultsToVoodu0 mirrors the deployment
// handler's invariant: bridge-mode jobs always join voodu0 so they
// can talk to the platform's database containers (the typical job
// pattern: a migration that connects to the postgres plugin).
func TestValidateJobNetwork_DefaultsToVoodu0(t *testing.T) {
	cases := []struct {
		name string
		spec jobSpec
		want []string
	}{
		{
			name: "omitted → voodu0",
			spec: jobSpec{Image: "img:1"},
			want: []string{"voodu0"},
		},
		{
			name: "legacy singular → [network, voodu0]",
			spec: jobSpec{Image: "img:1", Network: "db"},
			want: []string{"db", "voodu0"},
		},
		{
			name: "explicit networks → voodu0 appended",
			spec: jobSpec{Image: "img:1", Networks: []string{"db"}},
			want: []string{"db", "voodu0"},
		},
		{
			name: "voodu0 already present → not duplicated",
			spec: jobSpec{Image: "img:1", Networks: []string{"voodu0", "db"}},
			want: []string{"voodu0", "db"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.spec
			if err := validateJobNetwork("migrate", &s); err != nil {
				t.Fatalf("validateJobNetwork: %v", err)
			}

			if len(s.Networks) != len(tc.want) {
				t.Fatalf("networks: got %v, want %v", s.Networks, tc.want)
			}

			for i := range s.Networks {
				if s.Networks[i] != tc.want[i] {
					t.Errorf("networks[%d]: got %q, want %q", i, s.Networks[i], tc.want[i])
				}
			}
		})
	}
}

// TestValidateJobNetwork_HostModeExclusive locks in that
// network_mode=host rejects networks/network exactly the way the
// deployment handler does. Same docker semantics, same loud error.
func TestValidateJobNetwork_HostModeExclusive(t *testing.T) {
	cases := []jobSpec{
		{Image: "img:1", NetworkMode: "host", Networks: []string{"db"}},
		{Image: "img:1", NetworkMode: "host", Network: "db"},
		{Image: "img:1", NetworkMode: "none", Networks: []string{"voodu0"}},
		{Image: "img:1", NetworkMode: "bridge"}, // bridge is invalid per spec
	}

	for i, spec := range cases {
		s := spec
		if err := validateJobNetwork("migrate", &s); err == nil {
			t.Errorf("case %d (%+v) should have errored", i, spec)
		}
	}
}

// exitCodeContainers wraps fakeContainers to override Wait without
// touching the per-name maps. Useful for tests that don't know the
// generated run id ahead of time.
type exitCodeContainers struct {
	*fakeContainers
	exit    int
	waitErr error
}

func (e *exitCodeContainers) Wait(name string) (int, error) {
	e.fakeContainers.waits = append(e.fakeContainers.waits, name)

	if e.waitErr != nil {
		return 0, e.waitErr
	}

	return e.exit, nil
}
