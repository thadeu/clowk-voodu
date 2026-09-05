// handlers_deploy_manifests.go owns `GET /deploy/manifests` and
// `GET /deploy/preflight` — the two reads that let a control plane show what
// this box would do, before it does any of it.
//
// THE CONSOLE ASKS THE BOX, NOT GITHUB. That is the whole point of these
// living here. A screen that read `.voodu/*.yml` from the GitHub API directly
// could show a file the box would never use: a different ref, a different
// token's visibility, a permission that drifted. A screen showing config that
// is not the config that runs is worse than no screen, because it is believed.
//
// Each file comes back WITH THE BOX'S VERDICT — valid, or the exact reason it
// was refused. The caller does not re-validate. A second opinion that disagrees
// with the first is how a screen starts lying.

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"

	"go.voodu.clowk.in/internal/github"
	"go.voodu.clowk.in/internal/triggerspec"
)

// GitHubTokenHeader carries the short-lived, repository-scoped token the
// control plane minted for this request.
//
// A header and not a query parameter: a URL ends up in access logs, browser
// history and error reports, and this value opens a repository for an hour.
const GitHubTokenHeader = "X-Voodu-GitHub-Token"

// maxTriggerFiles bounds how many trigger files one repository may declare.
//
// A shape check, not a resource limit. A repository with hundreds of these is
// one where a generator ran away, and finding out at four hundred is worse
// than being told at twenty.
const maxTriggerFiles = 20

// ManifestFile is one `.voodu/**/*.yml` as the box read it.
type ManifestFile struct {
	Path string `json:"path"`

	// Spec is present when the file parsed and validated.
	Spec *triggerspec.Spec `json:"spec,omitempty"`

	// Error is the exact reason this file was refused, when it was. Exactly
	// one of Spec and Error is set — a file is either usable or explained.
	Error string `json:"error,omitempty"`
}

// RepoStats is what the box can say about a repository without spending a
// single extra request.
//
// Every number here comes from the tree listing the trigger files were found
// in: each blob entry carries its size, so counting them is arithmetic on data
// already in hand. That is the whole reason these are the metrics offered and
// not others — a screen that costs an API call per visit is a screen that gets
// throttled on the day somebody leaves it open.
//
// It measures the WORKING TREE, not the repository. GitHub's own `size` field
// counts git objects including history, which is not what a deploy downloads
// and not what a build reads.
type RepoStats struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`

	// Languages is bytes per file extension, largest first.
	//
	// Derived from the same listing rather than from GitHub's /languages
	// endpoint, which would be an extra call for an answer of the same shape.
	// It is a proxy: extension is not language, and a `.h` shared by C and C++
	// is counted once under `.h`. Named for what it is measured from.
	Languages []LanguageBytes `json:"languages,omitempty"`
}

// LanguageBytes is one extension's share of the working tree.
type LanguageBytes struct {
	Ext   string `json:"ext"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
}

type manifestsResponse struct {
	Repo   string         `json:"repo"`
	Ref    string         `json:"ref"`
	Commit string         `json:"commit"`
	Stats  RepoStats      `json:"stats"`
	Files  []ManifestFile `json:"files"`

	// Truncated says the repository was too large for one tree listing and we
	// read `.voodu/` on its own instead. Reported rather than hidden: a caller
	// that cannot tell a complete answer from a partial one will present the
	// partial one as complete.
	Truncated bool `json:"truncated,omitempty"`
}

// handleDeployManifests reads the trigger files a repository declares.
//
//	GET /deploy/manifests?trigger=<id>[&ref=<branch|sha>]
//
// `ref` defaults to the trigger's pinned branch. Passing one is for previewing
// a change before it merges; it does NOT widen what may be deployed, because
// nothing here deploys.
func (a *API) handleDeployManifests(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.Header.Get(GitHubTokenHeader))
	if token == "" {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("%s is required — this box has no GitHub credentials of its own", GitHubTokenHeader))

		return
	}

	repo, ref, ok := a.resolveManifestTarget(w, r, token)
	if !ok {
		return
	}

	result, err := a.readManifests(r, repo, ref, token)
	if err != nil {
		writeGitHubErr(w, err)

		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: result})
}

