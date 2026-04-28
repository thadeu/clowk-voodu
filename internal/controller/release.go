package controller

import (
	crand "crypto/rand"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"go.voodu.clowk.in/internal/containers"
)

// newReleaseID returns a short sortable identifier for a release
// run. Format: base36(unix_seconds) + 2 hex random chars, ~9 chars
// total (e.g. "1ksdtcj7e"). Lexicographic order matches creation
// time — within ~70 years, base36 width stays at 7, so simple
// string sort gives newest-first ordering.
//
// No read-modify-write: each release independently mints its ID
// from the wall clock. Two concurrent releases on the same
// deployment hit different timestamps in 99.9999...% of cases;
// if they DID collide on the second, the lock prevents both from
// running anyway. Versions/counters were considered but rejected
// for the per-deployment race window when computing max+1.
func newReleaseID() string {
	sec := time.Now().Unix()
	ts := strconv.FormatInt(sec, 36)

	var rnd [1]byte
	_, _ = crand.Read(rnd[:])

	return ts + hex.EncodeToString(rnd[:])
}

// acquireReleaseLock TryLocks the per-deployment mutex. Returns the
// release function and true on acquisition; nil + false when another
// release for the same deployment is already in flight.
//
// Fail-fast (TryLock) over wait-and-queue because the alternative —
// blocking until the prior release finishes — would silently chain
// two migrations and surprise the operator. With fail-fast, the
// second invocation returns "already in progress" and the operator
// decides whether to wait, cancel, or investigate.
func (h *DeploymentHandler) acquireReleaseLock(app string) (release func(), acquired bool) {
	val, _ := h.releaseLocks.LoadOrStore(app, &sync.Mutex{})

	mu, _ := val.(*sync.Mutex)
	if mu == nil {
		// Defensive — sync.Map shouldn't return a nil value, but
		// any oddity (panic, race) shouldn't crash the handler.
		return func() {}, true
	}

	if !mu.TryLock() {
		return nil, false
	}

	return mu.Unlock, true
}


// releaseContainerName composes the docker name for a release-phase
// container. Distinct from the deployment's replica names so the
// reconciler never mistakes a release container for a deployment
// slot — release containers carry kind=job labels for the same
// reason: `vd get pods` lists them under the right kind, and the
// deployment's ListByIdentity(scope, name, kind=deployment) skips
// them.
//
// Shape: <scope>-<name>-release.<release_id>
//
// The "-release" suffix on the resource name is deliberate: it
// means `vd logs clowk-lp/web-release` brings up the release-run
// logs without polluting `vd logs clowk-lp/web` (the deploy itself).
func releaseContainerName(scope, name, releaseID string) string {
	resourceName := name + "-release"

	return containers.ContainerName(scope, resourceName, releaseID)
}

