// handlers_deploy_run.go owns `POST /deploy/triggers/{id}/run` — the endpoint
// a control plane calls to deploy a commit.
//
// THIS IS WHERE THE INVARIANTS LAND. Everything else in the deploy plane is
// bookkeeping; this handler is what a stolen deploy token actually reaches.
//
//	I    The body carries a REF, never a manifest. If it accepted manifest
//	     content, whoever holds the token could apply an arbitrary container
//	     with a host mount — root, by another name. It is this rule, and not
//	     the token's scope, that prevents it.
//
//	II   Repository, branch and allowed scopes come from the trigger on this
//	     box; what to apply comes from a file in the repository. Nothing in
//	     the request widens either.
//
//	III  The commit must be an ancestor of the pinned branch. A fork's pull
//	     request lives in the same object store and is reachable by SHA, so
//	     without this check the token applies code nobody reviewed.
//
// Every one of those has a test named after it in handlers_deploy_run_test.go.

package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.voodu.clowk.in/internal/activity"
	"go.voodu.clowk.in/internal/triggerspec"
)

// ManifestParser turns a manifest file's bytes into manifests.
//
// Injected rather than imported: internal/manifest imports this package for
// the Manifest type, so depending on it here would be a cycle. Nil means this
// controller cannot deploy — which is a 503, not a panic, because a box
// without the parser wired is a misconfiguration and not a bug in the caller.
type ManifestParser func(r io.Reader, format string, vars map[string]string) ([]Manifest, error)

// deployRunRequest is what a control plane may say.
//
// INVARIANT I lives in this struct's SHAPE, not in a check. There is no field
// for manifest content, so there is nothing to validate and nothing a future
// edit can accidentally start honouring. A body carrying anything else is
// refused outright — see the DisallowUnknownFields below.
type deployRunRequest struct {
	// SHA is the commit to deploy. Validated as an ancestor of the trigger's
	// pinned branch before anything is read from it.
	SHA string `json:"sha"`

	// Ref is the git ref the push arrived on ("refs/heads/main"), used to
	// decide which trigger files fire. Optional: absent means the pinned
	// branch, which is what a manual run means.
	Ref string `json:"ref,omitempty"`

	// Manifest names WHICH trigger file to act on, for a repository that
	// declares several. Optional: absent runs every file whose `on` matches.
	Manifest string `json:"manifest,omitempty"`
}

type deployRunResponse struct {
	JobID   string   `json:"job_id"`
	Trigger string   `json:"trigger"`
	Repo    string   `json:"repo"`
	Commit  string   `json:"commit"`
	Applied []string `json:"applied"`
	Skipped []string `json:"skipped,omitempty"`

	// Resources is WHAT THIS COMMIT PUT ON THE BOX, as (kind, scope, name).
	//
	// Distinct from Applied, which names the trigger FILES that fired — "PWA",
	// "API". Those are labels a human chose; these are the things a console can
	// link to. Without them a deployment screen can say a deploy succeeded and
	// cannot say what it touched, which is the question anybody looking at one
	// is actually asking.
	//
	// This is also what the original sketch wanted a whole DeployJob record
	// for. It comes from the manifests already parsed to be applied, so it
	// costs nothing and cannot disagree with what ran.
	Resources []DeployedResource `json:"resources,omitempty"`
}

// DeployedResource is one thing a deploy applied.
//
// Scope and Name are the pair every path on the box is keyed by, so a console
// holding these can link straight to the pod without a lookup table.
type DeployedResource struct {
	Kind  string `json:"kind"`
	Scope string `json:"scope,omitempty"`
	Name  string `json:"name"`
}

