package controller

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "go.voodu.clowk.in/internal/github"
)

const testSHA = "1234567890abcdef1234567890abcdef12345678"

// runGitHub answers the deploy path's calls from an in-memory repository.
// `ancestor` drives the invariant III check.
func runGitHub(t *testing.T, files map[string]string, ancestor bool) *gh.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/compare/"):
			status := "diverged"
			if ancestor {
				status = "behind"
			}

			_, _ = w.Write([]byte(`{"status":"` + status + `"}`))

		case strings.Contains(r.URL.Path, "/commits/"):
			_, _ = w.Write([]byte(`{"sha":"` + testSHA + `","commit":{"tree":{"sha":"tree1"}}}`))

		case strings.Contains(r.URL.Path, "/git/trees/"):
			entries := []gh.TreeEntry{}
			for p := range files {
				entries = append(entries, gh.TreeEntry{Path: p, Type: "blob", SHA: "blob:" + p})
			}

			_ = json.NewEncoder(w).Encode(gh.Tree{SHA: "tree1", Entries: entries})

		case strings.Contains(r.URL.Path, "/git/blobs/"):
			p := r.URL.Path[strings.Index(r.URL.Path, "blob:")+len("blob:"):]
			content, ok := files[p]

			if !ok {
				w.WriteHeader(http.StatusNotFound)

				return
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"content":  base64.StdEncoding.EncodeToString([]byte(content)),
				"encoding": "base64",
				"size":     len(content),
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(srv.Close)

	return &gh.Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

// stubParser stands in for internal/manifest, which cannot be imported here.
// It returns whatever manifests the test declared for the file's content.
func stubParser(byContent map[string][]Manifest) ManifestParser {
	return func(r io.Reader, _ string, _ map[string]string) ([]Manifest, error) {
		raw, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}

		return byContent[string(raw)], nil
	}
}

func newRunAPI(t *testing.T, files map[string]string, ancestor bool, parsed map[string][]Manifest) (*API, *httptest.Server) {
	t.Helper()

	api, _ := newTestAPI(t)
	api.GitHub = runGitHub(t, files, ancestor)
	api.ParseManifests = stubParser(parsed)

	trigger := Trigger{
		ID: "trg1", Repo: "acme/web", Branch: "main",
		AllowScopes: []string{"runa"}, Enabled: true,
	}

	if err := api.Store.PutTrigger(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	return api, ts
}

func postRun(t *testing.T, ts *httptest.Server, body string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/deploy/triggers/trg1/run", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(GitHubTokenHeader, "ghs_x")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	return resp, string(raw)
}

const runSpec = `
name: PWA
on:
  push:
    branches: [main]
apply:
  file: voodu.hcl
`

func repoFiles() map[string]string {
	return map[string]string{
		".voodu/pwa.yml": runSpec,
		"voodu.hcl":      "MANIFEST-BODY",
	}
}

func TestDeployRunAppliesTheRepositoryManifest(t *testing.T) {
	api, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1"}`)}},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	// The manifest reached the store through the ordinary apply path.
	stored, err := api.Store.Get(t.Context(), KindDeployment, "runa", "web")
	if err != nil || stored == nil {
		t.Fatalf("the manifest was not applied: %v", err)
	}

	// The trigger records that it fired — usually the answer to "why is
	// nothing deploying".
	trigger, _ := api.Store.GetTrigger(t.Context(), "trg1")

	if trigger.LastFiredAt == nil {
		t.Error("last_fired_at was not stamped")
	}
}

// INVARIANT I. If the endpoint accepted manifest content, whoever holds the
// token could apply an arbitrary container with a host mount — root, by
// another name. It is this rule, and not the token's scope, that prevents it.
func TestDeployRunRefusesManifestContentInTheBody(t *testing.T) {
	_, ts := newRunAPI(t, repoFiles(), true, nil)

	bodies := []string{
		`{"sha":"` + testSHA + `","manifests":[{"kind":"deployment","scope":"runa","name":"evil"}]}`,
		`{"sha":"` + testSHA + `","spec":{"image":"attacker/x"}}`,
		`{"sha":"` + testSHA + `","apply":{"file":"/etc/passwd"}}`,
	}

	for _, body := range bodies {
		resp, out := postRun(t, ts, body)

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s was accepted with %d: %s", body, resp.StatusCode, out)
		}
	}
}