// runReleaseIfNeeded runs the release-phase pre + main commands
// inside a fresh container with the deployment's NEW image, BEFORE
// the rolling restart. Idempotent: skips when the release for this
// exact spec hash already succeeded (the rollback path re-applies
// a previous spec; we don't want to re-migrate a database that's
// already on schema X).
//
// On failure, records the outcome in DeploymentStatus.Releases and
// returns an error. Caller (recreateReplicasIfSpecChanged) bails out
// without touching replicas — old containers stay alive.
//
// `output` receives container stdout+stderr in real-time. The
// reconciler-triggered path passes io.Discard; HTTP-triggered runs
// pass the response body so CI sees migration output flow live.
func (h *DeploymentHandler) runReleaseIfNeeded(ctx context.Context, scope, name, app string, spec deploymentSpec, hash string, specSnapshot json.RawMessage, output io.Writer) error {
	releaseLock, acquired := h.acquireReleaseLock(app)
	if !acquired {
		return fmt.Errorf("release already in progress for deployment/%s", app)
	}

	defer releaseLock()

	// Idempotency: was this hash already released successfully?
	prev, _ := h.loadDeploymentStatus(ctx, app)

	for _, r := range prev.Releases {
		if r.Status == ReleaseStatusSucceeded && r.SpecHash == hash {
			h.logf("deployment/%s release for hash %s already succeeded (release %s); skipping",
				app, shortHash(hash), r.ID)

			return nil
		}
	}

	releaseID := newReleaseID()

	record := ReleaseRecord{
		ID:           releaseID,
		SpecHash:     hash,
		Image:        spec.Image,
		Status:       ReleaseStatusRunning,
		StartedAt:    time.Now().UTC(),
		SpecSnapshot: specSnapshot,
	}

	timeout := releaseTimeout(spec.Release.Timeout)

	// Pre-command first.
	if len(spec.Release.PreCommand) > 0 {
		exit, err := h.runReleaseCommand(ctx, scope, name, app, spec, releaseID+"-pre", spec.Release.PreCommand, timeout, output)
		record.PreExitCode = exit

		if err != nil {
			record.Status = ReleaseStatusFailed
			record.Error = fmt.Sprintf("pre_command: %v", err)
			record.EndedAt = time.Now().UTC()

			_ = h.appendReleaseRecord(ctx, app, record)

			return fmt.Errorf("pre_command exit %d: %w", exit, err)
		}
	}

	// Main command. The "real" release work — migrations, schema
	// updates, etc. Empty Command is allowed (operator might only
	// want pre + post hooks); skip the run, mark as succeeded.
	if len(spec.Release.Command) > 0 {
		exit, err := h.runReleaseCommand(ctx, scope, name, app, spec, releaseID, spec.Release.Command, timeout, output)
		record.ExitCode = exit

		if err != nil {
			record.Status = ReleaseStatusFailed
			record.Error = err.Error()
			record.EndedAt = time.Now().UTC()

			_ = h.appendReleaseRecord(ctx, app, record)

			return fmt.Errorf("release command exit %d: %w", exit, err)
		}
	}

	// pre + main both succeeded. Persist as "running" still — the
	// post_command (if any) and the rolling restart haven't happened
	// yet. The handler updates the record to Succeeded after both.
	// For simplicity in M-6 first cut: mark Succeeded here. If
	// post_command fails it gets a separate record.
	record.Status = ReleaseStatusSucceeded
	record.EndedAt = time.Now().UTC()

	if err := h.appendReleaseRecord(ctx, app, record); err != nil {
		// Persist failure: log only, don't abort the rollout. The
		// release process succeeded; status persistence is
		// best-effort.
		h.logf("deployment/%s release record persist failed: %v", app, err)
	}

	return nil
}

// runReleasePostCommand fires the optional post-rollout hook.
// Always runs in its own container with a fresh suffix so logs
// stay separate from the main release run. Failures are logged but
// don't fail the rollout — the new replicas are already live.
func (h *DeploymentHandler) runReleasePostCommand(ctx context.Context, scope, name, app string, spec deploymentSpec, hash string, output io.Writer) error {
	releaseID := containers.NewReplicaID() + "-post"

	timeout := releaseTimeout(spec.Release.Timeout)

	_, err := h.runReleaseCommand(ctx, scope, name, app, spec, releaseID, spec.Release.PostCommand, timeout, output)

	return err
}

