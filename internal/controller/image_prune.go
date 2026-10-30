// image_prune.go removes the images this host no longer needs.
//
// `docker image prune -a` is NOT what this does, and cannot be: it deletes
// every image with no container, which is every <app>:<buildID> tag `vd
// rollback` retags from, and every cronjob image between two runs. So the
// pruner works from what voodu knows rather than from what docker sees.
//
// An image stays when ANY of these hold:
//
//   - a container references it, stopped ones included
//   - a stored manifest names it, at any depth of the spec (init containers
//     and plugin-expanded kinds carry their own `image` fields)
//   - it is <app>:latest of a resource that can run from a build
//   - it is one of the newest KeepReleases builds of a deployment or
//     statefulset, so rollback keeps working
//   - it was tagged within imagePruneGrace, so a build that finished seconds
//     ago is not deleted before its first container exists
//
// Fail closed: the keep set is the only thing standing between this code and a
// production image, so an error while building it stops the run before anything
// is deleted. And removal is never forced: if the daemon refuses, the image is
// reported as skipped and stays.
//
// With a --keep below the history cap, an older release stays in the history
// but loses its image, and rolling back to it fails with the clear "image not
// present locally" error from retagToTargetBuild.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"go.voodu.clowk.in/internal/activity"
	"go.voodu.clowk.in/internal/docker"
)

const (
	// DefaultImagePruneSchedule runs twice a day, in UTC. A fixed time of day
	// instead of "every 12h from start": a controller restarted by each
	// self-update would push an interval timer forward forever.
	DefaultImagePruneSchedule = "0 3,15 * * *"

	// defaultImagePruneKeep matches the history cap, so every release the
	// history lists can still roll back. A lower --keep trades that for disk.
	defaultImagePruneKeep = maxReleaseHistory

	imagePruneGrace = time.Hour

	// imagePruneTimeout bounds one run. The daemon deletes a few images a
	// second; ten minutes means something is wrong with the disk.
	imagePruneTimeout = 10 * time.Minute
)

// ErrImagePruneRunning answers a run that arrived during another run. The
// second one would only race the first for the same tags.
var ErrImagePruneRunning = errors.New("image prune already running")

// ImageRuntime is the docker surface the pruner needs. An interface so tests
// can drive every keep rule without a daemon.
type ImageRuntime interface {
	ListImages(ctx context.Context) ([]docker.Image, error)
	ContainerImageIDs(ctx context.Context) ([]string, error)
	ResolveImageID(ctx context.Context, ref string) (string, error)
	ImageLastTagged(ctx context.Context, id string) (time.Time, error)
	RemoveImage(ctx context.Context, ref string) error
	ImagesDiskSize(ctx context.Context) (int64, error)
}

// DockerImageRuntime is the production ImageRuntime.
type DockerImageRuntime struct{}

func (DockerImageRuntime) ListImages(ctx context.Context) ([]docker.Image, error) {
	return docker.ListImages(ctx)
}

func (DockerImageRuntime) ContainerImageIDs(ctx context.Context) ([]string, error) {
	return docker.ContainerImageIDs(ctx)
}

func (DockerImageRuntime) ResolveImageID(ctx context.Context, ref string) (string, error) {
	return docker.ResolveImageID(ctx, ref)
}

func (DockerImageRuntime) ImageLastTagged(ctx context.Context, id string) (time.Time, error) {
	return docker.ImageLastTagged(ctx, id)
}

func (DockerImageRuntime) RemoveImage(ctx context.Context, ref string) error {
	return docker.RemoveImage(ctx, ref)
}

func (DockerImageRuntime) ImagesDiskSize(ctx context.Context) (int64, error) {
	return docker.ImagesDiskSize(ctx)
}

// ImagePruneOptions tunes one run. KeepReleases <= 0 means the default.
type ImagePruneOptions struct {
	KeepReleases int
	DryRun       bool
}

// ImagePruneReport is what one run did. In a dry run Removed lists what would
// go, and ReclaimedBytes stays 0: the size of a set of images that share layers
// cannot be known without deleting them.
type ImagePruneReport struct {
	DryRun         bool     `json:"dry_run"`
	Scanned        int      `json:"scanned"`
	Kept           int      `json:"kept"`
	Removed        []string `json:"removed"`
	Skipped        []string `json:"skipped"`
	Errors         []string `json:"errors"`
	ReclaimedBytes int64    `json:"reclaimed_bytes"`
}

// ImagePruner owns the runs on this host: the scheduled ones and the ones an
// operator asks for, which share one lock.
type ImagePruner struct {
	Store  Store
	Images ImageRuntime
	Logger *log.Logger

	// Record writes a scheduled run to the activity trail. Manual runs reach
	// the trail through their HTTP handler.
	Record func(activity.Record)

	// Now and After are overridden in tests.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time

	mu sync.Mutex
}