// handleDeployRun deploys a commit.
//
// Synchronous, and returning 200 rather than the 202 the original sketch had:
// the apply is fast (it writes desired state; the reconciler does the slow
// part), and a caller that has to poll for the outcome of a call it just made
// is a caller that will forget to. The activity trail carries the long tail.
func (a *API) handleDeployRun(w http.ResponseWriter, r *http.Request) {
	if a.ParseManifests == nil {
		writeErr(w, http.StatusServiceUnavailable,
			fmt.Errorf("this controller has no manifest parser wired and cannot deploy"))

		return
	}

	trigger, ok := a.resolveTriggerFromPath(w, r)
	if !ok {
		return
	}

	if !trigger.Enabled {
		writeErr(w, http.StatusConflict,
			fmt.Errorf("trigger %s is paused — `vd deploy trigger enable %s` on the box", trigger.ID, trigger.ID))

		return
	}

	req, ok := decodeRunRequest(w, r)
	if !ok {
		return
	}

	token := strings.TrimSpace(r.Header.Get(GitHubTokenHeader))
	if token == "" {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("%s is required — this box has no GitHub credentials of its own", GitHubTokenHeader))

		return
	}

	// INVARIANT III, before a single byte of the repository is read. Reading
	// first and checking later would mean an unreviewed commit's files had
	// already been fetched, parsed and expanded on the box.
	client := a.githubClient()

	ancestor, err := client.IsAncestor(r.Context(), token, trigger.Repo, trigger.Branch, req.SHA)
	if err != nil {
		writeGitHubErr(w, err)

		return
	}

	if !ancestor {
		writeErr(w, http.StatusForbidden,
			fmt.Errorf("commit %s is not an ancestor of %s — a commit that was never on the pinned branch cannot be deployed",
				short(req.SHA), trigger.Branch))

		return
	}

	// One snapshot for the whole request: the trigger files, the manifest and
	// the subtree SHAs all come out of the same commit and the same tree
	// listing. Fetching them separately was two wasted calls per deploy.
	snap, err := a.snapshotRepo(r, trigger.Repo, req.SHA, token)
	if err != nil {
		writeGitHubErr(w, err)

		return
	}

	specs, ok := a.selectTriggerSpecs(w, r, trigger, req, token, snap)
	if !ok {
		return
	}

	out := deployRunResponse{
		JobID:   NewActivityID(),
		Trigger: trigger.ID,
		Repo:    trigger.Repo,
		Commit:  req.SHA,
	}

	for _, spec := range specs {
		resources, err := a.applyFromRepo(w, r, trigger, req, token, snap, spec)
		if err != nil {
			return
		}

		out.Applied = append(out.Applied, spec.Name)
		out.Resources = append(out.Resources, resources...)
	}

	a.touchTrigger(r, trigger)

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: out})
}

// selectTriggerSpecs reads the repository's trigger files and returns the ones
// this run should act on.
func (a *API) selectTriggerSpecs(
	w http.ResponseWriter, r *http.Request, trigger *Trigger, req deployRunRequest,
	token string, snap repoSnapshot,
) ([]triggerspec.Spec, bool) {
	found := a.manifestsFromSnapshot(r, trigger.Repo, req.SHA, token, snap)

	ref := req.Ref
	if ref == "" {
		ref = "refs/heads/" + trigger.Branch
	}

	var selected []triggerspec.Spec

	for _, file := range found.Files {
		if file.Spec == nil {
			// A broken file is not a reason to refuse the whole deploy: the
			// other files are still valid, and refusing everything would let
			// one typo stop a repository deploying at all.
			continue
		}

		if req.Manifest != "" && file.Path != req.Manifest {
			continue
		}

		if req.Manifest == "" && !file.Spec.MatchesRef(ref) {
			continue
		}

		selected = append(selected, *file.Spec)
	}

	if len(selected) == 0 {
		writeErr(w, http.StatusUnprocessableEntity,
			fmt.Errorf("no trigger file in %s matches %s at %s", triggerspec.Dir, ref, short(req.SHA)))

		return nil, false
	}

	return selected, true
}

// applyFromRepo fetches one manifest, checks its scopes and applies it.
//
// Writes its own error response and returns the error so the caller stops —
// a partial deploy across several trigger files is a state nobody described.
// resourcesOf names what a set of manifests puts on the box.
//
// Deduplicated on (kind, scope, name): a manifest file may declare the same
// resource twice through composition, and a console listing it twice would
// suggest two things were deployed.
func resourcesOf(manifests []Manifest) []DeployedResource {
	out := make([]DeployedResource, 0, len(manifests))
	seen := make(map[DeployedResource]bool, len(manifests))

	for _, m := range manifests {
		res := DeployedResource{Kind: string(m.Kind), Scope: m.Scope, Name: m.Name}

		if seen[res] {
			continue
		}

		seen[res] = true

		out = append(out, res)
	}

	return out
}

