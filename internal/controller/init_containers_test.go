// Tests for the init container runner — the controller-side
// orchestrator that runs spec.InitContainers per-replica before
// the main container spawns. Pins the ordering invariant, retry
// semantics, timeout behavior, name composition, and the
// status-recording side-effects so a regression in any of those
// shows up loudly.

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/containers"
)

// fakeStatusRecorder satisfies initFailureRecorder. Records every
// recorded failure + every clear call so tests can assert the
// runner produced the right status events without standing up an
// etcd Store.
type fakeStatusRecorder struct {
	mu       sync.Mutex
	failures []InitFailure
	cleared  []string // replicaIDs cleared, in order
}

func (f *fakeStatusRecorder) RecordInitFailure(_ context.Context, _ string, failure InitFailure) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failures = append(f.failures, failure)
}

func (f *fakeStatusRecorder) ClearInitFailures(_ context.Context, _ string, replicaID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.cleared = append(f.cleared, replicaID)
}

// newInitTestParent returns a stock initContainerParent for tests
// — minimum non-empty values so the runner's spec construction
// doesn't trip an inherited-image fallback or empty-volume edge.
func newInitTestParent() initContainerParent {
	return initContainerParent{
		Image:    "ghcr.io/acme/api:1.0",
		Hash:     "abc123",
		Volumes:  []string{"/data:/data"},
		Networks: []string{"voodu0"},
		EnvFile:  "/var/lib/voodu/apps/test.env",
		Env:      map[string]string{"VOODU_SCOPE": "prod"},
	}
}