// Prune runs one cleanup. An error means nothing was removed; a problem with a
// single image lands in the report instead, and the run continues.
func (p *ImagePruner) Prune(ctx context.Context, opts ImagePruneOptions) (ImagePruneReport, error) {
	if !p.mu.TryLock() {
		return ImagePruneReport{}, ErrImagePruneRunning
	}

	defer p.mu.Unlock()

	if opts.KeepReleases <= 0 {
		opts.KeepReleases = defaultImagePruneKeep
	}

	protected, err := p.protectedImages(ctx, opts.KeepReleases)
	if err != nil {
		return ImagePruneReport{}, err
	}

	images, err := p.Images.ListImages(ctx)
	if err != nil {
		return ImagePruneReport{}, err
	}

	report := ImagePruneReport{
		DryRun:  opts.DryRun,
		Scanned: len(images),
		Removed: []string{},
		Skipped: []string{},
		Errors:  []string{},
	}

	before := p.diskSize(ctx, opts.DryRun)

	for _, img := range images {
		if ctx.Err() != nil {
			report.Errors = append(report.Errors, ctx.Err().Error())
			break
		}

		if protected[img.ID] {
			report.Kept++
			continue
		}

		recent, err := p.recentlyTagged(ctx, img)
		if err != nil {
			report.Kept++
			report.Errors = append(report.Errors, err.Error())

			continue
		}

		if recent {
			report.Kept++
			continue
		}

		p.remove(ctx, img, opts.DryRun, &report)
	}

	if !opts.DryRun && len(report.Removed) > 0 {
		report.ReclaimedBytes = max(before-p.diskSize(ctx, false), 0)
	}

	return report, nil
}

// remove drops every tag of one image. A dangling image has no tag, so its ID
// is the only name it answers to.
func (p *ImagePruner) remove(ctx context.Context, img docker.Image, dryRun bool, report *ImagePruneReport) {
	refs := img.Tags
	if len(refs) == 0 {
		refs = []string{img.ID}
	}

	for _, ref := range refs {
		if dryRun {
			report.Removed = append(report.Removed, ref)
			continue
		}

		err := p.Images.RemoveImage(ctx, ref)

		switch {
		case err == nil:
			report.Removed = append(report.Removed, ref)
		case errors.Is(err, docker.ErrImageInUse):
			report.Skipped = append(report.Skipped, ref)
		default:
			report.Errors = append(report.Errors, err.Error())
		}
	}
}

// diskSize is reporting only, so a failure here must not cost the cleanup: the
// run reports 0 bytes reclaimed and still frees the disk.
func (p *ImagePruner) diskSize(ctx context.Context, dryRun bool) int64 {
	if dryRun {
		return 0
	}

	size, err := p.Images.ImagesDiskSize(ctx)
	if err != nil {
		p.logf("image prune: %v", err)
	}

	return size
}

func (p *ImagePruner) recentlyTagged(ctx context.Context, img docker.Image) (bool, error) {
	cutoff := p.now().Add(-imagePruneGrace)

	if img.Created.After(cutoff) {
		return true, nil
	}

	tagged, err := p.Images.ImageLastTagged(ctx, img.ID)
	if err != nil {
		return false, err
	}

	return tagged.After(cutoff), nil
}

// protectedImages resolves everything that must stay to image IDs, so an image
// is kept whichever of its tags something refers to.
func (p *ImagePruner) protectedImages(ctx context.Context, keep int) (map[string]bool, error) {
	containers, err := p.Images.ContainerImageIDs(ctx)
	if err != nil {
		return nil, err
	}

	protected := make(map[string]bool, len(containers))

	for _, id := range containers {
		protected[id] = true
	}

	refs, err := p.protectedRefs(ctx, keep)
	if err != nil {
		return nil, err
	}

	for ref := range refs {
		id, err := p.Images.ResolveImageID(ctx, ref)
		if err != nil {
			return nil, err
		}

		if id != "" {
			protected[id] = true
		}
	}

	return protected, nil
}

func (p *ImagePruner) protectedRefs(ctx context.Context, keep int) (map[string]bool, error) {
	manifests, err := p.Store.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list manifests: %w", err)
	}

	refs := map[string]bool{}

	for _, m := range manifests {
		if err := collectImageRefs(m.Spec, refs); err != nil {
			return nil, fmt.Errorf("%s/%s/%s: %w", m.Kind, m.Scope, m.Name, err)
		}

		app := AppID(m.Scope, m.Name)

		switch m.Kind {
		case KindJob, KindCronJob:
			refs[app+":latest"] = true

		case KindDeployment, KindStatefulset:
			refs[app+":latest"] = true

			releases, err := p.releases(ctx, m.Kind, app)
			if err != nil {
				return nil, err
			}

			retainedReleaseRefs(app, releases, keep, refs)
		}
	}

	return refs, nil
}

