// Tests for the image pruner. Each case states one thing an operator would lose
// if the rule broke: a running pod's image, a rollback target, a build that
// just finished. The fake runtime stands in for docker; the real calls are in
// internal/docker/images.go.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.voodu.clowk.in/internal/activity"
	"go.voodu.clowk.in/internal/docker"
)

var pruneNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

type fakeImageRuntime struct {
	images     []docker.Image
	containers []string
	tagged     map[string]time.Time

	inUse      map[string]bool
	resolveErr error
	listErr    error
	removed    []string
	onRemove   func()
}

func (f *fakeImageRuntime) ListImages(context.Context) ([]docker.Image, error) {
	return f.images, f.listErr
}

func (f *fakeImageRuntime) ContainerImageIDs(context.Context) ([]string, error) {
	return f.containers, nil
}

func (f *fakeImageRuntime) ResolveImageID(_ context.Context, ref string) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}

	for _, img := range f.images {
		if slices.Contains(img.Tags, ref) {
			return img.ID, nil
		}
	}

	return "", nil
}

func (f *fakeImageRuntime) ImageLastTagged(_ context.Context, id string) (time.Time, error) {
	return f.tagged[id], nil
}

func (f *fakeImageRuntime) RemoveImage(_ context.Context, ref string) error {
	if f.onRemove != nil {
		f.onRemove()
	}

	if f.inUse[ref] {
		return docker.ErrImageInUse
	}

	f.removed = append(f.removed, ref)

	return nil
}

func (f *fakeImageRuntime) ImagesDiskSize(context.Context) (int64, error) {
	return int64(1000 - 100*len(f.removed)), nil
}

func oldImage(id string, tags ...string) docker.Image {
	return docker.Image{ID: id, Tags: tags, Created: pruneNow.Add(-30 * 24 * time.Hour)}
}

func newPruner(store Store, rt *fakeImageRuntime) *ImagePruner {
	return &ImagePruner{Store: store, Images: rt, Now: func() time.Time { return pruneNow }}
}

func putManifest(t *testing.T, store *memStore, kind Kind, scope, name, spec string) {
	t.Helper()

	if _, err := store.Put(t.Context(), &Manifest{Kind: kind, Scope: scope, Name: name, Spec: json.RawMessage(spec)}); err != nil {
		t.Fatal(err)
	}
}

func putReleases(t *testing.T, store *memStore, app string, releases ...ReleaseRecord) {
	t.Helper()

	raw, err := json.Marshal(DeploymentStatus{Releases: releases})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.PutStatus(t.Context(), KindDeployment, app, raw); err != nil {
		t.Fatal(err)
	}
}

func succeeded(build string) ReleaseRecord {
	return ReleaseRecord{BuildID: build, Status: ReleaseStatusSucceeded}
}

func TestPrune_RemovesWhatNothingNeeds(t *testing.T) {
	store := newMemStore()
	rt := &fakeImageRuntime{images: []docker.Image{oldImage("sha:gone", "orphan:1", "orphan:2"), oldImage("sha:dangling")}}

	report, err := newPruner(store, rt).Prune(t.Context(), ImagePruneOptions{})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"orphan:1", "orphan:2", "sha:dangling"}
	if !slices.Equal(rt.removed, want) {
		t.Errorf("removed %v, want %v", rt.removed, want)
	}

	if report.ReclaimedBytes != 300 {
		t.Errorf("reclaimed %d, want 300", report.ReclaimedBytes)
	}
}

