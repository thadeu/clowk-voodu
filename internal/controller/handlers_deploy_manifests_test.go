package controller

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "go.voodu.clowk.in/internal/github"
)

// fakeGitHub answers the three calls this path makes, from an in-memory
// repository. Everything else 404s, so a call nobody meant to make is visible.
// fakeGitHubSized is fakeGitHub with real blob sizes in the tree listing, for
// the metrics the screen reads.
func fakeGitHubSized(t *testing.T, files map[string]string, truncated bool) *gh.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/commits/"):
			_, _ = w.Write([]byte(`{"sha":"commit1","commit":{"tree":{"sha":"tree1"}}}`))

		case r.URL.Path == "/repos/acme/web":
			_, _ = w.Write([]byte(`{"full_name":"acme/web","default_branch":"trunk"}`))

		case strings.Contains(r.URL.Path, "/git/trees/"):
			entries := []gh.TreeEntry{{Path: "apps", Type: "tree", SHA: "t1"}}

			for path, content := range files {
				entries = append(entries, gh.TreeEntry{
					Path: path, Type: "blob", SHA: "blob:" + path, Size: int64(len(content)),
				})
			}

			_ = json.NewEncoder(w).Encode(gh.Tree{SHA: "tree1", Entries: entries, Truncated: truncated})

		case strings.Contains(r.URL.Path, "/git/blobs/"):
			path := strings.TrimPrefix(r.URL.Path, "/repos/acme/web/git/blobs/blob:")
			content, ok := files[path]

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