// runReleaseCommand spawns a one-shot release container, streams
// its stdout+stderr to `output` as the process runs, waits for
// exit, and removes the container.
//
// Output streaming is the operator-friendly half: when called from
// the /releases/run HTTP handler, `output` is the chunked HTTP
// response body, so a CI runner sees migration logs flow in
// real-time. When called from the apply-time auto-trigger, `output`
// is io.Discard — apply-time releases run silently from the
// reconciler goroutine; the operator sees outcome via release
// status / history. Either way, the container is auto-cleaned on
// exit (explicit Remove instead of `--rm` so we still get the exit
// code from Wait without racing the daemon's auto-removal).
//
// Labels carry voodu.role=release alongside kind=job so callers
// can filter releases out of `vd get pods` if they want, while
// still getting the kind-aware machinery of jobs (logs, identity).
func (h *DeploymentHandler) runReleaseCommand(ctx context.Context, scope, deployName, app string, spec deploymentSpec, suffix string, command []string, timeout time.Duration, output io.Writer) (int, error) {
	if h.Containers == nil {
		return 1, fmt.Errorf("no container manager configured")
	}

	if output == nil {
		output = io.Discard
	}

	cname := releaseContainerName(scope, deployName, suffix)

	envFile := ""
	if h.EnvFilePath != nil {
		envFile = h.EnvFilePath(app)
	}

	if _, err := h.linkEnv(ctx, scope, deployName, app, spec.Env); err != nil {
		return 1, fmt.Errorf("link env: %w", err)
	}

	labels := containers.BuildLabels(containers.Identity{
		Kind:         containers.KindJob,
		Scope:        scope,
		Name:         deployName + "-release",
		ReplicaID:    suffix,
		ManifestHash: "release",
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	})

	// Add voodu.role=release as an extra label so operators can
	// filter releases distinctly from regular jobs (e.g. via
	// docker inspect --filter label=voodu.role=release). The
	// containers.Identity-driven labels keep kind=job for the
	// existing list/logs paths.
	labels = append(labels, "voodu.role=release")

	releaseSpec := ContainerSpec{
		Name:        cname,
		Image:       spec.Image,
		Command:     command,
		Volumes:     spec.Volumes,
		Networks:    spec.Networks,
		NetworkMode: spec.NetworkMode,
		EnvFile:     envFile,
		Labels:      labels,
		// AutoRemove is explicitly false: we need the container to
		// stay alive long enough for Wait to return the exit code.
		// `--rm` would race with Wait under heavy load. Equivalent
		// cleanup via the explicit Remove call below.
		AutoRemove: false,
		// TTY=true forces line-buffered stdout in the runtime
		// (Ruby/Node/Bun/Python/Go default to full-buffering when
		// stdout is a pipe). Without it, the operator only sees
		// migration logs in one big dump after the process exits —
		// defeating the realtime streaming we wire up below.
		TTY: true,
	}

	if err := h.Containers.Recreate(releaseSpec); err != nil {
		return 1, fmt.Errorf("spawn release container %s: %w", cname, err)
	}

	// Always remove the container at the end — success, failure,
	// timeout, ctx cancel. Mirrors --rm semantics without the race.
	defer func() { _ = h.Containers.Remove(cname) }()

	// Stream logs to `output` while the container runs. The stream
	// follows: it stays open until the process exits (kernel closes
	// pipe write end → docker's logs -f sees EOF → goroutine
	// returns). We block on Wait below, so the streaming completes
	// either before Wait returns or right after it.
	logsDone := make(chan struct{})

	go func() {
		defer close(logsDone)

		stream, err := h.Containers.Logs(cname, LogsOptions{Follow: true})
		if err != nil {
			fmt.Fprintf(output, "-----> Release: failed to open logs: %v\n", err)
			return
		}

		defer stream.Close()

		_, _ = io.Copy(output, stream)
	}()

	type waitResult struct {
		code int
		err  error
	}

	done := make(chan waitResult, 1)

	go func() {
		code, err := h.Containers.Wait(cname)
		done <- waitResult{code: code, err: err}
	}()

	select {
	case res := <-done:
		// Give the log streamer a moment to drain after the process
		// exits — without this, the last few lines of stdout can
		// arrive after we've already returned, defeating the
		// real-time UX.
		<-logsDone

		if res.err != nil {
			return res.code, fmt.Errorf("wait release: %w", res.err)
		}

		if res.code != 0 {
			return res.code, fmt.Errorf("container %s exited %d", cname, res.code)
		}

		return 0, nil

	case <-time.After(timeout):
		return 1, fmt.Errorf("release command timed out after %s", timeout)

	case <-ctx.Done():
		return 1, ctx.Err()
	}
}

// appendReleaseRecord prepends `r` to the deployment's release
// history, caps it at maxReleaseHistory, and GCs any release
// containers whose IDs no longer appear in the kept history. The
// container GC mirrors the job/cronjob pattern: stopped containers
// outside the keep set are removed; running ones (typically just
// the one we just spawned) are left alone.
//
// Newest-first matches the operator's "last release" intuition
// (`vd release status` shows index 0); the GC ensures `docker ps -a`
// doesn't grow unbounded after 100 deploys with release blocks.
func (h *DeploymentHandler) appendReleaseRecord(ctx context.Context, app string, r ReleaseRecord) error {
	prev, _ := h.loadDeploymentStatus(ctx, app)

	prev.Releases = append([]ReleaseRecord{r}, prev.Releases...)

	if len(prev.Releases) > maxReleaseHistory {
		prev.Releases = prev.Releases[:maxReleaseHistory]
	}

	raw, err := json.Marshal(prev)
	if err != nil {
		return err
	}

	if err := h.Store.PutStatus(ctx, KindDeployment, app, raw); err != nil {
		return err
	}

	// Garbage-collect release containers that fell off the cap.
	// Each release run produces up to 3 containers (pre, main, post),
	// each with a suffix-derived replica id; we keep every container
	// whose suffix prefix matches any in-history release ID.
	keep := make(map[string]bool, len(prev.Releases)*3)

	for _, rec := range prev.Releases {
		keep[rec.ID] = true
		keep[rec.ID+"-pre"] = true
		keep[rec.ID+"-post"] = true
	}

	scope, deployName := splitAppID(app)
	if h.Containers != nil {
		// Release containers carry kind=job and name=<deployName>-release.
		// Errors are logged but don't fail the release flow.
		if err := gcRunContainers(h.Containers, string(KindJob), scope, deployName+"-release", keep); err != nil {
			h.logf("deployment/%s release container GC failed: %v", app, err)
		}
	}

	return nil
}

