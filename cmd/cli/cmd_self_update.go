// cmd_self_update.go owns the `vd self-update` command surface — the
// operator-facing entry point for refreshing voodu binaries without
// SSHing into the server or re-running curl|bash by hand.
//
// Flow:
//
//	$ vd self-update
//	  Detected: client=v0.9.6, server (staging)=v0.9.5, latest=v0.10.0
//	  Update server 'staging' to v0.10.0? [y/N]: y
//	    → SSH + download + sha256 verify + systemctl swap
//	  Update client to v0.10.0? [y/N]: y
//	    → Download + sha256 verify + atomic replace + (macOS) codesign
//
// Two independent confirmations — operators commonly want to update
// the server only (production bump) or the client only (dev box catching
// up). Pinning to a specific version via --version skips both prompts
// when paired with --yes.

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/remote"
)

// selfUpdateFlags collects the CLI knobs for `vd self-update`.
type selfUpdateFlags struct {
	remote  string
	version string
	yes     bool
	server  bool // pre-decided answer for the server prompt (set by --server)
	client  bool // pre-decided answer for the client prompt (set by --client)
}

func newSelfUpdateCmd() *cobra.Command {
	var f selfUpdateFlags

	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update the voodu CLI and/or controller binaries",
		Long: `self-update refreshes the voodu binaries from GitHub releases.

By default it shows current and latest versions, then asks y/N for
each target (server via SSH, then client locally).

Pin a specific version with --version=vX.Y.Z. Use --yes to skip
prompts (server + client both updated). Use --server-only or
--client-only to target a single side without interactive prompts.

Server upgrade requires SSH to the configured remote. The remote
must have curl + tar + sudo (default on Ubuntu/Debian/RHEL) and
internet access to github.com.

Client upgrade replaces the binary at $(which voodu). When the
target path requires root (e.g. /usr/local/bin), the CLI shells
out to sudo install — operators see a sudo prompt for their
password.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSelfUpdate(cmd, f)
		},
	}

	cmd.Flags().StringVarP(&f.remote, "remote", "r", "", "remote to update (defaults to the configured default remote)")
	cmd.Flags().StringVar(&f.version, "version", "", "pin to a specific release tag (e.g. v0.10.0); defaults to latest")
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "skip all confirmations (assume yes for any detected upgrade)")
	cmd.Flags().BoolVar(&f.server, "server-only", false, "update only the server (no client prompt)")
	cmd.Flags().BoolVar(&f.client, "client-only", false, "update only the client (no server prompt)")

	return cmd
}

// runSelfUpdate is the top-level orchestrator. Steps:
//
//  1. Resolve target version (latest from GitHub, or --version).
//  2. Resolve remote info if a server update might happen.
//  3. Detect current versions (client always; server via SSH if
//     remote is configured).
//  4. Prompt y/N for each (unless --yes / --server-only / --client-only).
//  5. Execute confirmed upgrades.
func runSelfUpdate(cmd *cobra.Command, f selfUpdateFlags) error {
	// Mutual-exclusivity: --server-only AND --client-only is a typo
	// guarantee, not a feature. Fail loud.
	if f.server && f.client {
		return fmt.Errorf("--server-only and --client-only are mutually exclusive")
	}

	// Resolve target version once — both client and server paths
	// use the same tag so the cluster stays version-aligned.
	targetTag := f.version

	if targetTag == "" {
		latest, err := fetchLatestRelease(updateRepo)
		if err != nil {
			return fmt.Errorf("resolve latest release: %w", err)
		}

		targetTag = latest.TagName

		fmt.Fprintf(cmd.OutOrStdout(), "Latest release: %s\n", colorize(cMint400, targetTag))
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Pinned version: %s\n", colorize(cMint400, targetTag))
	}

	// Resolve SSH remote (if any). When --client-only is set we skip
	// the lookup entirely — saves a config read for the common
	// laptop-only upgrade flow.
	var (
		info     *remote.Info
		identity string
	)

	if !f.client {
		i, err := remote.Resolve(f.remote)
		if err == nil && i != nil {
			info = i
			identity = os.Getenv("VOODU_SSH_IDENTITY")

			if identity == "" {
				identity = i.Identity
			}
		}
		// nil info is fine — `vd self-update` on a no-remote setup
		// still wants to upgrade the local client. Only fail when
		// the operator explicitly asked for server-only without a
		// remote configured.
		if f.server && info == nil {
			return fmt.Errorf("--server-only requires a configured remote (see `vd remote --help`)")
		}
	}

	// Detect current versions — best-effort. A version probe over
	// SSH can fail transiently (network blip, old binary missing
	// the --version flag); we still let the operator proceed if
	// they accept the prompt.
	clientCurrent := version

	var serverCurrent string

	if info != nil && !f.client {
		serverCurrent = detectServerVersion(info, identity)
	}

	printVersionTable(cmd.OutOrStdout(), clientCurrent, serverCurrent, info, targetTag)

	// Decide server step.
	if !f.client && info != nil {
		want := f.server || f.yes

		if !want {
			ok, err := promptYesNo(cmd, fmt.Sprintf("Update server '%s' from %s to %s?", info.RemoteName, displayVersion(serverCurrent), targetTag))
			if err != nil {
				return err
			}

			want = ok
		}

		if want {
			if err := upgradeServer(cmd.OutOrStdout(), info, identity, targetTag); err != nil {
				return fmt.Errorf("server upgrade failed: %w", err)
			}
		}
	}

	// Decide client step.
	if !f.server {
		want := f.client || f.yes

		if !want {
			if !isUpdateAvailable(clientCurrent, targetTag) {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%s client already on %s\n", check(), targetTag)
				return nil
			}

			ok, err := promptYesNo(cmd, fmt.Sprintf("Update client from %s to %s?", displayVersion(clientCurrent), targetTag))
			if err != nil {
				return err
			}

			want = ok
		}

		if want {
			if err := upgradeClient(cmd.OutOrStdout(), targetTag); err != nil {
				return fmt.Errorf("client upgrade failed: %w", err)
			}
		}
	}

	return nil
}

// printVersionTable shows the operator a compact "where am I, where
// am I going" view before the prompts. Aligned columns let the eye
// scan for "behind" rows without reading every value.
func printVersionTable(w io.Writer, clientCurrent, serverCurrent string, info *remote.Info, targetTag string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Versions:")
	fmt.Fprintf(w, "  client                   %s%s\n", displayVersion(clientCurrent), behindHint(clientCurrent, targetTag))

	if info != nil {
		label := fmt.Sprintf("server (%s)", info.RemoteName)
		fmt.Fprintf(w, "  %-24s %s%s\n", label, displayVersion(serverCurrent), behindHint(serverCurrent, targetTag))
	}

	fmt.Fprintln(w)
}

// displayVersion returns a human-friendly version string, replacing
// empty with "(unknown)" so the version table never has blank cells.
func displayVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(unknown)"
	}

	return v
}

// behindHint returns a colored " — behind" / " — up to date" suffix
// for the version table. Skipped when current is unknown so the
// table doesn't lie about state we couldn't detect.
func behindHint(current, target string) string {
	if strings.TrimSpace(current) == "" || strings.TrimSpace(target) == "" {
		return ""
	}

	if isUpdateAvailable(current, target) {
		return "  " + colorize(cAmber, "→ behind")
	}

	return "  " + dim("up to date")
}

// detectServerVersion runs `voodu --version` over SSH. Best-effort:
// errors don't fail the upgrade flow — we just leave serverCurrent
// empty and let the operator decide whether to push the upgrade.
func detectServerVersion(info *remote.Info, identity string) string {
	var stdout bytes.Buffer

	code, err := remote.Forward(info, []string{"--version"}, remote.ForwardOptions{
		Identity: identity,
		Stdout:   &stdout,
		Stderr:   io.Discard,
	})
	if err != nil || code != 0 {
		return ""
	}

	out := strings.TrimSpace(stdout.String())

	// `voodu --version` output is something like
	// "voodu v0.9.6-5-g6a15220-dirty (commit: 6a15220, built: ...)" —
	// peel the leading "voodu " and the trailing paren block so the
	// table column stays compact.
	out = strings.TrimPrefix(out, "voodu ")

	if i := strings.Index(out, " ("); i > 0 {
		out = out[:i]
	}

	return out
}

// promptYesNo writes the prompt to stderr and reads a y/N answer from
// stdin. Default is no — operators who want yes have to type it.
// Reuses the existing promptConfirm idiom from apply_forwarded.go.
func promptYesNo(cmd *cobra.Command, message string) (bool, error) {
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N]: ", message)

	return promptConfirm(os.Stdin, cmd.ErrOrStderr())
}

// upgradeServer drives the SSH-based controller upgrade. Sends an
// inline bash script that downloads, verifies, swaps the binaries,
// and restarts systemd. Output streams to the CLI in real time so
// operators see each step land — same visual rhythm as `vd apply`.
//
// The script is self-contained (no externally-sourced scripts) so an
// air-gapped or proxied remote with curl + tar + sudo is sufficient.
// Idempotent in the sense that an interrupted run leaves the host
// either on the OLD binary (failure before systemctl restart) or
// the NEW binary (failure after). The systemctl step is last; the
// service either stays up on the old or comes up on the new.
func upgradeServer(out io.Writer, info *remote.Info, identity, targetTag string) error {
	fmt.Fprintf(out, "\n%s upgrading server '%s' to %s\n", arrow(), info.RemoteName, targetTag)

	script := serverUpgradeScript(targetTag)

	// We hand the script to `bash -s` on the remote — stdin
	// carries the script body so we don't have to shell-escape it
	// inside the args. Resort to `RemoteBinary: "bash"` so SSH
	// invokes bash directly instead of trying to find `voodu`.
	code, err := remote.Forward(info, []string{"-s"}, remote.ForwardOptions{
		Identity:     identity,
		RemoteBinary: "bash",
		Stdin:        strings.NewReader(script),
		Stdout:       out,
		Stderr:       out,
	})
	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("remote upgrade exit %d", code)
	}

	fmt.Fprintf(out, "%s server now on %s\n", check(), targetTag)

	return nil
}

// serverUpgradeScript builds the bash one-shot the CLI ships over
// SSH. Inline-defined (rather than //go:embed) so a reader of this
// file sees the exact remote behaviour without jumping files.
//
// Quirks:
//   - Uses `set -euo pipefail` so any step's failure aborts cleanly.
//   - sha256sum verification fails the script before any systemctl
//     touch — a tampered archive can't half-install.
//   - systemctl stop before binary swap is intentional: replacing
//     a running ELF on Linux works (the kernel keeps the old fd
//     open) but in-process probes can race the restart. Stop-then-
//     swap is the boring safe order.
//   - Both `voodu` and `voodu-controller` get replaced so an
//     operator SSHing in finds a CLI that matches the controller.
func serverUpgradeScript(targetTag string) string {
	return `set -euo pipefail

VERSION="` + targetTag + `"
NUM_VERSION="${VERSION#v}"
REPO="` + updateRepo + `"

ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64) ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH_RAW" >&2; exit 1 ;;
esac