func (p *ImagePruner) releases(ctx context.Context, kind Kind, app string) ([]ReleaseRecord, error) {
	raw, err := p.Store.GetStatus(ctx, kind, app)
	if err != nil {
		return nil, fmt.Errorf("status of %s/%s: %w", kind, app, err)
	}

	if len(raw) == 0 {
		return nil, nil
	}

	var status DeploymentStatus

	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, fmt.Errorf("status of %s/%s: %w", kind, app, err)
	}

	return status.Releases, nil
}

// retainedReleaseRefs adds the images of the newest `keep` DISTINCT builds.
// Distinct because a config change or a rollback mints a record with the same
// build, and ten records of one build are one rollback target. A failed release
// is no rollback target and takes no slot, except the newest record, which is
// what is deploying right now whatever its status says.
func retainedReleaseRefs(app string, releases []ReleaseRecord, keep int, refs map[string]bool) {
	seen := map[string]bool{}

	for i, r := range releases {
		if i > 0 && r.Status != ReleaseStatusSucceeded {
			continue
		}

		build := r.BuildID
		if build == "" {
			build = r.Image
		}

		if build == "" || seen[build] {
			continue
		}

		if len(seen) == keep {
			return
		}

		seen[build] = true

		if r.BuildID != "" {
			refs[app+":"+r.BuildID] = true
		}

		if r.Image != "" {
			refs[r.Image] = true
		}
	}
}

// collectImageRefs walks a spec for `image` fields at any depth, so a kind that
// grows a new place to name an image is protected without this file changing.
func collectImageRefs(spec json.RawMessage, refs map[string]bool) error {
	if len(spec) == 0 {
		return nil
	}

	var doc any

	if err := json.Unmarshal(spec, &doc); err != nil {
		return err
	}

	walkImageRefs(doc, refs)

	return nil
}

func walkImageRefs(node any, refs map[string]bool) {
	switch v := node.(type) {
	case map[string]any:
		for key, value := range v {
			if s, ok := value.(string); ok && key == "image" && s != "" {
				refs[s] = true
				continue
			}

			walkImageRefs(value, refs)
		}

	case []any:
		for _, value := range v {
			walkImageRefs(value, refs)
		}
	}
}

// RunSchedule blocks until ctx is canceled, running a prune at each match of
// the schedule. Same semantics as CronScheduler: a time missed while the
// controller was down is skipped, not caught up.
func (p *ImagePruner) RunSchedule(ctx context.Context, schedule *Schedule) {
	after := p.After
	if after == nil {
		after = time.After
	}

	for {
		wait := schedule.Next(p.now()).Sub(p.now())

		select {
		case <-ctx.Done():
			return
		case <-after(wait):
			p.runScheduled(ctx)
		}
	}
}

func (p *ImagePruner) runScheduled(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, imagePruneTimeout)
	defer cancel()

	start := p.now()

	report, err := p.Prune(ctx, ImagePruneOptions{})
	if errors.Is(err, ErrImagePruneRunning) {
		p.logf("image prune: skipped, a manual run is in progress")
		return
	}

	rec := activity.Record{
		Event:     activity.EventDone,
		Action:    activity.ActionImagePrune,
		Origin:    activity.OriginSchedule,
		ElapsedMS: elapsedMS(start),
	}

	if err != nil {
		p.logf("image prune: %v", err)

		rec.Status = activity.StatusFailed
		rec.Error = err.Error()

		p.record(rec)

		return
	}

	p.logf("image prune: removed %d, skipped %d, kept %d, %d error(s), reclaimed %d bytes",
		len(report.Removed), len(report.Skipped), report.Kept, len(report.Errors), report.ReclaimedBytes)

	// Twice a day, every day: a run that found nothing is not an action
	// anyone took, and a trail full of them hides the ones that count.
	if len(report.Removed) == 0 && len(report.Errors) == 0 {
		return
	}

	report.annotate(&rec)

	p.record(rec)
}

// annotate writes the outcome of a run onto its activity record.
func (r ImagePruneReport) annotate(rec *activity.Record) {
	for _, ref := range r.Removed {
		rec.Resources = append(rec.Resources, activity.Resource{Kind: "image", Name: ref, Action: "deleted"})
	}

	rec.Warnings = r.Errors

	if len(r.Errors) == 0 {
		rec.Status = activity.StatusSucceeded
	} else {
		rec.Status = activity.StatusPartial
	}
}

func (p *ImagePruner) record(rec activity.Record) {
	if p.Record != nil {
		p.Record(rec)
	}
}

func (p *ImagePruner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}

	return time.Now()
}

func (p *ImagePruner) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
	}
}
