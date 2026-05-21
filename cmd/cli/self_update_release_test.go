package main

import (
	"strings"
	"testing"
)

// TestIsUpdateAvailable pins the version comparison semantics that
// drive the "→ behind" hint and the "client already on latest" early
// exit. The git-describe shape ("v0.9.6-5-g6a15220-dirty") is the
// only non-trivial case — dev builds that descend from a tag should
// NOT be flagged as behind that tag (the dev binary is ahead).
func TestIsUpdateAvailable(t *testing.T) {
	cases := []struct {
		name    string
		current string
		latest  string
		want    bool
	}{
		{"exact match", "v0.10.0", "v0.10.0", false},
		{"current is older tag", "v0.9.6", "v0.10.0", true},
		{"git-describe dev build above tag", "v0.9.6-5-g6a15220-dirty", "v0.9.6", false},
		{"git-describe but tag is newer", "v0.9.6-5-g6a15220-dirty", "v0.10.0", true},
		{"empty current treated as behind", "", "v0.10.0", true},
		{"empty latest is not a downgrade", "v0.10.0", "", false},
		{"both empty", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUpdateAvailable(c.current, c.latest); got != c.want {
				t.Errorf("isUpdateAvailable(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
			}
		})
	}
}

// TestArchiveName pins the goreleaser-compatible filename shape used
// by the release-download URL builder. The "0.10.0" (numeric, no v
// prefix) middle segment matches what .goreleaser.yml emits — drift
// here would point self-update at a 404.
func TestArchiveName(t *testing.T) {
	cases := []struct {
		name    string
		binary  string
		version string
		goos    string
		arch    string
		want    string
	}{
		{"cli linux arm64 with v prefix", "voodu", "v0.10.0", "linux", "arm64", "voodu_0.10.0_linux_arm64.tar.gz"},
		{"cli darwin amd64 with v prefix", "voodu", "v0.10.0", "darwin", "amd64", "voodu_0.10.0_darwin_amd64.tar.gz"},
		{"controller linux arm64", "voodu-controller", "v0.10.0", "linux", "arm64", "voodu-controller_0.10.0_linux_arm64.tar.gz"},
		{"version already stripped", "voodu", "0.10.0", "linux", "amd64", "voodu_0.10.0_linux_amd64.tar.gz"},
		{"prerelease tag", "voodu", "v0.10.0-rc.1", "linux", "amd64", "voodu_0.10.0-rc.1_linux_amd64.tar.gz"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := archiveName(c.binary, c.version, c.goos, c.arch); got != c.want {
				t.Errorf("archiveName(%q,%q,%q,%q) = %q, want %q",
					c.binary, c.version, c.goos, c.arch, got, c.want)
			}
		})
	}
}

// TestReleaseDownloadURL pins the URL pattern. Hard-coded
// github.com/<repo>/releases/download/<tag>/<archive> is what every
// downstream tool (curl, goreleaser, install.sh) builds — drift
// here would point self-update at a 404 too.
func TestReleaseDownloadURL(t *testing.T) {
	got := releaseDownloadURL("thadeu/clowk-voodu", "v0.10.0", "voodu_0.10.0_darwin_arm64.tar.gz")
	want := "https://github.com/thadeu/clowk-voodu/releases/download/v0.10.0/voodu_0.10.0_darwin_arm64.tar.gz"

	if got != want {
		t.Errorf("releaseDownloadURL = %q, want %q", got, want)
	}
}

// TestSha256HexDeterministic pins that identical input produces
// identical hex output. Receiver-side checksum verification depends
// on it; a regression would either pass tampered archives or reject
// clean ones.
func TestSha256HexDeterministic(t *testing.T) {
	a := sha256Hex([]byte("hello world"))
	b := sha256Hex([]byte("hello world"))

	if a != b {
		t.Errorf("non-deterministic sha256: %q vs %q", a, b)
	}

	if a != "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9" {
		t.Errorf("sha256 hex mismatch: got %q", a)
	}
}

// TestServerUpgradeScriptContainsCriticalSteps does a smoke test on
// the inline bash script — making sure key steps survive future
// refactors. A regression that dropped systemctl restart or sha256
// verify would silently break server self-update; a string-match
// test catches it before it ships.
func TestServerUpgradeScriptContainsCriticalSteps(t *testing.T) {
	script := serverUpgradeScript("v0.10.0")

	required := []string{
		"set -euo pipefail",       // fail-fast
		"sha256sum -c",            // verify before install
		"sudo systemctl stop",     // stop before swap
		"sudo install -m 0755",    // install with executable mode
		"voodu-controller",        // the daemon binary
		"sudo systemctl start",    // start after swap
		"VERSION=\"v0.10.0\"",     // pinned version threaded through
		"github.com/",             // download from github releases
		"checksums.txt",           // verify against goreleaser checksum file
	}

	for _, r := range required {
		if !strings.Contains(script, r) {
			t.Errorf("server upgrade script missing %q", r)
		}
	}
}
