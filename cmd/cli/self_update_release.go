// self_update_release.go owns the GitHub-release plumbing for
// `vd self-update`: resolving "what's the latest release", computing
// the right archive URL for an (os, arch, kind) triple, and the
// version-comparison logic that decides "is this update needed".
//
// Lives separately from cmd_self_update.go so the cobra surface stays
// terse and the GitHub-API knowledge has one home for future swaps
// (e.g. self-hosted mirror, signed-manifest verification).

package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// updateRepo is the GitHub repo that hosts voodu's binary releases.
// Mirrors the install script's VOODU_INSTALL_REPO env default —
// keeping these in sync means `vd self-update` and `curl install`
// always pull from the same source.
//
// var (not const) so a future operator using a fork can override at
// build time via `-ldflags "-X main.updateRepo=acme/voodu-fork"`. The
// install script already supports the same pattern via env, and
// keeping the CLI in line means a homegrown distribution doesn't
// fragment between fresh-install and self-update flows.
var updateRepo = "thadeu/clowk-voodu"

// releaseInfo is the minimal subset of GitHub's release JSON we need.
// Decoding into a partial struct keeps the CLI insulated from
// upstream schema churn — adding fields to the GitHub API never
// breaks self-update.
type releaseInfo struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	HTMLURL    string `json:"html_url"`
}

// fetchLatestRelease queries GitHub's "releases/latest" endpoint.
// "latest" on GitHub means "highest non-prerelease tag" — exactly
// what an operator expects from a casual `vd self-update`. Operators
// who want a prerelease pass --version=<tag> explicitly.
//
// 10s timeout: GitHub's API is usually <1s; the cap exists so a
// network black hole (corporate proxy, sleeping VPN) doesn't hang
// the CLI past the operator's patience.
func fetchLatestRelease(repo string) (releaseInfo, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return releaseInfo{}, fmt.Errorf("build request: %w", err)
	}

	// GitHub's API rate-limits anonymous callers more aggressively
	// than user-agent-identified ones. Be a good citizen: a clear
	// UA string also helps when debugging release-traffic patterns
	// on the GitHub side.
	req.Header.Set("User-Agent", "voodu-cli/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return releaseInfo{}, fmt.Errorf("get %s: %w", url, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return releaseInfo{}, fmt.Errorf("github api returned %s", resp.Status)
	}

	var info releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return releaseInfo{}, fmt.Errorf("decode release: %w", err)
	}

	if info.TagName == "" {
		return releaseInfo{}, fmt.Errorf("github response had empty tag_name")
	}

	return info, nil
}

// downloadAndExtract fetches an archive URL and extracts the named
// file's contents to memory. Returns the binary bytes ready to be
// written wherever the caller's atomic-replace logic decides.
//
// In-memory rather than to a temp file because the archives are small
// (~10MB for the controller, ~6MB for the CLI) and the temp-file
// dance adds permissions friction on macOS (gatekeeper scans temp
// files mid-download and intermittently blocks the extract).
func downloadAndExtract(url, wantFile string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:gosec // URL is composed from a const repo + version
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}

	defer gz.Close()

	tr := tar.NewReader(gz)

	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("archive %s did not contain %q", url, wantFile)
		}

		if err != nil {
			return nil, fmt.Errorf("tar read: %w", err)
		}

		if h.Name != wantFile {
			continue
		}

		// Cap at 64MB to defend against a malicious or accidentally
		// huge archive. Production binaries are ~10MB; doubling the
		// ceiling several times still leaves a clean upper bound.
		buf := &cappedBuffer{max: 64 * 1024 * 1024}
		if _, err := io.Copy(buf, tr); err != nil {
			return nil, fmt.Errorf("extract %s: %w", wantFile, err)
		}

		return buf.bytes, nil
	}
}

// cappedBuffer is a sliced io.Writer that refuses to grow past
// `max` bytes. Used by downloadAndExtract to bound memory regardless
// of what comes off the wire.
type cappedBuffer struct {
	bytes []byte
	max   int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if len(c.bytes)+len(p) > c.max {
		return 0, fmt.Errorf("archive entry exceeded %d-byte ceiling", c.max)
	}

	c.bytes = append(c.bytes, p...)

	return len(p), nil
}

// fetchChecksums downloads the goreleaser-produced checksums.txt
// for the given release tag. Returns a map from filename to expected
// sha256 (hex string). Used by both the client and server upgrade
// paths to verify archives before installing them.
//
// Goreleaser's checksums.txt format:
//
//	<sha256_hex>  <filename>
//
// (Two spaces between hash and filename — standard `sha256sum`-tool
// shape so the install script can use `sha256sum -c` directly. Our
// in-process parser doesn't care about the spacing, only the two
// tokens per line.)
func fetchChecksums(repo, tag string) (map[string]string, error) {
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/checksums.txt", repo, tag)

	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("download checksums: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("checksums %s: %s", url, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB cap
	if err != nil {
		return nil, fmt.Errorf("read checksums: %w", err)
	}

	out := map[string]string{}

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}

		out[fields[1]] = fields[0]
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("checksums.txt parsed empty")
	}

	return out, nil
}

// sha256Hex returns the lowercase hex sha256 of b. Used by client
// upgrade to compare the downloaded binary's hash to the entry in
// checksums.txt.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// archiveName builds the goreleaser-style archive filename for a
// (binary, version, os, arch) tuple. Keep in sync with
// .goreleaser.yml's `name_template`. Goreleaser strips the leading
// "v" from the tag in archive names — our string concat matches.
//
//	binary="voodu"  ver="v0.10.0"  os="darwin"  arch="arm64"
//	→ voodu_0.10.0_darwin_arm64.tar.gz
func archiveName(binary, version, goos, arch string) string {
	return fmt.Sprintf("%s_%s_%s_%s.tar.gz", binary, strings.TrimPrefix(version, "v"), goos, arch)
}

// releaseDownloadURL composes the full GitHub-releases URL for an
// archive. Mirrors the install script's `https://github.com/{REPO}/
// releases/download/{VERSION}/{archive}` pattern.
func releaseDownloadURL(repo, tag, archive string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, archive)
}

// isUpdateAvailable returns true when `latest` is a meaningful upgrade
// from `current`. The comparison is intentionally permissive: any
// non-empty difference where current doesn't already contain latest
// counts as "available". Reasons:
//
//   - The user's local CLI shows shapes like "v0.9.6-5-g6a15220-dirty"
//     (git-describe-with-dirty). A strict semver comparison would
//     either reject those or require us to ship a semver parser.
//   - "Up-to-date" here means "your version matches the latest tag
//     verbatim OR contains it as a prefix" — the strictest workable
//     definition without a parser.
//
// Empty current is treated as "behind" (assume we should update). The
// CLI binary always has a version set at build time; truly empty
// means the binary wasn't built with the Makefile ldflags, which is
// itself a signal that an upgrade would help.
func isUpdateAvailable(current, latest string) bool {
	if latest == "" {
		return false
	}

	if current == "" {
		return true
	}

	// git-describe shape: "v0.9.6-5-g6a15220-dirty". If the latest
	// tag is a strict prefix and the rest of the string is the
	// expected git-describe suffix (dash + count + commit + maybe
	// "-dirty"), we treat current as ahead-of-the-tag — no update
	// needed unless the latest tag is itself the head of HEAD.
	if strings.HasPrefix(current, latest) && len(current) > len(latest) {
		// Operator's local is ahead of the released tag — they're on
		// a dev build derived from the tag. Don't downgrade them.
		return false
	}

	return current != latest
}