// resolveManifestTarget works out which repository and ref to read.
//
// TWO WAYS IN, and the second is what the connect flow needs:
//
//	?trigger=<id>          an already-authorised repository; ref defaults to
//	                       the trigger's pinned branch
//	?repo=owner/name       a repository with NO trigger yet; ref defaults to
//	                       the repository's default branch
//
// The second is a READ and only a read. Nothing here applies anything, and
// what bounds it is the token — scoped by GitHub to the repositories the
// customer selected when they installed the App. A box asked to read a
// repository outside that returns the same 404 GitHub gives for one that does
// not exist.
//
// It exists because of the order the customer works in: they connect GitHub,
// pick repositories, and want to see whether their `.voodu/*.yml` is valid
// BEFORE authorising a trigger. Requiring a trigger first would mean granting
// scopes to find out whether the thing is configured at all.
func (a *API) resolveManifestTarget(w http.ResponseWriter, r *http.Request, token string) (string, string, bool) {
	query := r.URL.Query()
	triggerID := strings.TrimSpace(query.Get("trigger"))
	repo := strings.TrimSpace(query.Get("repo"))
	ref := strings.TrimSpace(query.Get("ref"))

	// Both is not a merge, it is a contradiction: the two carry different
	// repositories and different defaults, and silently preferring one would
	// answer a question the caller did not ask.
	if triggerID != "" && repo != "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("pass trigger or repo, not both"))

		return "", "", false
	}

	if triggerID == "" && repo == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("trigger or repo is required"))

		return "", "", false
	}

	if triggerID != "" {
		trigger, ok := a.resolveTrigger(w, r)
		if !ok {
			return "", "", false
		}

		if ref == "" {
			ref = trigger.Branch
		}

		return trigger.Repo, ref, true
	}

	repo = strings.ToLower(repo)

	// Strict, because this is interpolated into GitHub API URLs. A value with
	// an extra slash or a `..` addresses a different endpoint than the code
	// reads like it addresses.
	if err := validateRepo(repo); err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return "", "", false
	}

	if ref != "" {
		return repo, ref, true
	}

	// No ref given — a FALLBACK, not the normal path.
	//
	// The console already knows this: `GET /installation/repositories`, which
	// it calls to list the authorised repositories, returns `default_branch`
	// on each one. It should pass `?ref=` from what it already holds rather
	// than making this box ask again, on a screen that opens several times a
	// day. The fallback exists for callers without that listing — curl, a
	// test, a future `vd deploy preview`.
	//
	// Asked rather than guessed, because guessing `main` is wrong for every
	// repository still on `master`, and it fails SILENTLY: an empty file list
	// reads as "no triggers configured" rather than "wrong branch".
	//
	// This is not the branch from the YAML, and not the trigger's pinned
	// branch. Those answer different questions — `on.push.branches` says which
	// pushes fire, the pinned branch says what a commit must descend from —
	// and neither can be read before something says where to read from.
	meta, err := a.githubClient().GetRepo(r.Context(), token, repo)
	if err != nil {
		writeGitHubErr(w, err)

		return "", "", false
	}

	if meta.DefaultBranch == "" {
		writeErr(w, http.StatusBadGateway,
			fmt.Errorf("GitHub did not report a default branch for %s; pass ?ref= explicitly", repo))

		return "", "", false
	}

	return repo, meta.DefaultBranch, true
}

// repoSnapshot is one repository at one commit: the commit and its full tree
// listing, fetched together.
//
// Threaded through a request rather than re-fetched, which is what this code
// used to do — reading the trigger files fetched the commit and the tree, then
// reading the manifest fetched the same commit and the same tree again. Two
// wasted calls per deploy, and another two per extra trigger file, for an
// answer already in hand.
type repoSnapshot struct {
	Commit github.Commit
	Tree   github.Tree
}

// blobSHA finds a file's blob in the snapshot.
func (s repoSnapshot) blobSHA(filePath string) (string, bool) {
	want := strings.TrimPrefix(filePath, "./")

	for _, entry := range s.Tree.Entries {
		if entry.Type == "blob" && entry.Path == want {
			return entry.SHA, true
		}
	}

	return "", false
}

