package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/term"

	"go.voodu.clowk.in/internal/remote"
)

// defaultInstallURL is the canonical bootstrap script the first-apply
// preflight pipes over SSH when a remote is missing voodu. Overridable
// via VOODU_INSTALL_URL for forks, dev installs, or pinned versions.
const defaultInstallURL = "https://raw.githubusercontent.com/thadeu/clowk-voodu/main/install"

// preflightProbe is the boolean readiness snapshot the operator
// cares about. The on-disk dirs (`/opt/voodu/...`) are intentionally
// skipped here because secrets.Set materialises them on first
// apply via paths.EnsureAppLayout — voodu binary + a healthy
// controller are the two things the controller-side cannot
// bootstrap itself.
type preflightProbe struct {
	VooduBinary  bool
	ControllerUp bool
}

func (p preflightProbe) ready() bool {
	return p.VooduBinary && p.ControllerUp
}

// probeRemote runs a single SSH roundtrip that prints a tiny
// status block. Two checks worth running over the wire (vs. a
// pile of small commands): cheap and gives us a single failure
// surface for the bootstrap path.
//
//	voodu=ok|missing
//	controller=ok|down
//
// Anything else (parse failure, ssh dies, etc.) bubbles up as an
// error and the caller treats the remote as un-probable.
func probeRemote(host, identity string) (preflightProbe, error) {
	const script = `voodu_bin="missing"; if command -v voodu >/dev/null 2>&1; then voodu_bin="ok"; fi; echo "voodu=$voodu_bin"; ctl="down"; if curl -fsS -m 2 http://127.0.0.1:8686/health >/dev/null 2>&1; then ctl="ok"; fi; echo "controller=$ctl"`

	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5"}

	if identity != "" {
		args = append(args, "-i", identity)
	}

	args = append(args, host, script)

	out, err := exec.Command("ssh", args...).Output()
	if err != nil {
		return preflightProbe{}, fmt.Errorf("ssh probe %s: %w", host, err)
	}

	var probe preflightProbe

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		switch strings.TrimSpace(line) {
		case "voodu=ok":
			probe.VooduBinary = true
		case "controller=ok":
			probe.ControllerUp = true
		}
	}

	return probe, nil
}

// bootstrapRemote pipes the install script over SSH and streams
// its stdout to the local terminal so the operator sees apt/yum
// progress, docker pulls, and the systemd unit lighting up. Blocks
// until the remote `bash` exits.
//
// The remote runs `bash -s -- --server`: the trailing `--server`
// is the install script's flag (forces server mode, in case its
// auto-detection picks something else). VOODU_INSTALL_URL on the
// LOCAL env overrides the source URL — useful for forks, dev
// branches, or air-gapped mirrors.
func bootstrapRemote(host, identity string) error {
	url := defaultInstallURL
	if v := os.Getenv("VOODU_INSTALL_URL"); v != "" {
		url = v
	}

	// Single-line bash so SSH gets one argv. The remote shell pipes
	// curl into bash; the trailing -s -- --server forwards "--server"
	// as $1 to the install script (which would otherwise re-detect
	// mode based on $(uname)).
	line := fmt.Sprintf("curl -fsSL %s | bash -s -- --server", url)

	args := []string{"-o", "BatchMode=yes"}

	if identity != "" {
		args = append(args, "-i", identity)
	}

	args = append(args, host, line)

	cmd := exec.Command("ssh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh install pipe %s: %w", host, err)
	}

	return nil
}

// waitForController polls the remote probe until the controller
// reports ready or the timeout elapses. Used after bootstrap because
// `systemctl restart voodu-controller` returns instantly but the
// HTTP listener takes a beat to bind.
func waitForController(host, identity string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		probe, err := probeRemote(host, identity)
		if err == nil && probe.ControllerUp {
			return nil
		}

		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("controller did not respond on %s within %s", host, timeout)
}