// splitAppID is the inverse of AppID — splits the "<scope>-<name>"
// hyphen-joined string back into its parts. Best-effort: scope and
// resource names can both contain hyphens, so this picks the first
// hyphen as the boundary. Good enough for the GC path because the
// container labels carry scope and name explicitly; this only
// computes a hint for the GC's name filter.
func splitAppID(app string) (scope, name string) {
	for i, c := range app {
		if c == '-' {
			return app[:i], app[i+1:]
		}
	}

	return "", app
}

// loadDeploymentStatus reads the persisted DeploymentStatus blob.
// Returns a zero value when no status exists yet — first reconcile
// after apply, or after status was reset.
func (h *DeploymentHandler) loadDeploymentStatus(ctx context.Context, app string) (DeploymentStatus, error) {
	raw, err := h.Store.GetStatus(ctx, KindDeployment, app)
	if err != nil || raw == nil {
		return DeploymentStatus{}, err
	}

	var st DeploymentStatus

	if err := json.Unmarshal(raw, &st); err != nil {
		return DeploymentStatus{}, err
	}

	return st, nil
}

// releaseTimeout parses the manifest's release.timeout string into
// a time.Duration. Empty or malformed values fall back to the
// package default. Defensive: bad input shouldn't block the rollout.
func releaseTimeout(s string) time.Duration {
	if s == "" {
		return defaultReleaseTimeout
	}

	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return defaultReleaseTimeout
	}

	return d
}