OS="$(uname -s | tr A-Z a-z)"
if [ "$OS" != "linux" ]; then
  echo "self-update only supports linux servers (got $OS)" >&2
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"

base="https://github.com/${REPO}/releases/download/${VERSION}"
controller_archive="voodu-controller_${NUM_VERSION}_${OS}_${ARCH}.tar.gz"
cli_archive="voodu_${NUM_VERSION}_${OS}_${ARCH}.tar.gz"

echo "→ downloading checksums.txt"
curl -fsSL "${base}/checksums.txt" -o checksums.txt

echo "→ downloading ${controller_archive}"
curl -fsSL "${base}/${controller_archive}" -o "${controller_archive}"

echo "→ downloading ${cli_archive}"
curl -fsSL "${base}/${cli_archive}" -o "${cli_archive}"

echo "→ verifying sha256"
grep -E "  (${controller_archive}|${cli_archive})$" checksums.txt > expected.txt
sha256sum -c expected.txt

echo "→ extracting archives"
tar xzf "${controller_archive}"
tar xzf "${cli_archive}"

# Sanity: extracted binaries must be executable + non-empty.
[ -s ./voodu-controller ] || { echo "controller binary missing/empty" >&2; exit 1; }
[ -s ./voodu ] || { echo "cli binary missing/empty" >&2; exit 1; }