func fakeGitHub(t *testing.T, files map[string]string, truncated bool) *gh.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/commits/"):
			_, _ = w.Write([]byte(`{"sha":"commit1","commit":{"tree":{"sha":"tree1"}}}`))

		case strings.Contains(r.URL.Path, "/git/trees/"):
			entries := []gh.TreeEntry{}
			for path := range files {
				entries = append(entries, gh.TreeEntry{Path: path, Type: "blob", SHA: "blob:" + path})
			}

			_ = json.NewEncoder(w).Encode(gh.Tree{SHA: "tree1", Entries: entries, Truncated: truncated})

		case strings.Contains(r.URL.Path, "/git/blobs/"):
			path := strings.TrimPrefix(r.URL.Path, "/repos/acme/web/git/blobs/blob:")
			content, ok := files[path]

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
			t.Errorf("unexpected GitHub call: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(srv.Close)

	return &gh.Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func seedTrigger(t *testing.T, api *API) Trigger {
	t.Helper()

	trigger := Trigger{
		ID: "trg1", Repo: "acme/web", Branch: "main",
		AllowScopes: []string{"runa"}, Enabled: true,
	}

	if err := api.Store.PutTrigger(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}

	return trigger
}

func getWithToken(t *testing.T, url, token string) (*http.Response, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	if token != "" {
		req.Header.Set(GitHubTokenHeader, token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	var env struct {
		Data map[string]any `json:"data"`
	}

	_ = json.NewDecoder(resp.Body).Decode(&env)

	return resp, env.Data
}

const validSpec = `
name: PWA
on:
  push:
    branches: [main]
apply:
  file: .voodu/pwa.hcl
`

func TestManifestsReturnsParsedSpecs(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = fakeGitHub(t, map[string]string{
		".voodu/pwa.yml": validSpec,
		// The manifest lives beside the trigger and must not be mistaken for
		// one — the extension is what separates them.
		".voodu/pwa.hcl": `deployment "runa" "web" {}`,
		"README.md":      "hi",
	}, false)

	seedTrigger(t, api)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, data := getWithToken(t, ts.URL+"/deploy/manifests?trigger=trg1", "ghs_x")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	files, _ := data["files"].([]any)

	if len(files) != 1 {
		t.Fatalf("files = %v — only the .yml is a trigger", files)
	}

	first, _ := files[0].(map[string]any)

	if first["path"] != ".voodu/pwa.yml" {
		t.Errorf("path = %v", first["path"])
	}

	if first["spec"] == nil {
		t.Fatalf("no spec: %v", first)
	}

	if data["commit"] != "commit1" {
		t.Errorf("commit = %v — the console must see which revision it is reading", data["commit"])
	}
}

// An operator with three triggers and a typo in one needs to see the other two
// working; that is also how they learn the typo is the problem.
func TestOneBrokenFileDoesNotBlankTheRest(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = fakeGitHub(t, map[string]string{
		".voodu/good.yml": validSpec,
		".voodu/bad.yml":  "on:\n  push:\n    branch: [main]\napply:\n  file: x.hcl\n",
	}, false)

	seedTrigger(t, api)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, data := getWithToken(t, ts.URL+"/deploy/manifests?trigger=trg1", "ghs_x")

	files, _ := data["files"].([]any)

	if len(files) != 2 {
		t.Fatalf("both files must come back: %v", files)
	}

	var good, bad map[string]any

	for _, f := range files {
		m, _ := f.(map[string]any)

		if m["path"] == ".voodu/good.yml" {
			good = m
		} else {
			bad = m
		}
	}

	if good["spec"] == nil || good["error"] != nil {
		t.Errorf("the good file should be usable: %v", good)
	}

	if bad["spec"] != nil || bad["error"] == nil {
		t.Errorf("the bad file should carry its reason: %v", bad)
	}
}

// A caller that cannot tell a complete answer from a partial one will present
// the partial one as complete.
func TestTruncationIsReported(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = fakeGitHub(t, map[string]string{}, true)

	seedTrigger(t, api)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, data := getWithToken(t, ts.URL+"/deploy/manifests?trigger=trg1", "ghs_x")

	if data["truncated"] != true {
		t.Fatalf("truncation was swallowed: %v", data)
	}
}

// This box has no GitHub credentials of its own, by design.
func TestManifestsRequiresAToken(t *testing.T) {
	api, _ := newTestAPI(t)
	seedTrigger(t, api)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, _ := getWithToken(t, ts.URL+"/deploy/manifests?trigger=trg1", "")

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

// Four separate answers, not one boolean: each failure has a different fix,
// and "preflight failed" names none of them.
func TestPreflightAnswersEachQuestionSeparately(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = fakeGitHub(t, map[string]string{".voodu/pwa.yml": validSpec}, false)

	seedTrigger(t, api)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, data := getWithToken(t, ts.URL+"/deploy/preflight?trigger=trg1", "ghs_x")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	if data["ok"] != true {
		t.Fatalf("preflight should pass: %v", data)
	}

	checks, _ := data["checks"].([]any)
	names := map[string]bool{}

	for _, c := range checks {
		m, _ := c.(map[string]any)
		names[m["name"].(string)] = true
	}

	for _, want := range []string{"trigger_enabled", "container_runtime", "github_reachable", "manifests_found"} {
		if !names[want] {
			t.Errorf("preflight did not answer %q: %v", want, names)
		}
	}
}

// A failed check is the ANSWER, not an error. Returning 502 for an unreachable
// GitHub would make the caller handle transport failure and check failure
// differently for the same question.
func TestPreflightReportsFailuresAsChecks(t *testing.T) {
	api, _ := newTestAPI(t)
	seedTrigger(t, api)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	// No token: GitHub cannot be reached, and the manifest question cannot
	// even be asked.
	resp, data := getWithToken(t, ts.URL+"/deploy/preflight?trigger=trg1", "")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d — a failed check is still a successful answer", resp.StatusCode)
	}

	if data["ok"] != false {
		t.Fatalf("preflight should fail: %v", data)
	}

	checks, _ := data["checks"].([]any)

	for _, c := range checks {
		m, _ := c.(map[string]any)

		if m["name"] == "manifests_found" && m["ok"] == false {
			// The detail must say it was not checked, not that nothing was
			// found — those are different facts with different fixes.
			if !strings.Contains(m["detail"].(string), "not checked") {
				t.Errorf("unchecked question reported as a negative answer: %v", m)
			}
		}
	}
}

func TestPreflightNamesAPausedTrigger(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = fakeGitHub(t, map[string]string{".voodu/pwa.yml": validSpec}, false)

	trigger := seedTrigger(t, api)
	trigger.Enabled = false

	if err := api.Store.PutTrigger(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, data := getWithToken(t, ts.URL+"/deploy/preflight?trigger=trg1", "ghs_x")

	if data["ok"] != false {
		t.Fatal("a paused trigger must fail preflight")
	}
}

// Every number here comes from the tree listing the trigger files were found
// in, so the screen costs no extra request. A screen that spends an API call
// per visit is a screen that gets throttled the day somebody leaves it open.
func TestManifestsCarryRepoStatsWithoutExtraCalls(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = fakeGitHubSized(t, map[string]string{
		".voodu/pwa.yml": validSpec,
		"main.go":        strings.Repeat("x", 1000),
		"util.go":        strings.Repeat("x", 500),
		"README.md":      strings.Repeat("x", 200),
		"Dockerfile":     strings.Repeat("x", 100),
	}, false)

	seedTrigger(t, api)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, data := getWithToken(t, ts.URL+"/deploy/manifests?trigger=trg1", "ghs_x")

	stats, _ := data["stats"].(map[string]any)

	if stats == nil {
		t.Fatal("no stats in the response")
	}

	if stats["files"].(float64) != 5 {
		t.Errorf("files = %v, want the blob count (the `apps` tree entry is not a file)", stats["files"])
	}

	// Sizes come from the listing, so the total is exact rather than estimated.
	wantBytes := float64(len(validSpec) + 1000 + 500 + 200 + 100)

	if stats["bytes"].(float64) != wantBytes {
		t.Errorf("bytes = %v, want %v", stats["bytes"], wantBytes)
	}

	langs, _ := stats["languages"].([]any)

	if len(langs) == 0 {
		t.Fatal("no language breakdown")
	}

	// Largest first: .go is 1500 bytes across two files, ahead of everything.
	first, _ := langs[0].(map[string]any)

	if first["ext"] != ".go" || first["files"].(float64) != 2 {
		t.Errorf("first language = %v, want .go with 2 files", first)
	}
}

// Dockerfile, Makefile and LICENSE are real files and their bytes are part of
// the total. Dropping them would make the breakdown fail to add up to it.
func TestExtensionlessFilesAreGroupedNotDropped(t *testing.T) {
	tree := gh.Tree{Entries: []gh.TreeEntry{
		{Path: "Dockerfile", Type: "blob", Size: 100},
		{Path: "Makefile", Type: "blob", Size: 50},
		{Path: "main.go", Type: "blob", Size: 10},
	}}

	stats := statsFromTree(tree)

	if stats.Bytes != 160 {
		t.Fatalf("bytes = %d, want 160", stats.Bytes)
	}

	var grouped *LanguageBytes

	for i := range stats.Languages {
		if stats.Languages[i].Ext == "(no extension)" {
			grouped = &stats.Languages[i]
		}
	}

	if grouped == nil || grouped.Files != 2 || grouped.Bytes != 150 {
		t.Fatalf("extensionless files were not grouped: %+v", stats.Languages)
	}
}

// Map iteration would order two runs over the same repository differently, and
// a screen whose rows reshuffle on refresh reads as data changing.
func TestLanguageOrderIsStable(t *testing.T) {
	tree := gh.Tree{Entries: []gh.TreeEntry{
		{Path: "a.go", Type: "blob", Size: 100},
		{Path: "b.rb", Type: "blob", Size: 100},
		{Path: "c.js", Type: "blob", Size: 100},
	}}

	first := statsFromTree(tree).Languages

	for i := 0; i < 20; i++ {
		next := statsFromTree(tree).Languages

		for j := range first {
			if first[j].Ext != next[j].Ext {
				t.Fatalf("order changed between runs: %v vs %v", first, next)
			}
		}
	}
}

// A truncated listing produces an UNDERCOUNT. The response carries `truncated`
// beside the numbers for exactly this reason: a partial sum presented as a
// total is a number that looks precise and is wrong.
func TestStatsFromATruncatedListingAreFlagged(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = fakeGitHubSized(t, map[string]string{"main.go": "x"}, true)

	seedTrigger(t, api)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, data := getWithToken(t, ts.URL+"/deploy/manifests?trigger=trg1", "ghs_x")

	if data["truncated"] != true {
		t.Fatal("stats came back without the flag that says they are partial")
	}
}

// The connect flow's read: a repository the customer just authorised, with no
// trigger yet. Requiring a trigger first would mean granting scopes to find
// out whether the thing is configured at all.
func TestManifestsCanReadARepositoryWithNoTrigger(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = fakeGitHubSized(t, map[string]string{".voodu/pwa.yml": validSpec}, false)

	// Deliberately NO trigger seeded.
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, data := getWithToken(t, ts.URL+"/deploy/manifests?repo=acme/web", "ghs_x")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	files, _ := data["files"].([]any)

	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}

	// No ref given, so it asked GitHub rather than guessing `main` — which is
	// wrong for every repository still on `master`, and fails silently by
	// showing an empty list instead of an error.
	if data["ref"] != "trunk" {
		t.Errorf("ref = %v, want the repository's default branch", data["ref"])
	}
}

func TestManifestsHonoursAnExplicitRef(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = fakeGitHubSized(t, map[string]string{".voodu/pwa.yml": validSpec}, false)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	_, data := getWithToken(t, ts.URL+"/deploy/manifests?repo=acme/web&ref=feature-x", "ghs_x")

	if data["ref"] != "feature-x" {
		t.Errorf("ref = %v", data["ref"])
	}
}

// The repository name is interpolated into GitHub API URLs. A value with an
// extra slash or a `..` addresses a different endpoint than the code reads
// like it addresses.
func TestManifestsRefusesAMalformedRepo(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = fakeGitHubSized(t, map[string]string{}, false)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	for _, bad := range []string{"web", "a/b/c", "../etc", "acme%2Fweb%2F.."} {
		resp, _ := getWithToken(t, ts.URL+"/deploy/manifests?repo="+bad, "ghs_x")

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("repo %q was accepted with %d", bad, resp.StatusCode)
		}
	}
}

// The two carry different repositories and different defaults. Silently
// preferring one would answer a question the caller did not ask.
func TestManifestsRefusesBothTriggerAndRepo(t *testing.T) {
	api, _ := newTestAPI(t)
	api.GitHub = fakeGitHubSized(t, map[string]string{}, false)

	seedTrigger(t, api)

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, _ := getWithToken(t, ts.URL+"/deploy/manifests?trigger=trg1&repo=acme/web", "ghs_x")

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}

	none, _ := getWithToken(t, ts.URL+"/deploy/manifests", "ghs_x")

	if none.StatusCode != http.StatusBadRequest {
		t.Fatalf("neither should be refused too, got %d", none.StatusCode)
	}
}

// "GitHub has no such repository or ref" is the same sentence whether the
// commit lookup, the tree read or the blob fetch failed — and the three have
// different causes. The box knows which call it made; saying so is the
// difference between a one-line diagnosis and guessing from the outside.
//
// The PATH is ours, not GitHub's response body: the body can echo request
// details including the credential, which is why it is discarded unread.
func TestGitHubFailureNamesTheCallThatFailed(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusNotFound, "/repos/acme/api/commits/main"},
		{http.StatusUnauthorized, "/repos/acme/api/commits/main"},
	} {
		rr := httptest.NewRecorder()
		writeGitHubErr(rr, &gh.APIError{Status: tc.status, Path: tc.want})

		if !strings.Contains(rr.Body.String(), tc.want) {
			t.Errorf("status %d: %q does not name the path %q", tc.status, rr.Body.String(), tc.want)
		}
	}
}