// subtreeSHA returns the SHA of a directory in the snapshot.
//
// A directory's SHA changes only when something INSIDE it changed, which is
// what lets a deploy decide whether a build context moved without downloading
// anything.
//
// "." and "" are the repository root, whose SHA is the commit's own tree — the
// default a build block with no path means.
func (s repoSnapshot) subtreeSHA(dirPath string) (string, bool) {
	clean := strings.Trim(strings.TrimPrefix(dirPath, "./"), "/")

	if clean == "" || clean == "." {
		return s.Commit.TreeSHA(), true
	}

	for _, entry := range s.Tree.Entries {
		if entry.Type == "tree" && entry.Path == clean {
			return entry.SHA, true
		}
	}

	return "", false
}

func (a *API) snapshotRepo(r *http.Request, repo, ref, token string) (repoSnapshot, error) {
	client := a.githubClient()
	ctx := r.Context()

	commit, err := client.GetCommit(ctx, token, repo, ref)
	if err != nil {
		return repoSnapshot{}, err
	}

	tree, err := client.GetTree(ctx, token, repo, commit.TreeSHA())
	if err != nil {
		return repoSnapshot{}, err
	}

	return repoSnapshot{Commit: commit, Tree: tree}, nil
}

func (a *API) readManifests(r *http.Request, repo, ref, token string) (manifestsResponse, error) {
	snap, err := a.snapshotRepo(r, repo, ref, token)
	if err != nil {
		return manifestsResponse{}, err
	}

	return a.manifestsFromSnapshot(r, repo, ref, token, snap), nil
}

// manifestsFromSnapshot reads the trigger files out of a snapshot already
// fetched, so a caller that needs the tree for its own reasons pays once.
func (a *API) manifestsFromSnapshot(
	r *http.Request, repo, ref, token string, snap repoSnapshot,
) manifestsResponse {
	client := a.githubClient()
	ctx := r.Context()
	commit, tree := snap.Commit, snap.Tree

	out := manifestsResponse{
		Repo:      repo,
		Ref:       ref,
		Commit:    commit.SHA,
		Stats:     statsFromTree(tree),
		Files:     []ManifestFile{},
		Truncated: tree.Truncated,
	}

	for _, entry := range tree.Entries {
		if entry.Type != "blob" || !triggerspec.IsTriggerFile(entry.Path) {
			continue
		}

		if len(out.Files) >= maxTriggerFiles {
			break
		}

		out.Files = append(out.Files, a.readTriggerFile(ctx, client, token, repo, entry))
	}

	return out
}

// readTriggerFile fetches and parses one file, turning every failure into a
// verdict ON THAT FILE rather than an error for the whole request.
//
// One unreadable file must not blank the screen: an operator with three
// triggers and a typo in one needs to see the other two working, which is also
// how they know the typo is the problem.
func (a *API) readTriggerFile(ctx context.Context, client *github.Client, token, repo string, entry github.TreeEntry) ManifestFile {
	file := ManifestFile{Path: entry.Path}

	raw, err := client.GetBlob(ctx, token, repo, entry.SHA)
	if err != nil {
		file.Error = "could not be read: " + err.Error()

		return file
	}

	spec, err := triggerspec.Parse(entry.Path, raw)
	if err != nil {
		var invalid triggerspec.ErrInvalid

		if errors.As(err, &invalid) {
			file.Error = invalid.Reason
		} else {
			file.Error = err.Error()
		}

		return file
	}

	file.Spec = &spec

	return file
}

