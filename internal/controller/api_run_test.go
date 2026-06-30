package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRunner stubs both JobRunner and CronJobRunner with the same
// canned response. Records the (scope, name) it received so tests
// can assert the API forwarded the resolved tuple correctly.
//
// run_job dispatch invokes RunOnce on a BACKGROUND goroutine
// (applyDispatchRunJob), so the recorded fields are written off the
// request thread. The mutex + called() getter let the asserting test
// read them race-free; honor the real runner's concurrency contract.
type fakeRunner struct {
	mu       sync.Mutex
	gotScope string
	gotName  string

	run JobRun
	err error
}

func (f *fakeRunner) RunOnce(_ context.Context, scope, name string) (JobRun, error) {
	f.mu.Lock()
	f.gotScope = scope
	f.gotName = name
	f.mu.Unlock()

	return f.run, f.err
}

func (f *fakeRunner) Tick(_ context.Context, scope, name string) (JobRun, error) {
	f.mu.Lock()
	f.gotScope = scope
	f.gotName = name
	f.mu.Unlock()

	return f.run, f.err
}

// called returns the (scope, name) the runner last recorded, read under
// the lock so it's safe to poll while RunOnce runs on the dispatch's
// async goroutine.
func (f *fakeRunner) called() (scope, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.gotScope, f.gotName
}

// TestCronJobRun_ForwardsScopeAndName confirms /cronjobs/run pulls
// scope+name from the query string and hands them to the runner
// verbatim. Without this the handler could silently swap kinds and
// fire a Job instead of a CronJob tick.
func TestCronJobRun_ForwardsScopeAndName(t *testing.T) {
	store := newMemStore()
	runner := &fakeRunner{run: JobRun{RunID: "abcd", Status: JobStatusSucceeded}}

	api := &API{Store: store, Version: "test", CronJobs: runner}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/cronjobs/run?scope=ops&name=purge", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	if scope, name := runner.called(); scope != "ops" || name != "purge" {
		t.Errorf("runner got scope=%q name=%q, want ops/purge", scope, name)
	}
}

// TestCronJobRun_ResolvesUnambiguousBareName mirrors the job runner:
// when scope is omitted and only one cronjob holds the name across
// scopes, the handler resolves and proceeds. Without this, a CLI
// invocation `vd run cronjob purge` would always 404.
func TestCronJobRun_ResolvesUnambiguousBareName(t *testing.T) {
	store := newMemStore()
	runner := &fakeRunner{run: JobRun{RunID: "abcd", Status: JobStatusSucceeded}}

	_, _ = store.Put(t.Context(), &Manifest{Kind: KindCronJob, Scope: "ops", Name: "purge"})

	api := &API{Store: store, Version: "test", CronJobs: runner}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/cronjobs/run?name=purge", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}

	if scope, _ := runner.called(); scope != "ops" {
		t.Errorf("scope should auto-resolve to ops, got %q", scope)
	}
}

// TestCronJobRun_ReturnsRunRecordOnFailure confirms the runner's
// failure surface: non-nil error gets wrapped in an envelope but
// the JobRun (with exit code, duration) still rides along so the
// CLI can render the run details on a 500.
func TestCronJobRun_ReturnsRunRecordOnFailure(t *testing.T) {
	store := newMemStore()
	runner := &fakeRunner{
		run: JobRun{RunID: "ee01", Status: JobStatusFailed, ExitCode: 7},
		err: errFake,
	}

	api := &API{Store: store, Version: "test", CronJobs: runner}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/cronjobs/run?scope=ops&name=purge", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", resp.StatusCode)
	}

	var env struct {
		Status string
		Error  string
		Data   JobRun
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	if env.Status != "error" || !strings.Contains(env.Error, "fake") {
		t.Errorf("error envelope: %+v", env)
	}

	if env.Data.RunID != "ee01" || env.Data.ExitCode != 7 {
		t.Errorf("run record dropped from error envelope: %+v", env.Data)
	}
}

// TestCronJobRun_503WhenRunnerNotConfigured locks in the
// reduced-functionality contract: an API wired without CronJobs
// returns 503 instead of nil-panicking.
func TestCronJobRun_503WhenRunnerNotConfigured(t *testing.T) {
	store := newMemStore()
	api := &API{Store: store, Version: "test"}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/cronjobs/run?scope=ops&name=purge", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", resp.StatusCode)
	}
}

// errFake is a sentinel returned by fakeRunner to drive the
// failure-path tests. Defined as a package var so the assertion
// "error mentions fake" doesn't depend on stdlib error wrapping.
var errFake = stringError("fake runner failure")

type stringError string

func (e stringError) Error() string { return string(e) }