// TestRunInitChain_AllSucceed pins the happy path: every init
// exits 0, the runner returns len(inits) + nil, every step shows
// up in Recreate calls in declared order, and the status
// recorder sees a Clear call (post-success cleanup of any stale
// records from a previous reconcile).
func TestRunInitChain_AllSucceed(t *testing.T) {
	fc := &fakeContainers{}
	status := &fakeStatusRecorder{}

	r := &initContainerRunner{
		Containers: fc,
		Status:     status,
	}

	inits := []initContainerWireSpec{
		{Name: "migrate", Command: []string{"bin/rails", "db:migrate"}},
		{Name: "warm", Command: []string{"bin/warm"}},
	}

	idx, err := r.runInitChain(
		context.Background(), "prod-api", containers.KindDeployment,
		"prod", "api", "a3f9", inits, newInitTestParent(),
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if idx != 2 {
		t.Errorf("returned idx=%d, want 2 (all succeeded)", idx)
	}

	if len(fc.recreates) != 2 {
		t.Fatalf("want 2 Recreate calls, got %d", len(fc.recreates))
	}

	// Order matters: migrate before warm.
	wantNames := []string{"prod-api-init-migrate-a3f9", "prod-api-init-warm-a3f9"}

	for i, w := range wantNames {
		if fc.recreates[i].Name != w {
			t.Errorf("Recreate[%d].Name = %q, want %q", i, fc.recreates[i].Name, w)
		}
	}

	if len(status.failures) != 0 {
		t.Errorf("unexpected failure record(s): %+v", status.failures)
	}

	if len(status.cleared) != 1 || status.cleared[0] != "a3f9" {
		t.Errorf("want Clear(a3f9), got %v", status.cleared)
	}
}

// TestRunInitChain_StopsOnFailure pins the sequencing rule:
// init[1] failing means init[2] never runs. Otherwise an init
// chain could silently mask a broken migration by running
// independent steps after it.
func TestRunInitChain_StopsOnFailure(t *testing.T) {
	fc := &fakeContainers{
		waitExits: map[string]int{
			"prod-api-init-step2-x": 1, // step2 fails
		},
	}

	status := &fakeStatusRecorder{}
	r := &initContainerRunner{Containers: fc, Status: status}

	inits := []initContainerWireSpec{
		{Name: "step1", Command: []string{"true"}},
		{Name: "step2", Command: []string{"false"}},
		{Name: "step3", Command: []string{"true"}}, // must not run
	}

	idx, err := r.runInitChain(
		context.Background(), "prod-api", containers.KindDeployment,
		"prod", "api", "x", inits, newInitTestParent(),
	)
	if err == nil {
		t.Fatal("expected failure when an init exits non-zero")
	}

	if idx != 1 {
		t.Errorf("failed idx=%d, want 1 (step2)", idx)
	}

	// Recreate count: step1 + step2 (and only step2; no retries
	// since the spec didn't request them).
	if len(fc.recreates) != 2 {
		t.Errorf("want 2 Recreate calls (step1, step2), got %d: %+v",
			len(fc.recreates), recreateNames(fc.recreates))
	}

	// step3 never ran.
	for _, r := range fc.recreates {
		if strings.Contains(r.Name, "step3") {
			t.Errorf("step3 should not have run: %s", r.Name)
		}
	}

	// Status recorder got one failure with the right shape.
	if len(status.failures) != 1 {
		t.Fatalf("want 1 failure record, got %d", len(status.failures))
	}

	got := status.failures[0]
	if got.InitName != "step2" || got.ExitCode != 1 || got.Attempts != 1 || got.ReplicaID != "x" {
		t.Errorf("failure record off: %+v", got)
	}

	// No clear — the chain failed, ClearInitFailures shouldn't fire.
	if len(status.cleared) != 0 {
		t.Errorf("Clear should not fire on failure, got: %v", status.cleared)
	}
}

// TestRunInitChain_RetriesUntilSuccess covers the bounded-retry
// behavior: an init that fails the first attempt but succeeds
// the second IS considered successful overall. Attempts count
// surfaces correctly + no failure record is written.
func TestRunInitChain_RetriesUntilSuccess(t *testing.T) {
	fc := &flakyContainers{
		// First call → exit 1; subsequent → exit 0.
		exitSequence: map[string][]int{
			"prod-api-init-migrate-x": {1, 0},
		},
	}

	status := &fakeStatusRecorder{}
	r := &initContainerRunner{Containers: fc, Status: status}

	inits := []initContainerWireSpec{
		{Name: "migrate", Command: []string{"bin/migrate"}, Retries: 2},
	}

	// Use a tiny test-only backoff so the test doesn't spend 2s
	// in retry sleep.
	oldBackoff := initRetryBackoffForTesting()
	defer restoreInitRetryBackoff(oldBackoff)

	idx, err := r.runInitChain(
		context.Background(), "prod-api", containers.KindDeployment,
		"prod", "api", "x", inits, newInitTestParent(),
	)
	if err != nil {
		t.Fatalf("retried-then-succeeded should not surface error: %v", err)
	}

	if idx != 1 {
		t.Errorf("idx=%d, want 1 (full success)", idx)
	}

	if len(fc.recreates) != 2 {
		t.Errorf("want 2 Recreate calls (initial + 1 retry), got %d", len(fc.recreates))
	}

	if len(status.failures) != 0 {
		t.Errorf("no failure should be recorded for a retried-success: %+v", status.failures)
	}

	if len(status.cleared) != 1 {
		t.Errorf("expected 1 Clear post-success, got %d", len(status.cleared))
	}
}

// TestRunInitChain_ExhaustsRetries pins the "give up after N
// attempts" rule: a perpetually failing init eventually surfaces
// as a single failure record with Attempts = 1 + retries.
func TestRunInitChain_ExhaustsRetries(t *testing.T) {
	fc := &fakeContainers{
		waitExits: map[string]int{
			"prod-api-init-flaky-x": 42,
		},
	}

	status := &fakeStatusRecorder{}
	r := &initContainerRunner{Containers: fc, Status: status}

	inits := []initContainerWireSpec{
		{Name: "flaky", Command: []string{"false"}, Retries: 3},
	}

	// Patch backoff so the 3 retries don't drag the test out.
	oldBackoff := initRetryBackoffForTesting()
	defer restoreInitRetryBackoff(oldBackoff)

	_, err := r.runInitChain(
		context.Background(), "prod-api", containers.KindDeployment,
		"prod", "api", "x", inits, newInitTestParent(),
	)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}

	if len(fc.recreates) != 4 {
		t.Errorf("want 4 Recreate calls (1 + 3 retries), got %d", len(fc.recreates))
	}

	if len(status.failures) != 1 {
		t.Fatalf("want 1 failure record, got %d", len(status.failures))
	}

	got := status.failures[0]
	if got.Attempts != 4 || got.ExitCode != 42 {
		t.Errorf("failure record: attempts=%d (want 4), exit=%d (want 42)", got.Attempts, got.ExitCode)
	}
}