// ensureRemoteReady is the entry-point preflight orchestrator. Fast
// path: a single probe round-trip; if everything is ready, returns
// silently in <100ms.
//
// Slow path: the remote is missing voodu (fresh VM, never seen this
// host before). Confirm with the operator (skipped via
// --auto-approve / VOODU_AUTO_APPROVE), then pipe the install
// script over SSH, then wait for the controller HTTP to come up.
//
// Designed for the `vd apply --remote prod` first-time UX: the
// operator creates an EC2/Droplet, configures SSH, writes
// voodu.hcl, installs the local CLI, and runs `vd apply` once —
// no separate "voodu remote setup" or bare-metal install step.
func ensureRemoteReady(info *remote.Info, identity string, autoApprove bool) error {
	probe, err := probeRemote(info.Host, identity)
	if err != nil {
		return fmt.Errorf("preflight probe: %w", err)
	}

	if probe.ready() {
		return nil
	}

	// Label-prefixed announce so the preflight section reads as
	// its own contextual block, matching the `[voodu/install]`
	// shape the install script uses. Internal probe details
	// ("voodu binary missing", "controller down") are operator-
	// noise on the happy path; the install script's own logs
	// surface the real progress.
	//
	// We surface the installer URL (not the full ssh+curl line)
	// because the URL is the one piece worth debugging — operators
	// using a fork or pinned version need to confirm which script
	// is being fetched. The ssh target is already obvious from
	// the --remote flag the operator just typed.
	preflightLog("First-time setup will run the voodu installer over SSH:")
	preflightLog(fmt.Sprintf("curl -fsSL %s | bash -s -- --server", currentInstallURL()))
	fmt.Fprintln(os.Stderr)

	if !autoApprove && !envAutoApprove() {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("remote not bootstrapped and running non-interactively. Re-run with --auto-approve (or VOODU_AUTO_APPROVE=1), or pre-bootstrap with: ssh HOST 'curl -fsSL " + currentInstallURL() + " | bash -s -- --server'")
		}

		ok, err := promptDeleteConfirm(os.Stdin, os.Stderr)
		if err != nil {
			return err
		}

		if !ok {
			return errors.New("preflight: operator declined bootstrap")
		}
	}

	if err := bootstrapRemote(info.Host, identity); err != nil {
		return fmt.Errorf("bootstrap %s: %w", info.Host, err)
	}

	// `enable_controller` in the install script restarts the unit;
	// the HTTP listener takes a moment to bind. 30s is generous —
	// systemd typically returns within ~2s on a warm box.
	if err := waitForController(info.Host, identity, 30*time.Second); err != nil {
		return err
	}

	// Blank line separates the install script's tail output from
	// the apply pipeline that follows ("✓ Checking remote
	// state..." etc.). The `installation complete` line at the
	// end of the install script already announces success — a
	// second "host ready" here was redundant.
	fmt.Fprintln(os.Stderr)

	return nil
}

func currentInstallURL() string {
	if v := os.Getenv("VOODU_INSTALL_URL"); v != "" {
		return v
	}

	return defaultInstallURL
}

// preflightLog emits a `[preflight]` line to stderr. Mirrors the
// install script's `[voodu/install]` shape so the operator gets
// two prefix-tagged sections (preflight then install) during a
// first-apply bootstrap. The colors are intentionally different
// — magenta for preflight, cyan for install — so the two flows
// visually separate at a glance: same shape, different color =
// "this is the next phase".
//
// Color is gated on stderr being a TTY: piping to a file or CI
// shouldn't carry literal escape codes. The plain-text fallback
// keeps the same `[preflight] msg` shape so log scrapers don't
// special-case TTY vs. non-TTY.
//
// Stderr (not stdout) on purpose: stderr is the universal
// "diagnostic / progress" channel; stdout stays clean for any
// machine-parseable output the apply pipeline emits later.
func preflightLog(msg string) {
	if writerIsTerminal(os.Stderr) {
		fmt.Fprintf(os.Stderr, "\x1b[35m[preflight]\x1b[0m %s\n", msg)
		return
	}

	fmt.Fprintf(os.Stderr, "[preflight] %s\n", msg)
}