echo "→ stopping voodu-controller.service"
sudo systemctl stop voodu-controller.service || true

echo "→ installing /usr/local/bin/voodu-controller"
sudo install -m 0755 ./voodu-controller /usr/local/bin/voodu-controller

echo "→ installing /usr/local/bin/voodu"
sudo install -m 0755 ./voodu /usr/local/bin/voodu

echo "→ starting voodu-controller.service"
sudo systemctl start voodu-controller.service

echo "→ verifying"
/usr/local/bin/voodu --version || true
/usr/local/bin/voodu-controller --version || true
`
}

// upgradeClient drives the local binary swap. Steps:
//
//  1. Resolve current executable path.
//  2. Download voodu_<v>_<os>_<arch>.tar.gz + checksums.txt.
//  3. Verify sha256.
//  4. Atomic replace: write to .new in same dir, chmod 0755,
//     os.Rename over current. Falls back to `sudo install` when
//     the dir isn't writable (typical for /usr/local/bin).
//  5. On macOS, codesign --force --sign - the new binary so
//     gatekeeper doesn't quarantine it.
//
// Best-effort error reporting at every step so an operator sees
// exactly which boundary failed (download / verify / replace /
// codesign).
func upgradeClient(out io.Writer, targetTag string) error {
	fmt.Fprintf(out, "\n%s upgrading client to %s\n", arrow(), targetTag)

	currentPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}

	// Resolve symlinks so we replace the underlying file rather
	// than the symlink itself. `/usr/local/bin/vd → voodu` is the
	// common shape; updating only `vd` would leave the real binary
	// stale and break parallel symlinks.
	if resolved, err := filepath.EvalSymlinks(currentPath); err == nil {
		currentPath = resolved
	}

	num := strings.TrimPrefix(targetTag, "v")
	archive := archiveName("voodu", targetTag, runtime.GOOS, runtime.GOARCH)

	checksums, err := fetchChecksums(updateRepo, targetTag)
	if err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}

	expected, ok := checksums[archive]
	if !ok {
		return fmt.Errorf("no checksum for %s in checksums.txt", archive)
	}

	fmt.Fprintf(out, "  %s downloading %s\n", dim("→"), archive)

	archiveURL := releaseDownloadURL(updateRepo, targetTag, archive)

	// Fetch the WHOLE archive (not just the binary inside) so we can
	// hash the archive bytes for checksum comparison — checksums.txt
	// hashes the .tar.gz, not the binary inside. Verify-then-extract.
	archiveBytes, err := fetchURL(archiveURL)
	if err != nil {
		return fmt.Errorf("download archive: %w", err)
	}

	if got := sha256Hex(archiveBytes); got != expected {
		return fmt.Errorf("checksum mismatch: want %s, got %s (refusing to install tampered archive)", expected, got)
	}

	fmt.Fprintf(out, "  %s checksum verified\n", check())

	binaryBytes, err := extractFromArchiveBytes(archiveBytes, "voodu")
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	if err := atomicInstallBinary(currentPath, binaryBytes); err != nil {
		return fmt.Errorf("install: %w", err)
	}

	fmt.Fprintf(out, "  %s replaced %s\n", check(), currentPath)

	if runtime.GOOS == "darwin" {
		if err := codesignBinary(currentPath); err != nil {
			// Codesign failure is non-fatal — the binary still
			// runs, just may prompt the user on first invocation.
			fmt.Fprintf(out, "  %s codesign failed: %v (run `sudo codesign --force --sign - %s` to fix)\n", warn(), err, currentPath)
		} else {
			fmt.Fprintf(out, "  %s codesigned (macOS gatekeeper)\n", check())
		}
	}

	fmt.Fprintf(out, "%s client now on %s\n", check(), targetTag)

	// num is unused inline — kept for future formatting if we want
	// to show "v0.10.0 (0.10.0)" anywhere.
	_ = num

	return nil
}

// fetchURL is a small helper that GETs a URL and returns the body
// bytes. Used for whole-archive download (so we can hash before
// extracting).
func fetchURL(url string) ([]byte, error) {
	resp, err := httpGet(url)
	if err != nil {
		return nil, err
	}

	defer resp.Close()

	return io.ReadAll(io.LimitReader(resp, 128<<20)) // 128MB cap
}

// httpGet is a tiny indirection so tests can stub a hermetic fixture
// without monkey-patching net/http. Production wires the real client.
var httpGet = func(url string) (io.ReadCloser, error) {
	client := &http.Client{Timeout: 60 * time.Second}

	resp, err := client.Get(url) //nolint:noctx,gosec
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("get %s: %s", url, resp.Status)
	}

	return resp.Body, nil
}

// extractFromArchiveBytes is the in-memory counterpart of
// downloadAndExtract for the case where we already have the archive
// bytes (because we needed to hash them first).
func extractFromArchiveBytes(archive []byte, wantFile string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}

	defer gz.Close()

	tr := tar.NewReader(gz)

	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("archive did not contain %q", wantFile)
		}

		if err != nil {
			return nil, fmt.Errorf("tar read: %w", err)
		}

		if h.Name != wantFile {
			continue
		}

		buf := &cappedBuffer{max: 128 * 1024 * 1024}
		if _, err := io.Copy(buf, tr); err != nil {
			return nil, fmt.Errorf("extract %s: %w", wantFile, err)
		}

		return buf.bytes, nil
	}
}

// atomicInstallBinary writes the bytes into the target path. When
// the target directory is owned by root and the calling user can't
// write to it directly, falls back to `sudo install` — prompting
// the operator for their password — so the swap still lands without
// the operator having to re-run the whole command with sudo.
func atomicInstallBinary(path string, content []byte) error {
	dir := filepath.Dir(path)

	// Fast path: caller can write to the directory directly. Atomic
	// replace via temp + rename.
	if canWrite(dir) {
		tmp, err := os.CreateTemp(dir, ".voodu-new-*")
		if err != nil {
			return fmt.Errorf("create temp: %w", err)
		}

		tmpName := tmp.Name()

		defer os.Remove(tmpName)

		if _, err := tmp.Write(content); err != nil {
			tmp.Close()
			return fmt.Errorf("write temp: %w", err)
		}

		if err := tmp.Close(); err != nil {
			return fmt.Errorf("close temp: %w", err)
		}

		if err := os.Chmod(tmpName, 0o755); err != nil {
			return fmt.Errorf("chmod temp: %w", err)
		}

		return os.Rename(tmpName, path)
	}

	// Slow path: shell out to sudo install. Operator sees a
	// password prompt the first time per sudo session.
	tmp, err := os.CreateTemp("", ".voodu-new-*")
	if err != nil {
		return fmt.Errorf("create temp (sudo path): %w", err)
	}

	tmpName := tmp.Name()

	defer os.Remove(tmpName)

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	cmd := exec.Command("sudo", "install", "-m", "0755", tmpName, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo install %s: %w", path, err)
	}

	return nil
}

// canWrite returns true when the current process can create a file
// in `dir`. Used by atomicInstallBinary to pick the fast path. The
// probe is cheap (a temp create + immediate delete).
func canWrite(dir string) bool {
	f, err := os.CreateTemp(dir, ".voodu-probe-*")
	if err != nil {
		return false
	}

	_ = f.Close()
	_ = os.Remove(f.Name())

	return true
}

// codesignBinary re-signs the new binary with a placeholder identity
// (ad-hoc) so macOS gatekeeper accepts it. Same pattern the Makefile
// uses in install-cli. Non-fatal on failure — gatekeeper will prompt
// the operator on first run instead.
func codesignBinary(path string) error {
	cmd := exec.Command("sudo", "codesign", "--force", "--sign", "-", path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	return cmd.Run()
}
