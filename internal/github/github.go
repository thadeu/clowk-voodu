// Package github is the box's read-only view of a repository.
//
// READ-ONLY, and that is a product decision rather than a limit of this code:
// the integration is passive. It fetches trees, blobs and commit metadata and
// has no method that writes anything. A caller cannot commit, open a pull
// request or change a setting, because there is nothing here to call.
//
// TOKENS ARE NEVER STORED. Every method takes one as an argument. It arrives
// on the request that needs it, minted by the control plane for one repository
// and one hour, and it is gone when the call returns. A field holding a token
// would outlive the request that was authorised to use it.
package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is github.com's API. Overridable on the Client for GitHub
// Enterprise and for tests, which is the only reason it is a field.
const DefaultBaseURL = "https://api.github.com"

// DefaultTimeout bounds one API call. Generous next to a tarball, tight next
// to a page render: these are small JSON reads on the path of a screen the
// operator is waiting on.
const DefaultTimeout = 15 * time.Second

// maxBlobBytes caps a single file we will decode.
//
// A trigger file is a few hundred bytes and a manifest a few kilobytes. This
// is far above either, and exists so a repository with a 200MB YAML — by
// accident or otherwise — cannot make the box allocate it.
const maxBlobBytes = 1 << 20

// Client talks to the GitHub API. The zero value works.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}

	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}

	return &http.Client{Timeout: DefaultTimeout}
}

// Commit is the subset of a commit this box reads.
// Commit is the subset of `GET /repos/{owner}/{repo}/commits/{ref}` this box
// reads.
//
// THE TREE IS NESTED UNDER `commit`, not at the top level, and getting that
// wrong is silent: `TreeSHA` comes back "", the next call goes to
// `/git/trees/?recursive=1`, and GitHub answers 404 — which reads as "no such
// repository or ref" and sends the operator to look at their token.
//
// It shipped that way, and the test double is what let it: the fake returned
// the shape the struct expected instead of the shape GitHub sends, so every
// test agreed with the bug. The double now mirrors GitHub's real payload.
type Commit struct {
	SHA string `json:"sha"`

	// Commit is GitHub's inner object — the git commit, as opposed to the
	// repository's view of it that carries `sha`, `author`, `stats`.
	Commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	} `json:"commit"`
}

// TreeSHA is the root tree of this commit.
//
// A method rather than a field so the nesting lives in one place: callers ask
// what they mean, and the shape of GitHub's payload stays here.
func (c Commit) TreeSHA() string { return c.Commit.Tree.SHA }

// TreeEntry is one path in a repository tree.
type TreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"` // blob | tree | commit
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

// Tree is a recursive listing.
type Tree struct {
	SHA     string      `json:"sha"`
	Entries []TreeEntry `json:"tree"`

	// Truncated means GitHub stopped early because the repository is large.
	// The caller has to notice: a truncated listing that looks complete is how
	// a monorepo gets told it has no triggers.
	Truncated bool `json:"truncated"`
}

// GetCommit reads a commit's metadata.
//
// The reason this exists rather than going straight to the tarball: it returns
// `tree.sha` in one small request. A commit whose tree matches the last
// successful deploy has nothing to build, and knowing that BEFORE the download
// is the difference between skipping a deploy and paying for four hundred
// megabytes to discover the build id would have matched.
func (c *Client) GetCommit(ctx context.Context, token, repo, ref string) (Commit, error) {
	var commit Commit

	path := fmt.Sprintf("/repos/%s/commits/%s", repo, url.PathEscape(ref))

	if err := c.getJSON(ctx, token, path, &commit); err != nil {
		return Commit{}, err
	}

	return commit, nil
}

// Repo is the subset of repository metadata this box reads.
type Repo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

// GetRepo reads a repository's metadata.
//
// Exists for one job: the default branch. A console previewing a repository it
// has not configured yet knows the name and nothing else, and guessing `main`
// would be wrong for every repository still on `master` — silently, by showing
// an empty list instead of an error.
func (c *Client) GetRepo(ctx context.Context, token, repo string) (Repo, error) {
	var out Repo

	if err := c.getJSON(ctx, token, "/repos/"+repo, &out); err != nil {
		return Repo{}, err
	}

	return out, nil
}

// GetTree lists a tree recursively.
//
// Recursive because the alternative is one request per directory level, and
// `.voodu/` may be nested. The response carries paths and blob SHAs but no
// content, so it stays small even for a large repository.
func (c *Client) GetTree(ctx context.Context, token, repo, treeSHA string) (Tree, error) {
	var tree Tree

	path := fmt.Sprintf("/repos/%s/git/trees/%s?recursive=1", repo, url.PathEscape(treeSHA))

	if err := c.getJSON(ctx, token, path, &tree); err != nil {
		return Tree{}, err
	}

	return tree, nil
}