// applyFromRepo applies one trigger file, and answers with WHAT it applied.
//
// Returning the resources rather than only an error: the caller is building a
// response a console reads, and "the deploy worked" without "here is what it
// touched" is the half that leaves somebody guessing. They come from the
// manifests parsed a few lines below, so the answer cannot drift from what ran.
func (a *API) applyFromRepo(
	w http.ResponseWriter, r *http.Request, trigger *Trigger,
	req deployRunRequest, token string, snap repoSnapshot, spec triggerspec.Spec,
) ([]DeployedResource, error) {
	raw, err := a.fetchRepoFile(r, trigger.Repo, token, snap, spec.Apply.File)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity,
			fmt.Errorf("%s names apply.file %q, which is not in the repository at %s",
				spec.Name, spec.Apply.File, short(req.SHA)))

		return nil, err
	}

	manifests, err := a.ParseManifests(bytes.NewReader(raw), formatFor(spec.Apply.File), nil)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, fmt.Errorf("%s: %w", spec.Apply.File, err))

		return nil, err
	}

	if len(manifests) == 0 {
		err := fmt.Errorf("%s declares no resources", spec.Apply.File)
		writeErr(w, http.StatusUnprocessableEntity, err)

		return nil, err
	}

	// INVARIANT II, applied to what will ACTUALLY be applied.
	//
	// The scopes come out of the parsed manifests, not out of a declaration
	// beside them — a file claiming one scope while declaring another would
	// otherwise pass. Every refusal is reported at once so an operator
	// widening a trigger does it in one pass instead of one deploy per scope.
	// Scopes BEFORE the build. Building first would mean an unauthorised
	// manifest still got its image built and its release directory written on
	// the box — work done on behalf of a deploy that was always going to be
	// refused.
	if refused := trigger.RefusedScopes(scopesOf(manifests)); len(refused) > 0 {
		err := fmt.Errorf("%s applies to scope(s) %s, which trigger %s does not allow (allowed: %s)",
			spec.Apply.File, strings.Join(refused, ", "), trigger.ID, strings.Join(trigger.AllowScopes, ", "))

		writeErr(w, http.StatusForbidden, err)

		return nil, err
	}

	// Build mode: the workload names no image, so one is produced here from
	// the repository's source BEFORE the manifest is applied.
	//
	// Before, and not after, for the reason the force-pull path already
	// documents: the apply's watch event has to find the tag already
	// re-pointed, or the reconciler decides there is no drift and the new
	// image waits for a second deploy.
	targets := buildTargets(manifests)

	if len(targets) > 0 {
		if a.BuildFromSource == nil {
			err := fmt.Errorf(
				"%s builds from source (%s), and this controller has no builder wired",
				spec.Apply.File, targets[0].Ref)

			writeErr(w, http.StatusServiceUnavailable, err)

			return nil, err
		}

		if err := a.buildTargetsFromRepo(r, trigger, req, token, snap, targets); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err)

			return nil, err
		}
	}

	if err := a.applyManifestsInProcess(w, r, trigger, req, spec, manifests); err != nil {
		return nil, err
	}

	return resourcesOf(manifests), nil
}