// resolveTrigger reads `?trigger=<id>` and answers the request itself on every
// failure, so callers are one `if !ok { return }`.
func (a *API) resolveTrigger(w http.ResponseWriter, r *http.Request) (*Trigger, bool) {
	id := strings.TrimSpace(r.URL.Query().Get("trigger"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("trigger is required"))

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

// githubClient returns the injected client or a default one. A field so tests
// can point it at a local server, never so a token can be stashed on it.
func (a *API) githubClient() *github.Client {
	if a.GitHub != nil {
		return a.GitHub
	}

	return &github.Client{}
}

// writeGitHubErr maps an upstream failure to a status the operator can act on.
//
// Each of these has a different fix, and collapsing them into 502 would leave
// somebody restarting a controller because their token expired.
func writeGitHubErr(w http.ResponseWriter, err error) {
	var apiErr *github.APIError

	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Unauthorized():
			writeErr(w, http.StatusUnauthorized,
				fmt.Errorf("GitHub refused the token on %s — it may have expired, "+
					"or it does not reach this repository", apiErr.Path))

			return
		case apiErr.NotFound():
			// THE PATH IS NAMED, and it is ours rather than GitHub's — the
			// endpoint we called, with no response body echoed.
			//
			// Without it, "GitHub has no such repository or ref" is the same
			// sentence whether the commit lookup, the tree read or the blob
			// fetch was the one that failed, and the three have different
			// causes. Diagnosing one of these cost a whole session of guessing
			// from the outside, because the box knew exactly which call it had
			// made and did not say.
			writeErr(w, http.StatusNotFound,
				fmt.Errorf("GitHub has no such repository or ref at %s — or the token "+
					"cannot see it, which it reports the same way", apiErr.Path))

			return
		}
	}

	writeErr(w, http.StatusBadGateway, fmt.Errorf("could not reach GitHub: %w", err))
}