// INVARIANT III. A fork's pull request lives in the same object store and its
// head is reachable by SHA. Without this check the token applies code nobody
// reviewed and that was never on the pinned branch.
func TestDeployRunRefusesACommitThatIsNotAnAncestor(t *testing.T) {
	api, ts := newRunAPI(t, repoFiles(), false, map[string][]Manifest{
		"MANIFEST-BODY": {{Kind: KindDeployment, Scope: "runa", Name: "web"}},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", resp.StatusCode, body)
	}

	if !strings.Contains(body, "ancestor") {
		t.Errorf("the refusal should name the reason: %s", body)
	}

	// Nothing was applied — the check runs BEFORE a byte of the repository is
	// read, so an unreviewed commit's files are never fetched or parsed.
	if stored, _ := api.Store.Get(t.Context(), KindDeployment, "runa", "web"); stored != nil {
		t.Fatal("an unreviewed commit was applied")
	}
}

// INVARIANT II, on what will ACTUALLY be applied. The scopes come out of the
// parsed manifests, so a file claiming one scope while declaring another
// cannot pass.
func TestDeployRunRefusesAScopeTheTriggerDoesNotAllow(t *testing.T) {
	api, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {{Kind: KindDeployment, Scope: "producao", Name: "web"}},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", resp.StatusCode, body)
	}

	if !strings.Contains(body, "producao") {
		t.Errorf("the refusal should name the scope: %s", body)
	}

	if stored, _ := api.Store.Get(t.Context(), KindDeployment, "producao", "web"); stored != nil {
		t.Fatal("an unauthorised scope was applied")
	}
}

// An operator widening a trigger should do it in one pass, not one deploy per
// refused scope.
func TestDeployRunNamesEveryRefusedScopeAtOnce(t *testing.T) {
	_, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {
			{Kind: KindDeployment, Scope: "runa", Name: "ok"},
			{Kind: KindDeployment, Scope: "producao", Name: "a"},
			{Kind: KindDeployment, Scope: "staging", Name: "b"},
		},
	})

	_, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	for _, scope := range []string{"producao", "staging"} {
		if !strings.Contains(body, scope) {
			t.Errorf("refusal did not mention %q: %s", scope, body)
		}
	}
}

// A batch that spans an allowed and a refused scope is refused ENTIRELY. Half
// an apply is a state nobody described.
func TestDeployRunAppliesNothingWhenOneScopeIsRefused(t *testing.T) {
	api, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {
			{Kind: KindDeployment, Scope: "runa", Name: "allowed"},
			{Kind: KindDeployment, Scope: "producao", Name: "refused"},
		},
	})

	postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if stored, _ := api.Store.Get(t.Context(), KindDeployment, "runa", "allowed"); stored != nil {
		t.Fatal("half the batch was applied")
	}
}

// An abbreviated SHA is ambiguous by construction, and this value decides what
// runs in production.
func TestDeployRunInsistsOnAFullSHA(t *testing.T) {
	_, ts := newRunAPI(t, repoFiles(), true, nil)

	for _, sha := range []string{"1234567", "", "not-hex-not-hex-not-hex-not-hex-not-hexx", strings.ToUpper(testSHA)} {
		resp, _ := postRun(t, ts, `{"sha":"`+sha+`"}`)

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("sha %q was accepted with %d", sha, resp.StatusCode)
		}
	}
}

func TestDeployRunRefusesAPausedTrigger(t *testing.T) {
	api, ts := newRunAPI(t, repoFiles(), true, nil)

	trigger, _ := api.Store.GetTrigger(t.Context(), "trg1")
	trigger.Enabled = false

	if err := api.Store.PutTrigger(t.Context(), *trigger); err != nil {
		t.Fatal(err)
	}

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", resp.StatusCode, body)
	}
}

// The repository declares WHEN it deploys. A push to a ref no trigger file
// matches is not an error, but it is not a deploy either.
func TestDeployRunRefusesWhenNoTriggerFileMatchesTheRef(t *testing.T) {
	_, ts := newRunAPI(t, repoFiles(), true, nil)

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`","ref":"refs/heads/feature"}`)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", resp.StatusCode, body)
	}
}