// Release is the manual entry point for `vd release run <ref>`.
// Re-runs the release for the deployment's CURRENT spec, bypassing
// the idempotency check (operator chose to re-trigger). Useful when
// the migration was buggy and is fixed in code (image rebuilt
// under same tag) but the spec hash didn't move.
//
// `output` receives container logs in real-time. The /releases/run
// HTTP handler passes the response body so CI sees output flow
// live; reconciler-style auto-callers pass io.Discard.
func (h *DeploymentHandler) Release(ctx context.Context, scope, name string, output io.Writer) error {
	if output == nil {
		output = io.Discard
	}

	app := AppID(scope, name)

	releaseLock, acquired := h.acquireReleaseLock(app)
	if !acquired {
		return fmt.Errorf("release already in progress for deployment/%s", app)
	}

	defer releaseLock()

	manifest, err := h.Store.Get(ctx, KindDeployment, scope, name)
	if err != nil {
		return fmt.Errorf("read deployment manifest: %w", err)
	}

	if manifest == nil {
		return fmt.Errorf("deployment/%s/%s not found", scope, name)
	}

	spec, err := decodeDeploymentSpec(manifest)
	if err != nil {
		return err
	}

	if err := applyDeploymentSpecDefaults(&spec, app); err != nil {
		return err
	}

	if spec.Release == nil {
		return fmt.Errorf("deployment/%s has no release block", app)
	}

	hash := deploymentSpecHash(spec)

	// Reserved for symmetry with runReleaseIfNeeded — no-op here
	// since we don't gate on prior status.
	_, _ = h.loadDeploymentStatus(ctx, app)

	// Force-run by NOT checking for prior success.
	releaseID := newReleaseID()

	record := ReleaseRecord{
		ID:           releaseID,
		SpecHash:     hash,
		Image:        spec.Image,
		Status:       ReleaseStatusRunning,
		StartedAt:    time.Now().UTC(),
		SpecSnapshot: manifest.Spec,
	}

	timeout := releaseTimeout(spec.Release.Timeout)

	if len(spec.Release.PreCommand) > 0 {
		fmt.Fprintf(output, "-----> Release %s: pre_command\n", releaseID)

		exit, err := h.runReleaseCommand(ctx, scope, name, app, spec, releaseID+"-pre", spec.Release.PreCommand, timeout, output)
		record.PreExitCode = exit

		if err != nil {
			record.Status = ReleaseStatusFailed
			record.Error = fmt.Sprintf("pre_command: %v", err)
			record.EndedAt = time.Now().UTC()

			_ = h.appendReleaseRecord(ctx, app, record)

			fmt.Fprintf(output, "-----> Release %s failed in pre_command (exit %d)\n", releaseID, exit)

			return err
		}
	}

	if len(spec.Release.Command) > 0 {
		fmt.Fprintf(output, "-----> Release %s: command\n", releaseID)

		exit, err := h.runReleaseCommand(ctx, scope, name, app, spec, releaseID, spec.Release.Command, timeout, output)
		record.ExitCode = exit

		if err != nil {
			record.Status = ReleaseStatusFailed
			record.Error = err.Error()
			record.EndedAt = time.Now().UTC()

			_ = h.appendReleaseRecord(ctx, app, record)

			fmt.Fprintf(output, "-----> Release %s failed in command (exit %d)\n", releaseID, exit)

			return err
		}
	}

	// Rolling restart so replicas pick up whatever the release
	// changed (env, schema, image).
	live, err := h.Containers.ListByIdentity(string(KindDeployment), scope, name)
	if err != nil {
		record.Status = ReleaseStatusFailed
		record.Error = fmt.Sprintf("list replicas: %v", err)
		record.EndedAt = time.Now().UTC()

		_ = h.appendReleaseRecord(ctx, app, record)

		return err
	}

	if len(live) > 0 {
		fmt.Fprintf(output, "-----> Release %s: rolling restart of %d replica(s)\n", releaseID, len(live))

		if err := h.rollingReplaceReplicas(ctx, scope, name, app, live, spec, hash, releaseID); err != nil {
			record.Status = ReleaseStatusFailed
			record.Error = fmt.Sprintf("rolling restart: %v", err)
			record.EndedAt = time.Now().UTC()

			_ = h.appendReleaseRecord(ctx, app, record)

			return err
		}
	}

	// post_command after restart.
	if len(spec.Release.PostCommand) > 0 {
		fmt.Fprintf(output, "-----> Release %s: post_command\n", releaseID)

		exit, err := h.runReleaseCommand(ctx, scope, name, app, spec, releaseID+"-post", spec.Release.PostCommand, timeout, output)
		record.PostExitCode = exit

		if err != nil {
			// Post failure doesn't roll back, just records.
			h.logf("deployment/%s post_command failed: %v", app, err)

			fmt.Fprintf(output, "-----> Release %s post_command failed (exit %d) — replicas already live\n", releaseID, exit)
		}
	}

	record.Status = ReleaseStatusSucceeded
	record.EndedAt = time.Now().UTC()

	// No `succeeded` marker on stdout — the CLI's closing banner
	// (`-----> Released ... in Xs`) already announces success and
	// a redundant marker on the same row would visually
	// duplicate. Failure paths above DO write a `failed in ...`
	// banner because the CLI relies on the keyword to set its
	// exit code.

	return h.appendReleaseRecord(ctx, app, record)
}