func TestPrune_KeepsImageOfAnyContainer(t *testing.T) {
	store := newMemStore()
	rt := &fakeImageRuntime{
		images:     []docker.Image{oldImage("sha:stopped", "app:old")},
		containers: []string{"sha:stopped"},
	}

	report, err := newPruner(store, rt).Prune(t.Context(), ImagePruneOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(rt.removed) != 0 || report.Kept != 1 {
		t.Errorf("removed %v, kept %d", rt.removed, report.Kept)
	}
}

func TestPrune_KeepsImagesNamedByManifests(t *testing.T) {
	store := newMemStore()

	putManifest(t, store, KindCronJob, "bk", "report", `{"image":"worker:7"}`)
	putManifest(t, store, KindDeployment, "bk", "web", `{"image":"web:1","init_containers":[{"image":"migrate:3"}]}`)

	rt := &fakeImageRuntime{images: []docker.Image{
		oldImage("sha:worker", "worker:7"),
		oldImage("sha:web", "web:1"),
		oldImage("sha:migrate", "migrate:3"),
		oldImage("sha:other", "other:1"),
	}}

	if _, err := newPruner(store, rt).Prune(t.Context(), ImagePruneOptions{}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(rt.removed, []string{"other:1"}) {
		t.Errorf("removed %v, want only other:1", rt.removed)
	}
}

func TestPrune_KeepsLatestOfBuildModeResources(t *testing.T) {
	store := newMemStore()

	putManifest(t, store, KindJob, "bk", "migrate", `{}`)
	putManifest(t, store, KindDeployment, "bk", "web", `{}`)

	rt := &fakeImageRuntime{images: []docker.Image{
		oldImage("sha:job", "bk-migrate:latest", "bk-migrate:abc"),
		oldImage("sha:web", "bk-web:latest"),
		oldImage("sha:deleted", "bk-deleted:latest"),
	}}

	if _, err := newPruner(store, rt).Prune(t.Context(), ImagePruneOptions{}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(rt.removed, []string{"bk-deleted:latest"}) {
		t.Errorf("removed %v, want only the deleted app's image", rt.removed)
	}
}

func TestPrune_KeepsNewestBuildsForRollback(t *testing.T) {
	store := newMemStore()

	putManifest(t, store, KindDeployment, "bk", "web", `{}`)
	putReleases(t, store, "bk-web",
		succeeded("b6"), succeeded("b5"), succeeded("b4"), succeeded("b3"), succeeded("b2"), succeeded("b1"))

	rt := &fakeImageRuntime{}

	for _, b := range []string{"b1", "b2", "b3", "b4", "b5", "b6"} {
		rt.images = append(rt.images, oldImage("sha:"+b, "bk-web:"+b))
	}

	if _, err := newPruner(store, rt).Prune(t.Context(), ImagePruneOptions{}); err != nil {
		t.Fatal(err)
	}

	if want := []string{"bk-web:b1"}; !slices.Equal(rt.removed, want) {
		t.Errorf("default keep: removed %v, want %v", rt.removed, want)
	}

	rt.removed = nil

	if _, err := newPruner(store, rt).Prune(t.Context(), ImagePruneOptions{KeepReleases: 4}); err != nil {
		t.Fatal(err)
	}

	if want := []string{"bk-web:b1", "bk-web:b2"}; !slices.Equal(rt.removed, want) {
		t.Errorf("keep=4: removed %v, want %v", rt.removed, want)
	}
}

func TestPrune_RecordsOfOneBuildTakeOneSlot(t *testing.T) {
	store := newMemStore()

	putManifest(t, store, KindDeployment, "bk", "web", `{}`)
	putReleases(t, store, "bk-web",
		succeeded("b3"), succeeded("b3"), succeeded("b3"), succeeded("b2"), succeeded("b1"), succeeded("b0"))

	rt := &fakeImageRuntime{images: []docker.Image{
		oldImage("sha:b3", "bk-web:b3"),
		oldImage("sha:b2", "bk-web:b2"),
		oldImage("sha:b1", "bk-web:b1"),
		oldImage("sha:b0", "bk-web:b0"),
	}}

	if _, err := newPruner(store, rt).Prune(t.Context(), ImagePruneOptions{KeepReleases: 3}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(rt.removed, []string{"bk-web:b0"}) {
		t.Errorf("removed %v, want only b0", rt.removed)
	}
}

// Rollback refuses a failed release, so a failed one must not push a good
// target out of the window. The newest record is what is live or deploying,
// and it stays whatever its status.
func TestPrune_FailedReleasesTakeNoSlot(t *testing.T) {
	store := newMemStore()

	putManifest(t, store, KindDeployment, "bk", "web", `{}`)
	putReleases(t, store, "bk-web",
		ReleaseRecord{BuildID: "running", Status: ReleaseStatusRunning},
		ReleaseRecord{BuildID: "failed", Status: ReleaseStatusFailed},
		succeeded("b2"), succeeded("b1"), succeeded("b0"))

	rt := &fakeImageRuntime{images: []docker.Image{
		oldImage("sha:running", "bk-web:running"),
		oldImage("sha:failed", "bk-web:failed"),
		oldImage("sha:b2", "bk-web:b2"),
		oldImage("sha:b1", "bk-web:b1"),
		oldImage("sha:b0", "bk-web:b0"),
	}}

	if _, err := newPruner(store, rt).Prune(t.Context(), ImagePruneOptions{KeepReleases: 3}); err != nil {
		t.Fatal(err)
	}

	if want := []string{"bk-web:failed", "bk-web:b0"}; !slices.Equal(rt.removed, want) {
		t.Errorf("removed %v, want %v", rt.removed, want)
	}
}

func TestPrune_ImageModeRollbackTargetsStay(t *testing.T) {
	store := newMemStore()

	putManifest(t, store, KindDeployment, "bk", "api", `{"image":"api:3"}`)
	putReleases(t, store, "bk-api",
		ReleaseRecord{Image: "api:3", Status: ReleaseStatusSucceeded},
		ReleaseRecord{Image: "api:2", Status: ReleaseStatusSucceeded},
		ReleaseRecord{Image: "api:1", Status: ReleaseStatusSucceeded},
		ReleaseRecord{Image: "api:0", Status: ReleaseStatusSucceeded})

	rt := &fakeImageRuntime{images: []docker.Image{
		oldImage("sha:3", "api:3"),
		oldImage("sha:2", "api:2"),
		oldImage("sha:1", "api:1"),
		oldImage("sha:0", "api:0"),
	}}

	if _, err := newPruner(store, rt).Prune(t.Context(), ImagePruneOptions{KeepReleases: 3}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(rt.removed, []string{"api:0"}) {
		t.Errorf("removed %v, want only api:0", rt.removed)
	}
}

// A build finishes, then the release records it. Between the two the image is
// referenced by nothing, and a cached build keeps its old Created date, so only
// the tag time can protect it.
func TestPrune_KeepsRecentlyTaggedImages(t *testing.T) {
	store := newMemStore()

	rt := &fakeImageRuntime{
		images: []docker.Image{
			oldImage("sha:cached-build", "bk-web:new"),
			{ID: "sha:fresh", Tags: []string{"bk-web:fresh"}, Created: pruneNow.Add(-10 * time.Minute)},
			oldImage("sha:stale", "bk-web:stale"),
		},
		tagged: map[string]time.Time{
			"sha:cached-build": pruneNow.Add(-time.Minute),
			"sha:stale":        pruneNow.Add(-2 * time.Hour),
		},
	}

	if _, err := newPruner(store, rt).Prune(t.Context(), ImagePruneOptions{}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(rt.removed, []string{"bk-web:stale"}) {
		t.Errorf("removed %v, want only the stale one", rt.removed)
	}
}

// failingStatusStore makes the status read fail after the manifests load.
type failingStatusStore struct{ *memStore }

func (failingStatusStore) GetStatus(context.Context, Kind, string) ([]byte, error) {
	return nil, errors.New("etcd down")
}

// If the keep set can't be built, the images it would have protected look
// unused. The run must stop instead of deleting them.
func TestPrune_FailsClosed(t *testing.T) {
	images := []docker.Image{oldImage("sha:live", "bk-web:latest"), oldImage("sha:other", "other:1")}

	t.Run("store error", func(t *testing.T) {
		store := newMemStore()
		putManifest(t, store, KindDeployment, "bk", "web", `{}`)

		rt := &fakeImageRuntime{images: images}

		if _, err := newPruner(failingStatusStore{store}, rt).Prune(t.Context(), ImagePruneOptions{}); err == nil {
			t.Fatal("expected an error")
		}

		if len(rt.removed) != 0 {
			t.Errorf("removed %v after a store error", rt.removed)
		}
	})

	t.Run("resolve error", func(t *testing.T) {
		store := newMemStore()
		putManifest(t, store, KindDeployment, "bk", "web", `{}`)

		rt := &fakeImageRuntime{images: images, resolveErr: errors.New("daemon down")}

		if _, err := newPruner(store, rt).Prune(t.Context(), ImagePruneOptions{}); err == nil {
			t.Fatal("expected an error")
		}

		if len(rt.removed) != 0 {
			t.Errorf("removed %v after a resolve error", rt.removed)
		}
	})
}

func TestPrune_DaemonRefusalIsSkippedNotFailed(t *testing.T) {
	rt := &fakeImageRuntime{
		images: []docker.Image{oldImage("sha:a", "raced:1"), oldImage("sha:b", "free:1")},
		inUse:  map[string]bool{"raced:1": true},
	}

	report, err := newPruner(newMemStore(), rt).Prune(t.Context(), ImagePruneOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(report.Skipped, []string{"raced:1"}) || !slices.Equal(report.Removed, []string{"free:1"}) || len(report.Errors) != 0 {
		t.Errorf("report: %+v", report)
	}
}

func TestPrune_DryRunRemovesNothing(t *testing.T) {
	rt := &fakeImageRuntime{images: []docker.Image{oldImage("sha:a", "orphan:1")}}

	report, err := newPruner(newMemStore(), rt).Prune(t.Context(), ImagePruneOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(rt.removed) != 0 {
		t.Errorf("dry run removed %v", rt.removed)
	}

	if !report.DryRun || !slices.Equal(report.Removed, []string{"orphan:1"}) {
		t.Errorf("report: %+v", report)
	}
}

func TestPrune_SecondRunIsRefusedWhileOneIsActive(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	rt := &fakeImageRuntime{
		images: []docker.Image{oldImage("sha:a", "orphan:1")},
		onRemove: func() {
			close(entered)
			<-release
		},
	}

	p := newPruner(newMemStore(), rt)
	done := make(chan struct{})

	go func() {
		defer close(done)

		_, _ = p.Prune(context.Background(), ImagePruneOptions{})
	}()

	<-entered

	if _, err := p.Prune(t.Context(), ImagePruneOptions{}); !errors.Is(err, ErrImagePruneRunning) {
		t.Errorf("second run: %v, want ErrImagePruneRunning", err)
	}

	close(release)
	<-done
}

func TestDefaultImagePruneSchedule_RunsTwiceADay(t *testing.T) {
	schedule, err := ParseSchedule(DefaultImagePruneSchedule, "")
	if err != nil {
		t.Fatal(err)
	}

	first := schedule.Next(pruneNow)
	second := schedule.Next(first)
	third := schedule.Next(second)

	if first.Hour() != 15 || second.Hour() != 3 || third.Hour() != 15 {
		t.Errorf("runs at %v, %v, %v", first, second, third)
	}

	if third.Sub(first) != 24*time.Hour {
		t.Errorf("two runs should span 24h, got %v", third.Sub(first))
	}
}

func TestRunSchedule_PrunesAndRecordsOnEachTick(t *testing.T) {
	schedule, err := ParseSchedule(DefaultImagePruneSchedule, "")
	if err != nil {
		t.Fatal(err)
	}

	tick := make(chan time.Time)
	recorded := make(chan activity.Record, 1)

	rt := &fakeImageRuntime{images: []docker.Image{oldImage("sha:a", "orphan:1")}}

	p := newPruner(newMemStore(), rt)
	p.After = func(time.Duration) <-chan time.Time { return tick }
	p.Record = func(rec activity.Record) { recorded <- rec }

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		p.RunSchedule(ctx, schedule)
	}()

	tick <- pruneNow

	rec := <-recorded

	cancel()
	<-done

	if rec.Action != activity.ActionImagePrune || rec.Origin != activity.OriginSchedule || rec.Status != activity.StatusSucceeded {
		t.Errorf("record: %+v", rec)
	}

	if len(rec.Resources) != 1 || rec.Resources[0].Name != "orphan:1" {
		t.Errorf("resources: %+v", rec.Resources)
	}
}

func TestRunScheduled_RecordsNothingWhenNothingHappened(t *testing.T) {
	p := newPruner(newMemStore(), &fakeImageRuntime{})

	p.Record = func(rec activity.Record) { t.Errorf("unexpected record: %+v", rec) }

	p.runScheduled(t.Context())
}

func TestImagePruneHandler(t *testing.T) {
	post := func(t *testing.T, api *API, query string) (*http.Response, envelope) {
		t.Helper()

		ts := httptest.NewServer(api.Handler())
		t.Cleanup(ts.Close)

		resp, err := http.Post(ts.URL+"/images/prune"+query, "application/json", strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}

		defer resp.Body.Close()

		var env envelope

		_ = json.NewDecoder(resp.Body).Decode(&env)

		return resp, env
	}

	t.Run("503 without a pruner", func(t *testing.T) {
		api, _ := newTestAPI(t)

		if resp, _ := post(t, api, ""); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status %d", resp.StatusCode)
		}
	})

	t.Run("400 on a bad keep, before anything runs", func(t *testing.T) {
		api, store := newTestAPI(t)
		rt := &fakeImageRuntime{images: []docker.Image{oldImage("sha:a", "orphan:1")}}
		api.ImagePruner = newPruner(store, rt)

		for _, q := range []string{"?keep=0", "?keep=-1", "?keep=abc"} {
			if resp, _ := post(t, api, q); resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s: status %d", q, resp.StatusCode)
			}
		}

		if len(rt.removed) != 0 {
			t.Errorf("removed %v", rt.removed)
		}
	})

	t.Run("dry_run reaches the pruner", func(t *testing.T) {
		api, store := newTestAPI(t)
		rt := &fakeImageRuntime{images: []docker.Image{oldImage("sha:a", "orphan:1")}}
		api.ImagePruner = newPruner(store, rt)

		if resp, _ := post(t, api, "?dry_run=true"); resp.StatusCode != http.StatusOK {
			t.Errorf("status %d", resp.StatusCode)
		}

		if len(rt.removed) != 0 {
			t.Errorf("removed %v", rt.removed)
		}
	})

	t.Run("runs and reports", func(t *testing.T) {
		api, store := newTestAPI(t)
		rt := &fakeImageRuntime{images: []docker.Image{oldImage("sha:a", "orphan:1")}}
		api.ImagePruner = newPruner(store, rt)

		resp, env := post(t, api, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}

		data, _ := json.Marshal(env.Data)

		if !strings.Contains(string(data), `"removed":["orphan:1"]`) {
			t.Errorf("data: %s", data)
		}
	})

	t.Run("409 while a run is active", func(t *testing.T) {
		api, store := newTestAPI(t)
		api.ImagePruner = newPruner(store, &fakeImageRuntime{})

		api.ImagePruner.mu.Lock()
		defer api.ImagePruner.mu.Unlock()

		if resp, _ := post(t, api, ""); resp.StatusCode != http.StatusConflict {
			t.Errorf("status %d", resp.StatusCode)
		}
	})
}

// Deleting images from a host is not a read: a read-only token must not reach
// the pruner.
func TestPATPlane_ReadScopeCannotPruneImages(t *testing.T) {
	store := newMemStore()

	plain, rec, _ := GeneratePAT([]Scope{ScopeRead}, "readonly")
	_ = store.PutPAT(t.Context(), rec)

	rt := &fakeImageRuntime{images: []docker.Image{oldImage("sha:a", "orphan:1")}}

	api := &API{Store: store, Version: "test", ImagePruner: newPruner(store, rt)}

	ts := httptest.NewServer(api.PATHandler(nil, 10, 3))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pat/v1/images/prune", nil)
	patSign(req, plain)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}

	if len(rt.removed) != 0 {
		t.Errorf("removed %v", rt.removed)
	}
}