// A trigger file naming a manifest that is not in the repository is a
// configuration error in the repository, and the message has to say which file
// named what.
func TestDeployRunReportsAMissingManifestFile(t *testing.T) {
	files := map[string]string{".voodu/pwa.yml": runSpec}

	_, ts := newRunAPI(t, files, true, nil)

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", resp.StatusCode, body)
	}

	if !strings.Contains(body, "voodu.hcl") {
		t.Errorf("the message should name the missing file: %s", body)
	}
}

// A controller without the parser wired cannot deploy. That is a
// misconfiguration of the box, not a bad request from the caller.
func TestDeployRunWithoutAParserIsUnavailable(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = runGitHub(t, repoFiles(), true)

	if err := api.Store.PutTrigger(t.Context(), Trigger{
		ID: "trg1", Repo: "acme/web", Branch: "main", AllowScopes: []string{"runa"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, _ := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", resp.StatusCode)
	}
}

// A box with no builder wired cannot deploy build mode — a misconfiguration of
// the box, not a bad request. It says so rather than applying a workload whose
// image was never built, which would fail far from here as a missing image.
func TestDeployRunRefusesBuildModeWithoutABuilder(t *testing.T) {
	api, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {{
			Kind: KindDeployment, Scope: "runa", Name: "web",
			Spec: json.RawMessage(`{"build":{"lang":"go"}}`),
		}},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", resp.StatusCode, body)
	}

	if !strings.Contains(body, "no builder wired") {
		t.Errorf("the refusal should name the reason: %s", body)
	}

	if stored, _ := api.Store.Get(t.Context(), KindDeployment, "runa", "web"); stored != nil {
		t.Fatal("a workload with no buildable image was applied")
	}
}

// A workload that names an image is registry mode even if it also carries a
// build block — the image is what runs.
func TestDeployRunAllowsAnImageAlongsideABuildBlock(t *testing.T) {
	api, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {{
			Kind: KindDeployment, Scope: "runa", Name: "web",
			Spec: json.RawMessage(`{"image":"acme/web:1.2.3","build":{"lang":"go"}}`),
		}},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	if stored, _ := api.Store.Get(t.Context(), KindDeployment, "runa", "web"); stored == nil {
		t.Fatal("a registry-mode workload was not applied")
	}
}

// Counting the downloads is the only honest way to assert the optimisation:
// the buildID dedup would make a "did it rebuild" assertion pass either way,
// because it also skips the build — after paying for the transfer.
func TestSecondDeployOfTheSameTreeDownloadsNothing(t *testing.T) {
	var tarballs int

	api, _ := newTestAPI(t)
	api.ParseManifests = stubParser(map[string][]Manifest{
		"MANIFEST-BODY": {{
			Kind: KindDeployment, Scope: "runa", Name: "web",
			Spec: json.RawMessage(`{"build":{"path":"apps/pwa"}}`),
		}},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/compare/"):
			_, _ = w.Write([]byte(`{"status":"behind"}`))

		case strings.Contains(r.URL.Path, "/tarball/"):
			tarballs++
			_, _ = w.Write(repoArchive(t, map[string]string{"apps/pwa/main.go": "package main"}))

		case strings.Contains(r.URL.Path, "/commits/"):
			_, _ = w.Write([]byte(`{"sha":"` + testSHA + `","commit":{"tree":{"sha":"root1"}}}`))

		case strings.Contains(r.URL.Path, "/git/trees/"):
			_ = json.NewEncoder(w).Encode(gh.Tree{SHA: "root1", Entries: []gh.TreeEntry{
				{Path: ".voodu/pwa.yml", Type: "blob", SHA: "blob:.voodu/pwa.yml"},
				{Path: "voodu.hcl", Type: "blob", SHA: "blob:voodu.hcl"},
				{Path: "apps", Type: "tree", SHA: "tapps"},
				{Path: "apps/pwa", Type: "tree", SHA: "tpwa"},
			}})

		case strings.Contains(r.URL.Path, "/git/blobs/"):
			body := runSpec
			if strings.HasSuffix(r.URL.Path, "voodu.hcl") {
				body = "MANIFEST-BODY"
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"content":  base64.StdEncoding.EncodeToString([]byte(body)),
				"encoding": "base64",
				"size":     len(body),
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	defer srv.Close()

	api.GitHub = &gh.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	api.BuildFromSource = func(string, io.Reader, json.RawMessage, bool, io.Writer) error { return nil }

	if err := api.Store.PutTrigger(t.Context(), Trigger{
		ID: "trg1", Repo: "acme/web", Branch: "main", AllowScopes: []string{"runa"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	if resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("first deploy: %d %s", resp.StatusCode, body)
	}

	if tarballs != 1 {
		t.Fatalf("first deploy downloaded %d times, want 1", tarballs)
	}

	// Same tree, second deploy: the subtree SHA is unchanged, so there is
	// nothing to build and nothing to fetch.
	if resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("second deploy: %d %s", resp.StatusCode, body)
	}

	if tarballs != 1 {
		t.Fatalf("the second deploy downloaded the archive again (%d total)", tarballs)
	}

	// The manifest is still applied — a deploy that changes only configuration
	// is still a deploy.
	if stored, _ := api.Store.Get(t.Context(), KindDeployment, "runa", "web"); stored == nil {
		t.Fatal("skipping the build also skipped the apply")
	}
}

// WHAT THE DEPLOY TOUCHED, not only that it worked.
//
// `applied` names the trigger FILES that fired — labels a human chose. The
// console needs the (kind, scope, name) triples to link a deployment to the
// pods it created, and a screen that can say "succeeded" but not "what
// changed" leaves the reader with the question they came with.
func TestDeployRunReportsTheResourcesItApplied(t *testing.T) {
	_, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {
			{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1"}`)},
			{Kind: KindDeployment, Scope: "runa", Name: "worker", Spec: json.RawMessage(`{"image":"x:1"}`)},
		},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	var env struct {
		Data deployRunResponse `json:"data"`
	}

	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}

	if len(env.Data.Resources) != 2 {
		t.Fatalf("resources = %+v", env.Data.Resources)
	}

	want := map[string]bool{"runa/web": true, "runa/worker": true}

	for _, res := range env.Data.Resources {
		key := res.Scope + "/" + res.Name

		if !want[key] {
			t.Errorf("unexpected resource %+v", res)
		}

		delete(want, key)

		if res.Kind == "" {
			t.Errorf("resource %+v has no kind — the console groups by it", res)
		}
	}

	if len(want) > 0 {
		t.Errorf("missing resources: %v", want)
	}
}

// A manifest file may declare the same resource twice through composition.
// Listing it twice would tell the reader two things were deployed.
func TestDeployRunDeduplicatesTheResourcesItReports(t *testing.T) {
	_, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {
			{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1"}`)},
			{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1"}`)},
		},
	})

	_, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	var env struct {
		Data deployRunResponse `json:"data"`
	}

	_ = json.Unmarshal([]byte(body), &env)

	if len(env.Data.Resources) != 1 {
		t.Errorf("resources = %+v, want one", env.Data.Resources)
	}
}

const manualSpec = runSpec + "deploy: manual\n"

// `deploy: manual` is the developer saying "I press the button". A push that
// matches such a file is acknowledged — the console needs to know it arrived —
// but nothing is applied until somebody dispatches it.
func TestDeployRunHoldsManualFilesOnPush(t *testing.T) {
	files := map[string]string{".voodu/pwa.yml": manualSpec, "voodu.hcl": "MANIFEST-BODY"}

	api, ts := newRunAPI(t, files, true, map[string][]Manifest{
		"MANIFEST-BODY": {{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1"}`)}},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`","ref":"refs/heads/main"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	if !strings.Contains(body, `"held":["PWA"]`) {
		t.Errorf("the held file should be named: %s", body)
	}

	if strings.Contains(body, `"applied":["PWA"]`) {
		t.Errorf("a push must not apply a manual file: %s", body)
	}

	if stored, _ := api.Store.Get(t.Context(), KindDeployment, "runa", "web"); stored != nil {
		t.Error("the manifest reached the store on a push it was meant to wait out")
	}
}

// The dispatch is the button. Same commit, same file, and now it applies.
func TestDeployRunDispatchAppliesManualFiles(t *testing.T) {
	files := map[string]string{".voodu/pwa.yml": manualSpec, "voodu.hcl": "MANIFEST-BODY"}

	api, ts := newRunAPI(t, files, true, map[string][]Manifest{
		"MANIFEST-BODY": {{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1"}`)}},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`","mode":"dispatch"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	if !strings.Contains(body, `"applied":["PWA"]`) || strings.Contains(body, `"held"`) {
		t.Errorf("a dispatch applies the manual file and holds nothing: %s", body)
	}

	if stored, _ := api.Store.Get(t.Context(), KindDeployment, "runa", "web"); stored == nil {
		t.Error("the dispatch did not apply the manifest")
	}
}

func TestDeployRunRefusesAnUnknownMode(t *testing.T) {
	_, ts := newRunAPI(t, repoFiles(), true, nil)

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`","mode":"yolo"}`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", resp.StatusCode, body)
	}
}

// A deployment with a release block is one the reconciler refuses to roll on
// its own: it waits for the release phase, which `vd apply` fires after its
// apply. The deploy plane has to do the same, or a push builds an image the
// old replicas never pick up.
func TestDeployRunRunsTheReleasePhaseForDeploymentsWithAReleaseBlock(t *testing.T) {
	api, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {
			{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1","release":{"command":["bin/rails","db:migrate"]}}`)},
			{Kind: KindDeployment, Scope: "runa", Name: "worker", Spec: json.RawMessage(`{"image":"x:1"}`)},
		},
	})

	restarter := &fakeRestarter{}
	api.Deployments = restarter

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	if restarter.gotVerb != "release" || restarter.gotScope != "runa" || restarter.gotName != "web" {
		t.Fatalf("release phase did not run for runa/web: verb=%q scope=%q name=%q", restarter.gotVerb, restarter.gotScope, restarter.gotName)
	}

	if !strings.Contains(body, `"released":["runa/web"]`) {
		t.Fatalf("response should name the released deployment, and only it: %s", body)
	}
}

func TestDeployRunFailsWhenTheReleasePhaseFails(t *testing.T) {
	api, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {
			{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1","release":{"command":["bin/rails","db:migrate"]}}`)},
		},
	})

	api.Deployments = &fakeRestarter{err: errors.New("exit 1")}

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", resp.StatusCode, body)
	}

	if !strings.Contains(body, "release of runa/web failed") {
		t.Fatalf("the reason should name the deployment: %s", body)
	}
}

func TestDeployRunSkipsTheReleasePhaseWithoutAReleaseBlock(t *testing.T) {
	api, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1"}`)}},
	})

	restarter := &fakeRestarter{}
	api.Deployments = restarter

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	if restarter.gotVerb != "" {
		t.Fatalf("no release block, yet the release phase ran (%s)", restarter.gotVerb)
	}

	if strings.Contains(body, `"released"`) {
		t.Fatalf("released should be omitted when nothing was released: %s", body)
	}
}

// newBuildRunAPI is the build-mode harness: a repository whose only
// workload builds from apps/pwa, served from an in-memory GitHub, with the
// builder under test control. The console reads the transcript out of the
// response, so what the builder prints is what these tests assert on.
func newBuildRunAPI(t *testing.T, build SourceBuilder) (*API, *httptest.Server) {
	t.Helper()

	api, _ := newTestAPI(t)
	api.ParseManifests = stubParser(map[string][]Manifest{
		"MANIFEST-BODY": {{
			Kind: KindDeployment, Scope: "runa", Name: "web",
			Spec: json.RawMessage(`{"build":{"path":"apps/pwa"}}`),
		}},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/compare/"):
			_, _ = w.Write([]byte(`{"status":"behind"}`))

		case strings.Contains(r.URL.Path, "/tarball/"):
			_, _ = w.Write(repoArchive(t, map[string]string{"apps/pwa/main.go": "package main"}))

		case strings.Contains(r.URL.Path, "/commits/"):
			_, _ = w.Write([]byte(`{"sha":"` + testSHA + `","commit":{"tree":{"sha":"root1"}}}`))

		case strings.Contains(r.URL.Path, "/git/trees/"):
			_ = json.NewEncoder(w).Encode(gh.Tree{SHA: "root1", Entries: []gh.TreeEntry{
				{Path: ".voodu/pwa.yml", Type: "blob", SHA: "blob:.voodu/pwa.yml"},
				{Path: "voodu.hcl", Type: "blob", SHA: "blob:voodu.hcl"},
				{Path: "apps", Type: "tree", SHA: "tapps"},
				{Path: "apps/pwa", Type: "tree", SHA: "tpwa"},
			}})

		case strings.Contains(r.URL.Path, "/git/blobs/"):
			body := runSpec
			if strings.HasSuffix(r.URL.Path, "voodu.hcl") {
				body = "MANIFEST-BODY"
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"content":  base64.StdEncoding.EncodeToString([]byte(body)),
				"encoding": "base64",
				"size":     len(body),
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	api.GitHub = &gh.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	api.BuildFromSource = build

	if err := api.Store.PutTrigger(t.Context(), Trigger{
		ID: "trg1", Repo: "acme/web", Branch: "main", AllowScopes: []string{"runa"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	return api, ts
}

// The transcript is the only place a console can read what happened on the
// box: the journal is not reachable from a browser.
func TestDeployRunReturnsTheBuildAndReleaseTranscript(t *testing.T) {
	api, ts := newBuildRunAPI(t, func(_ string, _ io.Reader, _ json.RawMessage, _ bool, output io.Writer) error {
		_, _ = io.WriteString(output, "-----> building release\nstep 1/3 done\n")

		return nil
	})

	api.ParseManifests = stubParser(map[string][]Manifest{
		"MANIFEST-BODY": {{
			Kind: KindDeployment, Scope: "runa", Name: "web",
			Spec: json.RawMessage(`{"build":{"path":"apps/pwa"},"release":{"command":["bin/rails","db:migrate"]}}`),
		}},
	})
	api.Deployments = &fakeRestarter{output: "== 20260916 CreateThings: migrating\n"}

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	var env struct {
		Data deployRunResponse `json:"data"`
	}

	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}

	want := "-----> build runa-web\n-----> building release\nstep 1/3 done\n-----> release runa/web\n== 20260916 CreateThings: migrating\n"

	if env.Data.Log != want {
		t.Fatalf("log:\n got %q\nwant %q", env.Data.Log, want)
	}
}

// A failed build answers 422 as before, and the reason the operator needs is
// in the transcript beside the one-line error, not only in the journal.
func TestDeployRunBuildFailureCarriesTheTranscript(t *testing.T) {
	_, ts := newBuildRunAPI(t, func(_ string, _ io.Reader, _ json.RawMessage, _ bool, output io.Writer) error {
		_, _ = io.WriteString(output, "-----> building release\nERROR: bundle install failed\n")

		return errors.New("exit 1")
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", resp.StatusCode, body)
	}

	var env struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Log string `json:"log"`
		} `json:"data"`
	}

	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}

	if env.Status != "error" || !strings.Contains(env.Error, "exit 1") {
		t.Fatalf("the error line must survive: %s", body)
	}

	if !strings.Contains(env.Data.Log, "-----> build runa-web\n") || !strings.Contains(env.Data.Log, "ERROR: bundle install failed") {
		t.Fatalf("the transcript must be on the error response: %s", body)
	}
}

// Same for a failed release: the migration's own output is what explains
// the failure, and the build that came before it is context.
func TestDeployRunReleaseFailureCarriesTheTranscript(t *testing.T) {
	api, ts := newRunAPI(t, repoFiles(), true, map[string][]Manifest{
		"MANIFEST-BODY": {
			{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1","release":{"command":["bin/rails","db:migrate"]}}`)},
		},
	})

	api.Deployments = &fakeRestarter{
		output: "== AddIndex: migrating\nPG::DuplicateTable: relation exists\n",
		err:    errors.New("exit 1"),
	}

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", resp.StatusCode, body)
	}

	var env struct {
		Error string `json:"error"`
		Data  struct {
			Log string `json:"log"`
		} `json:"data"`
	}

	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(env.Error, "release of runa/web failed") {
		t.Fatalf("the error line must name the deployment: %s", body)
	}

	if !strings.Contains(env.Data.Log, "-----> release runa/web\n== AddIndex: migrating\nPG::DuplicateTable") {
		t.Fatalf("the transcript must be on the error response: %s", body)
	}
}

// The reason a deploy failed is on its last lines; a cap that kept the head
// would keep the part nobody needs.
func TestDeployLogKeepsTheTailWhenTruncated(t *testing.T) {
	var l deployLog

	l.section("build", "runa-web")

	line := strings.Repeat("x", 99) + "\n"

	for i := 0; i < (deployLogCap/len(line))*2; i++ {
		_, _ = l.Write([]byte(line))
	}

	_, _ = l.Write([]byte("ERROR: the reason\n"))

	got := l.String()

	if !strings.HasPrefix(got, "[log truncated: showing the last 256 KiB]\n") {
		t.Fatalf("a truncated log must say so on its first line: %q", got[:80])
	}

	if !strings.HasSuffix(got, "ERROR: the reason\n") {
		t.Fatalf("the tail must survive: %q", got[len(got)-40:])
	}

	if strings.Contains(got, "-----> build") {
		t.Fatal("the head should have been cut")
	}

	if len(got) > deployLogCap+80 {
		t.Fatalf("log is %d bytes, cap is %d", len(got), deployLogCap)
	}

	// Below the cap, nothing is touched.
	var small deployLog

	small.section("release", "runa/web")
	_, _ = small.Write([]byte("ok\n"))

	if small.String() != "-----> release runa/web\nok\n" {
		t.Fatalf("small log altered: %q", small.String())
	}
}

const pathsSpec = `
name: PWA
on:
  push:
    branches: [main]
    paths: ["apps/pwa/**"]
apply:
  file: voodu.hcl
`

// `on.push.paths` decides from what the push touched. The webhook carries the
// list; a push that only changed the api must not build the pwa, and the pwa
// file has to show up in `skipped` so the console can say why.
func TestDeployRunSkipsAFileWhosePathsThePushDidNotTouch(t *testing.T) {
	files := map[string]string{".voodu/pwa.yml": pathsSpec, "voodu.hcl": "MANIFEST-BODY"}
	api, ts := newRunAPI(t, files, true, map[string][]Manifest{
		"MANIFEST-BODY": {{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1"}`)}},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`","changed":["apps/api/main.go","README.md"]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	if !strings.Contains(body, `"skipped":["PWA"]`) || strings.Contains(body, `"applied":["PWA"]`) {
		t.Fatalf("the file should be skipped, not applied: %s", body)
	}

	if stored, _ := api.Store.Get(t.Context(), KindDeployment, "runa", "web"); stored != nil {
		t.Fatal("nothing should have been applied")
	}
}

func TestDeployRunAppliesAFileWhosePathsThePushTouched(t *testing.T) {
	files := map[string]string{".voodu/pwa.yml": pathsSpec, "voodu.hcl": "MANIFEST-BODY"}
	_, ts := newRunAPI(t, files, true, map[string][]Manifest{
		"MANIFEST-BODY": {{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1"}`)}},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`","changed":["apps/pwa/src/index.ts"]}`)

	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"applied":["PWA"]`) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

// Unknown must fire: a caller that could not list the files (a push bigger
// than the payload carries, an older control plane) sends no `changed`, and
// the paths filter stands aside rather than skipping a deploy on a guess.
func TestDeployRunFiresPathsFilesWhenTheChangeSetIsUnknown(t *testing.T) {
	files := map[string]string{".voodu/pwa.yml": pathsSpec, "voodu.hcl": "MANIFEST-BODY"}
	_, ts := newRunAPI(t, files, true, map[string][]Manifest{
		"MANIFEST-BODY": {{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1"}`)}},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`"}`)

	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"applied":["PWA"]`) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

// A person dispatching a commit means it, whatever the commit touched.
func TestDeployRunDispatchIgnoresPaths(t *testing.T) {
	files := map[string]string{".voodu/pwa.yml": pathsSpec, "voodu.hcl": "MANIFEST-BODY"}
	_, ts := newRunAPI(t, files, true, map[string][]Manifest{
		"MANIFEST-BODY": {{Kind: KindDeployment, Scope: "runa", Name: "web", Spec: json.RawMessage(`{"image":"x:1"}`)}},
	})

	resp, body := postRun(t, ts, `{"sha":"`+testSHA+`","mode":"dispatch","changed":["README.md"]}`)

	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"applied":["PWA"]`) {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}