// Rollback re-applies a specific past release's spec snapshot to
// the deployment. Heroku-style: operator picks the release ID
// (`vd rollback web 1ksdtcj7e`) and voodu re-Puts that snapshot
// into etcd. The reconciler triggers a normal recreate flow; the
// release runner's idempotency check short-circuits the migration
// re-run (target's hash is already Succeeded), so only the rolling
// restart happens.
//
// targetID="" means "previous succeeded" — pick the second-most-
// recent succeeded release (`heroku rollback` with no args).
// Otherwise pick the record matching the exact ID. Errors when
// the ID doesn't exist or isn't a Succeeded release.
//
// Returns the new release ID assigned to the rollback record.
// Rollbacks always mint a NEW ID — the timeline never reuses old
// IDs; the RolledBackFrom field links back to the origin so
// audits stay linear.
func (h *DeploymentHandler) Rollback(ctx context.Context, scope, name, targetID string) (string, error) {
	app := AppID(scope, name)

	releaseLock, acquired := h.acquireReleaseLock(app)
	if !acquired {
		return "", fmt.Errorf("release already in progress for deployment/%s", app)
	}

	defer releaseLock()

	current, err := h.Store.Get(ctx, KindDeployment, scope, name)
	if err != nil {
		return "", fmt.Errorf("read current manifest: %w", err)
	}

	if current == nil {
		return "", fmt.Errorf("deployment/%s/%s not found", scope, name)
	}

	prev, err := h.loadDeploymentStatus(ctx, app)
	if err != nil {
		return "", fmt.Errorf("read status: %w", err)
	}

	target, err := pickRollbackTarget(prev.Releases, targetID)
	if err != nil {
		return "", err
	}

	rollback := *current
	rollback.Spec = target.SpecSnapshot

	if _, err := h.Store.Put(ctx, &rollback); err != nil {
		return "", fmt.Errorf("apply rollback manifest: %w", err)
	}

	newID := newReleaseID()

	rollbackRecord := ReleaseRecord{
		ID:             newID,
		RolledBackFrom: target.ID,
		SpecHash:       target.SpecHash,
		Image:          target.Image,
		Status:         ReleaseStatusSucceeded,
		StartedAt:      time.Now().UTC(),
		EndedAt:        time.Now().UTC(),
		SpecSnapshot:   target.SpecSnapshot,
	}

	if err := h.appendReleaseRecord(ctx, app, rollbackRecord); err != nil {
		h.logf("deployment/%s rollback record persist failed: %v", app, err)
	}

	// Drive the rolling restart inline. Two reasons it can't be left
	// to the reconciler the way a vanilla `vd apply` works:
	//
	//   1. For deployments WITH a release block, the reconciler
	//      explicitly skips the restart (recreateReplicasIfSpecChanged
	//      logs "awaiting `vd release run`" and returns). Rollback
	//      IS the orchestrator for the rollback case — without an
	//      inline restart the snapshot lands in etcd but the running
	//      pods stay on the old spec, exactly the bug report:
	//      "rollback doesn't generate new pods".
	//
	//   2. For deployments without a release block, the reconciler
	//      WOULD restart on its own, but doing it inline here keeps
	//      the rollback synchronous — the caller can chain commands
	//      ("rollback then check pods") without racing against the
	//      reconciler tick.
	//
	// The release-phase commands (pre/main/post) are NOT re-run
	// because the target's hash already has a Succeeded record (set
	// at the original release time). Only the rolling restart fires.
	if err := h.rolloutRollback(ctx, scope, name, app, &rollback, target, newID); err != nil {
		h.logf("deployment/%s rollback rolling restart failed: %v", app, err)
		return newID, fmt.Errorf("rollback rolling restart: %w", err)
	}

	h.logf("deployment/%s rolled back to %s (new release %s)", app, target.ID, newID)

	return newID, nil
}

