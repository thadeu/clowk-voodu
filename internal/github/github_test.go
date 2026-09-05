package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestGetCommitReadsTheTreeSHA(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/web/commits/main" {
			t.Errorf("path = %s", r.URL.Path)
		}

		if got := r.Header.Get("Authorization"); got != "Bearer ghs_x" {
			t.Errorf("authorization = %q", got)
		}

		_, _ = w.Write([]byte(`{"sha":"abc123","commit":{"tree":{"sha":"tree789"}}}`))
	})

	commit, err := c.GetCommit(context.Background(), "ghs_x", "acme/web", "main")
	if err != nil {
		t.Fatal(err)
	}

	if commit.SHA != "abc123" || commit.TreeSHA() != "tree789" {
		t.Fatalf("commit = %+v", commit)
	}
}

// The truncated flag is the difference between "this monorepo has no triggers"
// and "we stopped reading before we got to them".
func TestGetTreeSurfacesTruncation(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") != "1" {
			t.Errorf("the listing must be recursive: %s", r.URL.RawQuery)
		}

		_ = json.NewEncoder(w).Encode(Tree{
			SHA:       "t1",
			Entries:   []TreeEntry{{Path: ".voodu/pwa.yml", Type: "blob", SHA: "b1"}},
			Truncated: true,
		})
	})

	tree, err := c.GetTree(context.Background(), "ghs_x", "acme/web", "t1")
	if err != nil {
		t.Fatal(err)
	}

	if !tree.Truncated {
		t.Fatal("truncation was swallowed")
	}

	if len(tree.Entries) != 1 {
		t.Fatalf("entries = %+v", tree.Entries)
	}
}

// GitHub wraps base64 at 60 columns and the standard decoder rejects newlines.
func TestGetBlobDecodesWrappedBase64(t *testing.T) {
	content := strings.Repeat("name: PWA\n", 20)
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	wrapped := ""

	for i := 0; i < len(encoded); i += 60 {
		end := i + 60
		if end > len(encoded) {
			end = len(encoded)
		}

		wrapped += encoded[i:end] + "\n"
	}

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": wrapped, "encoding": "base64", "size": len(content),
		})
	})

	got, err := c.GetBlob(context.Background(), "ghs_x", "acme/web", "b1")
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != content {
		t.Fatalf("decoded %d bytes, want %d", len(got), len(content))
	}
}

// A repository with a huge file, by accident or otherwise, must not make the
// box allocate it.
func TestGetBlobRefusesAnOversizedFile(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": "", "encoding": "base64", "size": maxBlobBytes + 1,
		})
	})

	if _, err := c.GetBlob(context.Background(), "ghs_x", "acme/web", "b1"); err == nil {
		t.Fatal("an oversized blob must be refused")
	}
}

// INVARIANT III. A fork's pull-request head lives in the same object store and
// is reachable by SHA; without this check a deploy naming it would apply code
// nobody reviewed.
func TestIsAncestor(t *testing.T) {
	cases := map[string]bool{
		"behind":    true,  // head is an ancestor of the branch
		"identical": true,  // head IS the branch tip
		"ahead":     false, // head is beyond the branch — never merged
		"diverged":  false, // a fork's commit
	}

	for status, want := range cases {
		t.Run(status, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/compare/") {
					t.Errorf("path = %s", r.URL.Path)
				}

				_, _ = w.Write([]byte(`{"status":"` + status + `"}`))
			})

			got, err := c.IsAncestor(context.Background(), "ghs_x", "acme/web", "main", "abc")
			if err != nil {
				t.Fatal(err)
			}

			if got != want {
				t.Errorf("status %q: got %v, want %v", status, got, want)
			}
		})
	}
}

// A token problem and a missing repository have completely different fixes, so
// the caller has to be able to tell them apart.
func TestAPIErrorDistinguishesTheFailures(t *testing.T) {
	for status, check := range map[int]func(*APIError) bool{
		http.StatusUnauthorized: (*APIError).Unauthorized,
		http.StatusForbidden:    (*APIError).Unauthorized,
		http.StatusNotFound:     (*APIError).NotFound,
	} {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
		})

		_, err := c.GetCommit(context.Background(), "ghs_x", "acme/web", "main")

		apiErr, ok := err.(*APIError)
		if !ok {
			t.Fatalf("status %d produced %T, want *APIError", status, err)
		}

		if !check(apiErr) {
			t.Errorf("status %d was not classified", status)
		}
	}
}

// A GitHub error body can echo request details, and this string reaches an
// operator's screen.
func TestAPIErrorDoesNotEchoTheResponseBody(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Bad credentials for ghs_supersecret"}`))
	})

	_, err := c.GetCommit(context.Background(), "ghs_x", "acme/web", "main")

	if err == nil {
		t.Fatal("expected an error")
	}

	if strings.Contains(err.Error(), "ghs_supersecret") {
		t.Fatalf("the response body leaked into the error: %v", err)
	}
}

// THE PAYLOAD SHAPE, pinned against GitHub's own documented response.
//
// The tree is nested under `commit`. Read from the top level it comes back
// empty, the next call goes to `/git/trees/?recursive=1`, and GitHub answers
// 404 — which reads as "no such repository or ref" and sends the operator to
// look at their token. It shipped that way, and the test double is what let
// it: the fake returned the shape the struct expected rather than the shape
// GitHub sends, so every test agreed with the bug.
//
// This one uses a payload copied from GitHub's docs, including the top-level
// keys that must NOT be mistaken for it.
func TestGetCommitReadsTheTreeFromWhereGitHubPutsIt(t *testing.T) {
	body := `{
	  "sha": "6dcb09b",
	  "node_id": "MDY6Q29tbWl0",
	  "commit": {
	    "author": {"name": "Monalisa"},
	    "message": "Fix all the bugs",
	    "tree": {"sha": "6dcb09b5b5", "url": "https://api.github.com/..."}
	  },
	  "url": "https://api.github.com/repos/octocat/Hello-World/commits/6dcb09b",
	  "parents": []
	}`

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	commit, err := c.GetCommit(context.Background(), "ghs_x", "octocat/Hello-World", "main")
	if err != nil {
		t.Fatal(err)
	}

	if commit.TreeSHA() != "6dcb09b5b5" {
		t.Fatalf("TreeSHA = %q, want the sha under commit.tree", commit.TreeSHA())
	}

	// Empty is the failure this guards: it produces `/git/trees/?recursive=1`,
	// a URL that looks plausible and 404s.
	if commit.TreeSHA() == "" {
		t.Fatal("an empty tree SHA builds /git/trees/?recursive=1 and 404s")
	}
}