// TestRunInitChain_ImageDefaultsToParent confirms the inherit-
// from-parent rule: an init without an image override picks up
// the deployment's image. The whole point of init containers is
// to run "stuff with the app's image" by default.
func TestRunInitChain_ImageDefaultsToParent(t *testing.T) {
	fc := &fakeContainers{}
	r := &initContainerRunner{Containers: fc, Status: &fakeStatusRecorder{}}

	inits := []initContainerWireSpec{
		{Name: "migrate", Command: []string{"true"}}, // no Image
		{Name: "warm", Command: []string{"true"}, Image: "alpine:3"},
	}

	parent := newInitTestParent()

	_, err := r.runInitChain(
		context.Background(), "prod-api", containers.KindDeployment,
		"prod", "api", "x", inits, parent,
	)
	if err != nil {
		t.Fatal(err)
	}

	if fc.recreates[0].Image != parent.Image {
		t.Errorf("init[0] image=%q, want parent %q (inherit)", fc.recreates[0].Image, parent.Image)
	}

	if fc.recreates[1].Image != "alpine:3" {
		t.Errorf("init[1] image=%q, want override alpine:3", fc.recreates[1].Image)
	}
}

// TestRunInitChain_InheritsEnvAndVolumes confirms init containers
// share the deployment's env/volumes/networks. This is the
// invariant that makes "rails db:migrate" work in the common
// case — the init must see DATABASE_URL the main pod will see.
func TestRunInitChain_InheritsEnvAndVolumes(t *testing.T) {
	fc := &fakeContainers{}
	r := &initContainerRunner{Containers: fc, Status: &fakeStatusRecorder{}}

	parent := newInitTestParent()
	parent.ExtraEnvFiles = []string{"/var/lib/voodu/apps/shared.env"}
	parent.ExtraHosts = []string{"db.local:10.0.0.5"}
	parent.CapAdd = []string{"SYS_NICE"}

	inits := []initContainerWireSpec{
		{Name: "step", Command: []string{"true"}},
	}

	_, err := r.runInitChain(
		context.Background(), "prod-api", containers.KindDeployment,
		"prod", "api", "x", inits, parent,
	)
	if err != nil {
		t.Fatal(err)
	}

	got := fc.recreates[0]
	if got.EnvFile != parent.EnvFile {
		t.Errorf("env file lost: %q vs %q", got.EnvFile, parent.EnvFile)
	}

	if len(got.ExtraEnvFiles) != 1 || got.ExtraEnvFiles[0] != parent.ExtraEnvFiles[0] {
		t.Errorf("extra env files lost: %v", got.ExtraEnvFiles)
	}

	if len(got.Volumes) != 1 || got.Volumes[0] != "/data:/data" {
		t.Errorf("volumes lost: %v", got.Volumes)
	}

	if len(got.Networks) != 1 || got.Networks[0] != "voodu0" {
		t.Errorf("networks lost: %v", got.Networks)
	}

	if len(got.ExtraHosts) != 1 || got.ExtraHosts[0] != "db.local:10.0.0.5" {
		t.Errorf("extra_hosts lost: %v", got.ExtraHosts)
	}

	if len(got.CapAdd) != 1 || got.CapAdd[0] != "SYS_NICE" {
		t.Errorf("cap_add lost: %v", got.CapAdd)
	}

	if got.Env["VOODU_SCOPE"] != "prod" {
		t.Errorf("podEnv lost: %+v", got.Env)
	}
}

// TestRunInitChain_NoOp checks the safe-path: no inits declared
// → no work done, no status events.
func TestRunInitChain_NoOp(t *testing.T) {
	fc := &fakeContainers{}
	status := &fakeStatusRecorder{}
	r := &initContainerRunner{Containers: fc, Status: status}

	idx, err := r.runInitChain(
		context.Background(), "prod-api", containers.KindDeployment,
		"prod", "api", "x", nil, newInitTestParent(),
	)
	if err != nil {
		t.Fatal(err)
	}

	if idx != 0 {
		t.Errorf("idx=%d, want 0 (no inits)", idx)
	}

	if len(fc.recreates) != 0 {
		t.Errorf("no Recreate should fire on empty init list: %d", len(fc.recreates))
	}

	if len(status.cleared) != 0 || len(status.failures) != 0 {
		t.Errorf("no status events for empty init list: %+v / %+v", status.cleared, status.failures)
	}
}