// rolloutRollback brings the running fleet to whatever the snapshot
// declared — image, env, command, AND replica count. Three phases:
//
//  1. Scale down: if the snapshot wants fewer replicas than are
//     currently live, prune the extras first. (`replicas=1` rolled
//     back from `replicas=3` would otherwise leave the operator
//     with three rolled-back replicas instead of one.)
//
//  2. Rolling-replace whatever's left: each surviving replica
//     swaps to a new container with the rolled-back image / env /
//     command. The target's release-phase commands DO NOT re-run
//     because the target's spec hash already has a Succeeded
//     record (idempotency).
//
//  3. Scale up: if the snapshot wants MORE replicas than are
//     currently live (rolling back a scale-down), ensure the
//     missing ones get spawned with the rolled-back spec.
//
// Status is baselined at the end so the reconciler's next pass
// sees the rolled-back hash and skips its own drift detection —
// without this, the reconciler would compute the snapshot hash,
// compare against the OLD persisted hash, and fire ANOTHER
// rolling restart for nothing.
//
// Doing the full scale dance inline (not relying on the
// reconciler picking up the Store.Put watch) makes Rollback
// synchronous and self-contained: when the function returns, the
// runtime already matches the snapshot. The reconciler's eventual
// watch event becomes a no-op confirmation.
func (h *DeploymentHandler) rolloutRollback(ctx context.Context, scope, name, app string, rollback *Manifest, target *ReleaseRecord, newReleaseID string) error {
	if h.Containers == nil {
		return nil
	}

	spec, err := decodeDeploymentSpec(rollback)
	if err != nil {
		return fmt.Errorf("decode rollback spec: %w", err)
	}

	if err := applyDeploymentSpecDefaults(&spec, app); err != nil {
		return fmt.Errorf("apply defaults: %w", err)
	}

	want := effectiveReplicas(spec)

	live, err := h.Containers.ListByIdentity(string(KindDeployment), scope, name)
	if err != nil {
		return fmt.Errorf("list replicas: %w", err)
	}

	// Phase 1: scale down before replacing. Pruning extras first
	// minimises churn — we don't want to rolling-replace replicas
	// we're about to remove anyway.
	if len(live) > want {
		if err := h.pruneExtraReplicas(name, app, live, want, nil); err != nil {
			return fmt.Errorf("scale down for rollback: %w", err)
		}

		// Re-list so Phase 2 sees the post-prune set.
		live, err = h.Containers.ListByIdentity(string(KindDeployment), scope, name)
		if err != nil {
			return fmt.Errorf("list replicas (post scale-down): %w", err)
		}
	}

	// Phase 2: rolling-replace whatever survived (or all of them
	// if no scale-down happened) so the rolled-back image/env/etc.
	// is what's actually running. New replicas carry the rollback's
	// own release_id (newReleaseID) so `vd describe` correlates
	// pods to "release te6kxxx (rollback)" instead of the rolled-
	// back-FROM target.
	if len(live) > 0 {
		if err := h.rollingReplaceReplicas(ctx, scope, name, app, live, spec, target.SpecHash, newReleaseID); err != nil {
			return err
		}
	}

	// Phase 3: scale up if the snapshot wants more replicas than
	// we currently have. Re-list because Phase 2 swapped names —
	// ensureReplicaCount uses len(live) to compute the delta.
	live, err = h.Containers.ListByIdentity(string(KindDeployment), scope, name)
	if err != nil {
		return fmt.Errorf("list replicas (post replace): %w", err)
	}

	if len(live) < want {
		if _, err := h.ensureReplicaCount(scope, name, app, live, want, spec, target.SpecHash, newReleaseID); err != nil {
			return fmt.Errorf("scale up for rollback: %w", err)
		}
	}

	if err := h.writeDeploymentStatus(ctx, app, spec.Image, target.SpecHash); err != nil {
		h.logf("deployment/%s rollback status persist failed: %v", app, err)
	}

	return nil
}

// pickRollbackTarget walks the release history and finds the
// candidate snapshot. Two modes:
//
//   - targetID != "": exact match. Errors when the ID doesn't
//     exist or isn't Succeeded (don't roll back to a known-broken
//     release on purpose).
//   - targetID == "": pick the second-most-recent succeeded
//     release (i.e., the release BEFORE the current one). Mirror
//     of `heroku rollback` with no args.
func pickRollbackTarget(history []ReleaseRecord, targetID string) (*ReleaseRecord, error) {
	if targetID != "" {
		for i := range history {
			r := history[i]

			if r.ID != targetID {
				continue
			}

			if r.Status != ReleaseStatusSucceeded {
				return nil, fmt.Errorf("release %s is %s, can only roll back to a succeeded release", targetID, r.Status)
			}

			if len(r.SpecSnapshot) == 0 {
				return nil, fmt.Errorf("release %s has no spec snapshot — cannot reconstruct manifest", targetID)
			}

			return &r, nil
		}

		return nil, fmt.Errorf("release %s not found in history", targetID)
	}

	// targetID == "" → previous succeeded release.
	succeededCount := 0

	for i := range history {
		r := history[i]

		if r.Status != ReleaseStatusSucceeded {
			continue
		}

		succeededCount++

		// First succeeded is the current; second is the target.
		if succeededCount == 2 && len(r.SpecSnapshot) > 0 {
			return &r, nil
		}
	}

	return nil, fmt.Errorf("no prior succeeded release to roll back to")
}