// GetBlob reads one file's content by blob SHA.
//
// By SHA and not by path: the SHA came from a tree we already read, so this
// cannot drift to a different revision between the listing and the read.
func (c *Client) GetBlob(ctx context.Context, token, repo, blobSHA string) ([]byte, error) {
	var blob struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int64  `json:"size"`
	}

	path := fmt.Sprintf("/repos/%s/git/blobs/%s", repo, url.PathEscape(blobSHA))

	if err := c.getJSON(ctx, token, path, &blob); err != nil {
		return nil, err
	}

	if blob.Size > maxBlobBytes {
		return nil, fmt.Errorf("github: blob %s is %d bytes, over the %d cap", blobSHA, blob.Size, maxBlobBytes)
	}

	if blob.Encoding != "base64" {
		return nil, fmt.Errorf("github: blob %s has unexpected encoding %q", blobSHA, blob.Encoding)
	}

	// GitHub wraps base64 at 60 columns; the standard decoder rejects the
	// newlines, so they come out first.
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("github: decode blob %s: %w", blobSHA, err)
	}

	return decoded, nil
}

// IsAncestor reports whether `sha` is reachable from `branch`.
//
// INVARIANT III of the deploy plane, and the reason it cannot be skipped: a
// pull request from a fork lives in the same object store and its head commit
// is reachable by SHA. Without this check, a deploy naming that SHA would
// apply code nobody reviewed and that was never on the pinned branch.
//
// `/compare/{base}...{head}` answers it directly: GitHub reports `behind` or
// `identical` when head is an ancestor of base, and `ahead` or `diverged` when
// it is not.
func (c *Client) IsAncestor(ctx context.Context, token, repo, branch, sha string) (bool, error) {
	var compare struct {
		Status string `json:"status"`
	}

	path := fmt.Sprintf("/repos/%s/compare/%s...%s", repo, url.PathEscape(branch), url.PathEscape(sha))

	if err := c.getJSON(ctx, token, path, &compare); err != nil {
		return false, err
	}

	return compare.Status == "behind" || compare.Status == "identical", nil
}

func (c *Client) getJSON(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+path, nil)
	if err != nil {
		return fmt.Errorf("github: build request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("github: %s: %w", path, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The body is read but NOT returned verbatim: a GitHub error can echo
		// request details, and this string reaches an operator's screen.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

		return &APIError{Status: resp.StatusCode, Path: path}
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); err != nil {
		return fmt.Errorf("github: decode %s: %w", path, err)
	}

	return nil
}

// APIError carries the status so callers can tell the failures apart —
// 401/403 is a token problem, 404 is a repository or ref problem, and the two
// have completely different fixes.
type APIError struct {
	Status int
	Path   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s returned %d", e.Path, e.Status)
}

// Unauthorized reports a token that is missing, expired or not scoped to this
// repository.
func (e *APIError) Unauthorized() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// NotFound reports a repository, ref or blob that does not exist — or that the
// token cannot see, which GitHub deliberately reports the same way.
func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

// MaxTarballBytes caps a repository download.
//
// Generous — a working tree, not a repository with its history, since the
// tarball is `git archive`-equivalent and carries no `.git`. It exists so a
// repository with a gigabyte of committed binaries cannot fill the box's disk
// before anybody notices.
const MaxTarballBytes = 2 << 30 // 2 GiB

// Tarball streams the repository tree at `sha` as gzipped tar.
//
// The ONLY call here that moves real bytes, and the reason the rest of this
// package exists: a manifest is a few kilobytes and a tree can be hundreds of
// megabytes, so everything that can be answered without this is.
//
// GitHub answers with a redirect to codeload. Go's client drops the
// Authorization header when a redirect crosses to another host, which is what
// keeps the token off codeload — asserted by a test rather than trusted,
// because it is a property of the standard library that a future custom
// CheckRedirect could quietly remove.
func (c *Client) Tarball(ctx context.Context, token, repo, sha string) (io.ReadCloser, error) {
	path := fmt.Sprintf("/repos/%s/tarball/%s", repo, url.PathEscape(sha))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+path, nil)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s: %w", path, err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()

		return nil, &APIError{Status: resp.StatusCode, Path: path}
	}

	// The cap is enforced on the READER rather than by trusting
	// Content-Length, which a redirect target need not send at all.
	return &cappedReadCloser{r: io.LimitReader(resp.Body, MaxTarballBytes+1), c: resp.Body}, nil
}

// cappedReadCloser turns "read past the cap" into an error instead of silently
// truncating — a half-extracted repository would build something that never
// existed.
type cappedReadCloser struct {
	r    io.Reader
	c    io.Closer
	read int64
}

func (t *cappedReadCloser) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.read += int64(n)

	if t.read > MaxTarballBytes {
		return n, fmt.Errorf("github: repository archive exceeds %d bytes", MaxTarballBytes)
	}

	return n, err
}

func (t *cappedReadCloser) Close() error { return t.c.Close() }