// buildTargetsFromRepo builds the workloads whose source actually changed.
//
// THE DOWNLOAD IS DECIDED BEFORE IT HAPPENS. Each build context is a directory,
// and a directory's git SHA changes only when something inside it changed — so
// comparing the snapshot's subtree SHAs against what was last built answers
// "does anything need building" from data the request already holds. A commit
// that touched only the README skips the archive entirely.
//
// The buildID dedup inside the pipeline still exists and still works; it just
// runs AFTER the download, so on its own it saves the build and not the
// transfer. This is the half that saves the transfer.
//
// One download for N workloads: the archive is the expensive part, and a
// monorepo deploying three services pays for it once.
func (a *API) buildTargetsFromRepo(
	r *http.Request, trigger *Trigger, req deployRunRequest,
	token string, snap repoSnapshot, targets []buildTarget,
) error {
	stale, current := staleTargets(trigger, snap, targets)

	if len(stale) == 0 {
		// Nothing to build and nothing to download. The manifest still gets
		// applied by the caller — a deploy that changes only configuration is
		// still a deploy.
		return nil
	}

	archive, err := a.githubClient().Tarball(r.Context(), token, trigger.Repo, req.SHA)
	if err != nil {
		return fmt.Errorf("could not download %s at %s: %w", trigger.Repo, short(req.SHA), err)
	}

	defer archive.Close()

	root, cleanup, err := extractRepoArchive(a.BuildRoot, archive)

	// Cleanup runs even when extraction failed: a partial repository still
	// occupies disk, and a box that keeps one per failed deploy fills quietly.
	defer cleanup()

	if err != nil {
		return err
	}

	built := map[string]string{}

	for _, target := range stale {
		if err := a.buildOne(root, target, false); err != nil {
			// The successful builds so far are still recorded: they DID happen,
			// and forgetting them would rebuild them on the retry.
			a.rememberBuilt(r, trigger, built)

			return err
		}

		if sha, ok := current[target.Path]; ok {
			built[target.Path] = sha
		}
	}

	a.rememberBuilt(r, trigger, built)

	return nil
}

// staleTargets splits the targets into those needing a build and the subtree
// SHAs to record once they succeed.
//
// A target whose path is NOT in the snapshot is treated as stale rather than
// skipped: the build will fail with "that directory is not in this commit",
// which names the problem, whereas silently skipping would report a successful
// deploy that built nothing.
func staleTargets(trigger *Trigger, snap repoSnapshot, targets []buildTarget) ([]buildTarget, map[string]string) {
	stale := make([]buildTarget, 0, len(targets))
	current := make(map[string]string, len(targets))

	for _, target := range targets {
		sha, found := snap.subtreeSHA(target.Path)
		if !found {
			stale = append(stale, target)

			continue
		}

		current[target.Path] = sha

		if trigger.LastBuilt[target.Path] != sha {
			stale = append(stale, target)
		}
	}

	return stale, current
}

// rememberBuilt records which subtree each context was last built from.
//
// Best-effort, like the fired-at stamp: a build that worked must not be
// reported as failed because a bookkeeping write did not. The cost of losing
// it is one redundant download on the next deploy, not a wrong deploy.
func (a *API) rememberBuilt(r *http.Request, trigger *Trigger, built map[string]string) {
	if len(built) == 0 {
		return
	}

	updated := *trigger

	updated.LastBuilt = make(map[string]string, len(trigger.LastBuilt)+len(built))

	for path, sha := range trigger.LastBuilt {
		updated.LastBuilt[path] = sha
	}

	for path, sha := range built {
		updated.LastBuilt[path] = sha
	}

	if err := a.Store.PutTrigger(r.Context(), updated); err == nil {
		// Kept in sync so a later write in this same request — the fired-at
		// stamp — does not overwrite what was just recorded.
		*trigger = updated
	}
}

// applyManifestsInProcess hands the manifests to the SAME applyPost every
// other apply goes through.
//
// One apply path, not a second implementation. Plugin expansion, asset digest
// stamping, ingress host collision checks and the activity trail all live in
// applyPost, and a deploy that skipped any of them would be a deploy that
// behaves differently from `vd apply` for reasons nobody could see.
//
// The synthesised request carries the deploy plane's own origin and the commit,
// so the trail says where this came from.
func (a *API) applyManifestsInProcess(
	w http.ResponseWriter, r *http.Request, trigger *Trigger,
	req deployRunRequest, spec triggerspec.Spec, manifests []Manifest,
) error {
	body, err := json.Marshal(manifests)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return err
	}

	inner, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/apply", bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return err
	}

	inner.Header.Set("Content-Type", "application/json")
	inner.Header.Set(activity.OriginHeader, string(activity.OriginDeployPlane))
	inner.Header.Set(deployCommitHeader, req.SHA)
	inner.Header.Set(deployTriggerHeader, trigger.ID)
	inner.Header.Set(deployNameHeader, spec.Name)

	// The recorder captures applyPost's answer so a failure becomes THIS
	// endpoint's failure rather than two responses on one connection.
	rec := &captureWriter{header: http.Header{}}

	a.applyPost(rec, inner)

	if rec.status >= 400 {
		writeErr(w, rec.status, fmt.Errorf("apply of %s failed: %s", spec.Apply.File, rec.errorText()))

		return fmt.Errorf("apply failed")
	}

	return nil
}