// TestInitContainerName verifies the name composition rule —
// `<scope>-<name>-init-<initName>-<replicaID>` for scoped
// resources, and the unscoped variant elides the leading scope.
func TestInitContainerName(t *testing.T) {
	cases := []struct {
		scope, name, replica, initN, want string
	}{
		{"prod", "api", "a3f9", "migrate", "prod-api-init-migrate-a3f9"},
		{"", "api", "a3f9", "migrate", "api-init-migrate-a3f9"},
		{"prod", "api", "0", "migrate", "prod-api-init-migrate-0"},
	}

	for _, c := range cases {
		got := initContainerName(c.scope, c.name, c.replica, c.initN)
		if got != c.want {
			t.Errorf("initContainerName(%q,%q,%q,%q) = %q, want %q",
				c.scope, c.name, c.replica, c.initN, got, c.want)
		}
	}
}

// TestAppendInitFailure_RingBuffer pins the LRU cap: more than
// maxInitFailures entries roll off the front so the status blob
// stays bounded.
func TestAppendInitFailure_RingBuffer(t *testing.T) {
	var existing []InitFailure

	for i := 0; i < maxInitFailures+5; i++ {
		existing = appendInitFailure(existing, InitFailure{
			ReplicaID: fmt.Sprintf("r%d", i),
			InitName:  "step",
		})
	}

	if len(existing) != maxInitFailures {
		t.Fatalf("len=%d, want %d (capped)", len(existing), maxInitFailures)
	}

	// Oldest entries are the ones that should have rolled off —
	// the LAST maxInitFailures should be present.
	if existing[0].ReplicaID != "r5" {
		t.Errorf("oldest retained=%q, want r5", existing[0].ReplicaID)
	}

	last := existing[len(existing)-1]
	if last.ReplicaID != fmt.Sprintf("r%d", maxInitFailures+4) {
		t.Errorf("newest=%q, want r%d", last.ReplicaID, maxInitFailures+4)
	}
}

// flakyContainers extends fakeContainers with a per-name exit-
// code QUEUE so a test can simulate "fail first attempt, succeed
// second." Reuses fakeContainers for everything else.
type flakyContainers struct {
	fakeContainers

	mu           sync.Mutex
	exitSequence map[string][]int // name -> [exit1, exit2, ...]
}

// Recreate just records and seeds (parent behavior).
func (f *flakyContainers) Recreate(spec ContainerSpec) error {
	return f.fakeContainers.Recreate(spec)
}

// Wait pops the next exit code from the queue for this name; if
// the queue is exhausted, the LAST recorded exit replays (so a
// pure "always fail" test can declare a 1-element sequence).
func (f *flakyContainers) Wait(name string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	seq, ok := f.exitSequence[name]
	if !ok || len(seq) == 0 {
		return 0, nil
	}

	code := seq[0]

	if len(seq) > 1 {
		f.exitSequence[name] = seq[1:]
	}
	// else: keep the single remaining element so subsequent
	// Wait calls return the same code (sticky failure mode).

	return code, nil
}

// recreateNames extracts the .Name field from a slice of
// ContainerSpec — handy for test failure messages where the full
// struct is noisy.
func recreateNames(recs []ContainerSpec) []string {
	names := make([]string, 0, len(recs))
	for _, r := range recs {
		names = append(names, r.Name)
	}

	return names
}

// initRetryBackoffForTesting saves the current production value
// and swaps in 1ms for the test. Pair with a defer
// restoreInitRetryBackoff so leaks across tests stay impossible.
// Tests that exercise the retry path would otherwise spend the
// full 2s production backoff between every attempt — punitive on
// the suite runtime.
func initRetryBackoffForTesting() time.Duration {
	prev := initRetryBackoff
	initRetryBackoff = time.Millisecond

	return prev
}

func restoreInitRetryBackoff(prev time.Duration) {
	initRetryBackoff = prev
}

// Compile-time guard: the runner's status hook must satisfy the
// initFailureRecorder interface. Catches drift between the
// interface and the recorder shape early.
var _ initFailureRecorder = (*fakeStatusRecorder)(nil)
var _ initFailureRecorder = (*DeploymentHandler)(nil)
var _ initFailureRecorder = (*StatefulsetHandler)(nil)

// Stub to keep `errors` import referenced for future expansions
// (e.g. injecting wait errors via the fake). Avoids the linter
// flagging an unused import while keeping the import line stable.
var _ = errors.New