// Check is one preflight question and its answer.
//
// FOUR SEPARATE ANSWERS, not one boolean, and that is the design. Each failure
// has a different fix — a firewall, an expired token, a scope nobody
// authorised, a file nobody wrote — and "preflight failed" tells an operator
// none of them. This runs the moment somebody connects a repository, which is
// the one time they will fix the configuration willingly; discovering the same
// facts when Friday's deploy did not happen is the same information delivered
// at the worst possible moment.
type Check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`

	// Detail is what to DO about it, not a restatement of the name. Empty when
	// the check passed.
	Detail string `json:"detail,omitempty"`
}

type preflightResponse struct {
	Trigger string  `json:"trigger"`
	Repo    string  `json:"repo"`
	Branch  string  `json:"branch"`
	OK      bool    `json:"ok"`
	Checks  []Check `json:"checks"`
}

// handleDeployPreflight proves a deploy would work, without doing one.
//
//	GET /deploy/preflight?trigger=<id>
//
// Always 200 when the trigger resolves: a failed CHECK is the answer, not an
// error. Returning 502 for an unreachable GitHub would make the caller handle
// transport failure and check failure differently for the same question.
func (a *API) handleDeployPreflight(w http.ResponseWriter, r *http.Request) {
	trigger, ok := a.resolveTrigger(w, r)
	if !ok {
		return
	}

	out := preflightResponse{
		Trigger: trigger.ID,
		Repo:    trigger.Repo,
		Branch:  trigger.Branch,
	}

	out.Checks = append(out.Checks, a.checkTriggerEnabled(trigger))
	out.Checks = append(out.Checks, a.checkContainerRuntime(r))
	out.Checks = append(out.Checks, a.checkGitHubAndManifests(r, trigger)...)

	out.OK = true

	for _, c := range out.Checks {
		if !c.OK {
			out.OK = false

			break
		}
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: out})
}

func (a *API) checkTriggerEnabled(trigger *Trigger) Check {
	if !trigger.Enabled {
		return Check{
			Name:   "trigger_enabled",
			Detail: "this trigger is paused — `vd deploy trigger enable " + trigger.ID + "` on the box",
		}
	}

	return Check{Name: "trigger_enabled", OK: true}
}

// checkContainerRuntime asks whether this box can run anything at all.
//
// First among the failures worth telling apart: a controller that cannot reach
// its container runtime will accept a deploy and fail at the last step, after
// the download and the build.
func (a *API) checkContainerRuntime(r *http.Request) Check {
	if a.Pods == nil {
		return Check{Name: "container_runtime", OK: true, Detail: "not wired on this controller"}
	}

	if _, err := a.Pods.ListPods(); err != nil {
		return Check{
			Name:   "container_runtime",
			Detail: "the container runtime did not answer: " + err.Error(),
		}
	}

	return Check{Name: "container_runtime", OK: true}
}

// checkGitHubAndManifests answers the two remaining questions together,
// because the second cannot be asked without the first succeeding.
func (a *API) checkGitHubAndManifests(r *http.Request, trigger *Trigger) []Check {
	token := strings.TrimSpace(r.Header.Get(GitHubTokenHeader))

	if token == "" {
		return []Check{
			{
				Name:   "github_reachable",
				Detail: "no token was supplied — the control plane mints one per request; see " + GitHubTokenHeader,
			},
			{Name: "manifests_found", Detail: "not checked: GitHub was not reached"},
		}
	}

	result, err := a.readManifests(r, trigger.Repo, trigger.Branch, token)
	if err != nil {
		return []Check{
			{Name: "github_reachable", Detail: describeGitHubFailure(err, trigger)},
			{Name: "manifests_found", Detail: "not checked: GitHub was not reached"},
		}
	}

	checks := []Check{{Name: "github_reachable", OK: true}}

	usable := 0

	for _, f := range result.Files {
		if f.Spec != nil {
			usable++
		}
	}

	switch {
	case len(result.Files) == 0 && result.Truncated:
		checks = append(checks, Check{
			Name: "manifests_found",
			Detail: "no " + triggerspec.Dir + "/**/*.yml found, and the repository was too large to list in full — " +
				"the answer may be incomplete rather than empty",
		})
	case len(result.Files) == 0:
		checks = append(checks, Check{
			Name:   "manifests_found",
			Detail: "no " + triggerspec.Dir + "/**/*.yml on " + trigger.Branch + " — add one to declare when to deploy",
		})
	case usable == 0:
		checks = append(checks, Check{
			Name:   "manifests_found",
			Detail: fmt.Sprintf("found %d trigger file(s), none of them valid — see the per-file errors", len(result.Files)),
		})
	default:
		checks = append(checks, Check{Name: "manifests_found", OK: true})
	}

	return checks
}

// describeGitHubFailure names the fix, not the status code.
func describeGitHubFailure(err error, trigger *Trigger) string {
	var apiErr *github.APIError

	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Unauthorized():
			return "GitHub refused the token — it may have expired, or the app is not installed on " + trigger.Repo
		case apiErr.NotFound():
			return "GitHub has no " + trigger.Repo + " at " + trigger.Branch + ", or the token cannot see it"
		}
	}

	return "could not reach GitHub from this box — check outbound network and any egress proxy: " + err.Error()
}

// maxLanguages caps the extension breakdown.
//
// A long tail of one-file extensions is noise on a screen: the six that
// dominate say what the repository is, and the fortieth says somebody once
// committed a `.bak`.
const maxLanguages = 6

// statsFromTree measures the working tree from a listing already fetched.
//
// A TRUNCATED listing produces an UNDERCOUNT, and that is why the response
// carries `truncated` beside these numbers: reporting a partial sum as a total
// would be a number that looks precise and is wrong, which is worse than no
// number at all. The caller has to say so.
func statsFromTree(tree github.Tree) RepoStats {
	stats := RepoStats{}
	byExt := map[string]*LanguageBytes{}

	for _, entry := range tree.Entries {
		if entry.Type != "blob" {
			continue
		}

		stats.Files++
		stats.Bytes += entry.Size

		ext := strings.ToLower(path.Ext(entry.Path))
		if ext == "" {
			// Dockerfile, Makefile, LICENSE. Grouped rather than dropped:
			// they are real files and their bytes are part of the total, so
			// omitting them would make the breakdown fail to add up.
			ext = "(no extension)"
		}

		lang, ok := byExt[ext]
		if !ok {
			lang = &LanguageBytes{Ext: ext}
			byExt[ext] = lang
		}

		lang.Bytes += entry.Size
		lang.Files++
	}

	for _, lang := range byExt {
		stats.Languages = append(stats.Languages, *lang)
	}

	sort.Slice(stats.Languages, func(i, j int) bool {
		if stats.Languages[i].Bytes != stats.Languages[j].Bytes {
			return stats.Languages[i].Bytes > stats.Languages[j].Bytes
		}

		// Ties broken by name so two runs over the same repository produce the
		// same order — map iteration would not.
		return stats.Languages[i].Ext < stats.Languages[j].Ext
	})

	if len(stats.Languages) > maxLanguages {
		stats.Languages = stats.Languages[:maxLanguages]
	}

	return stats
}