// fetchRepoFile reads one file out of a snapshot already in hand.
//
// No tarball. A registry-mode deploy needs the manifest and nothing else, and
// pulling a repository's whole working tree to read a few kilobytes of HCL is
// a cost with no purchase. Build mode, which genuinely needs the source, is
// where the tarball earns its download.
func (a *API) fetchRepoFile(r *http.Request, repo, token string, snap repoSnapshot, filePath string) ([]byte, error) {
	blob, ok := snap.blobSHA(filePath)
	if !ok {
		return nil, fmt.Errorf("no %s at %s", filePath, short(snap.Commit.SHA))
	}

	return a.githubClient().GetBlob(r.Context(), token, repo, blob)
}

// touchTrigger records that this trigger fired. Best-effort: a deploy that
// worked must not be reported as failed because a bookkeeping write did not.
func (a *API) touchTrigger(r *http.Request, trigger *Trigger) {
	now := time.Now().UTC()
	updated := *trigger
	updated.LastFiredAt = &now

	_ = a.Store.PutTrigger(r.Context(), updated)
}

func (a *API) resolveTriggerFromPath(w http.ResponseWriter, r *http.Request) (*Trigger, bool) {
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("trigger id is required"))

		return nil, false
	}

	trigger, err := a.Store.GetTrigger(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return nil, false
	}

	if trigger == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no trigger %q", id))

		return nil, false
	}

	return trigger, true
}

// decodeRunRequest reads the body with DisallowUnknownFields.
//
// INVARIANT I is enforced HERE, and strictly: a body carrying `manifests`,
// `spec` or anything else this endpoint does not name is refused rather than
// ignored. Ignoring would be safe today and would stop being safe the moment
// somebody adds a field with a matching name.
func decodeRunRequest(w http.ResponseWriter, r *http.Request) (deployRunRequest, bool) {
	raw, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return deployRunRequest{}, false
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	var req deployRunRequest

	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("this endpoint accepts a commit to deploy, never manifest content: %w", err))

		return deployRunRequest{}, false
	}

	req.SHA = strings.TrimSpace(req.SHA)

	if !looksLikeSHA(req.SHA) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("sha must be a full 40-character commit id"))

		return deployRunRequest{}, false
	}

	return req, true
}

// looksLikeSHA insists on the full 40 hex characters.
//
// An abbreviated SHA is ambiguous by construction, and this value is compared
// against a branch's history and then interpolated into API paths. "Probably
// that commit" is not a basis for deciding what runs in production.
func looksLikeSHA(s string) bool {
	if len(s) != 40 {
		return false
	}

	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}

	return sha
}

// scopesOf collects the distinct scopes a manifest set declares, in order.
func scopesOf(manifests []Manifest) []string {
	seen := map[string]bool{}
	out := []string{}

	for i := range manifests {
		scope := manifests[i].Scope

		if seen[scope] {
			continue
		}

		seen[scope] = true

		out = append(out, scope)
	}

	return out
}

// formatFor picks the parser format from the file's extension. Anything that
// is not .json is HCL — the manifest language.
func formatFor(filePath string) string {
	if strings.HasSuffix(strings.ToLower(filePath), ".json") {
		return "json"
	}

	return "hcl"
}

// Headers the synthesised apply carries so the activity trail knows where the
// deploy came from.
const (
	deployCommitHeader  = "X-Voodu-Deploy-Commit"
	deployTriggerHeader = "X-Voodu-Deploy-Trigger"
	deployNameHeader    = "X-Voodu-Deploy-Name"
)

// captureWriter collects an in-process handler's response instead of writing
// it to the wire, so this endpoint can turn a failed apply into its own error
// rather than emitting two responses on one connection.
type captureWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}

	return c.body.Write(b)
}

// errorText pulls the message out of the envelope applyPost produced.
func (c *captureWriter) errorText() string {
	var env struct {
		Error string `json:"error"`
	}

	if err := json.Unmarshal(c.body.Bytes(), &env); err == nil && env.Error != "" {
		return env.Error
	}

	return strings.TrimSpace(c.body.String())
}
