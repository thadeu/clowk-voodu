package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/remote"
)

// newRemoteCmd wires `voodu remote add|list|remove|setup`. These are
// purely client-side: they manage git remotes on the developer's
// machine, so they never forward over SSH.
func newRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Manage Voodu server remotes (stored as git remotes)",
		Long: `Voodu reuses git remotes as the source of truth for where to ssh.
A remote is a label mapped to a user@host pair:

    voodu remote add staging ubuntu@staging.example.com
    voodu remote add prod-1  ubuntu@prod-1.example.com
    voodu apply -f prod.hcl --remote prod-1

The HCL manifest owns the app identity (scope + name). The remote
owns only the SSH target — one voodu host can run as many apps as
the HCL declares.`,
	}

	cmd.AddCommand(
		newRemoteAddCmd(),
		newRemoteListCmd(),
		newRemoteRemoveCmd(),
		newRemoteSetupCmd(),
	)

	return cmd
}

func newRemoteAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add NAME user@host[:identity]",
		Short: "Register a new Voodu remote (delegates to git remote add)",
		Long: `Adds a git remote pointing at user@host. Voodu reuses git
remotes as a lightweight key/value store for SSH targets — no
separate registry, no extra config file.

The optional :identity suffix embeds an SSH private key path so
voodu can use ssh -i automatically on every command (no need to
set VOODU_SSH_IDENTITY env or pass --identity each time):

  voodu remote add prod ubuntu@vps.example.com
  voodu remote add prod ubuntu@ec2.example.com:~/.ssh/ec2-prod.pem
  voodu remote add prod ubuntu@ec2.example.com:/etc/voodu/keys/prod.pem

Tilde (~) expands to the operator's home dir at parse time. The
suffix must look like a path (start with /, ~, ./, or ../) — bare
tokens are rejected to avoid confusion with the legacy app-shape
URLs older voodu versions used.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, url := args[0], args[1]

			if _, err := remote.ParseRemoteURL(url); err != nil {
				return err
			}

			out, err := exec.Command("git", "remote", "add", name, url).CombinedOutput()
			if err != nil {
				return fmt.Errorf("git remote add: %s", strings.TrimSpace(string(out)))
			}

			fmt.Printf("added remote %s -> %s\n", name, url)

			return nil
		},
	}
}

func newRemoteListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List Voodu remotes (those with user@host URLs)",
		RunE: func(cmd *cobra.Command, args []string) error {
			infos, err := remote.ListAll()
			if err != nil {
				return err
			}

			if len(infos) == 0 {
				fmt.Println("no voodu remotes configured")
				fmt.Println("add one with: voodu remote add <name> <user@host>")

				return nil
			}

			for _, info := range infos {
				fmt.Printf("%-16s %s\n", info.RemoteName, info.Host)
			}

			return nil
		},
	}
}

func newRemoteRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove NAME",
		Aliases: []string{"rm"},
		Short:   "Remove a git remote",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			out, err := exec.Command("git", "remote", "remove", name).CombinedOutput()
			if err != nil {
				return fmt.Errorf("git remote remove %s: %s", name, strings.TrimSpace(string(out)))
			}

			fmt.Printf("removed remote %s\n", name)

			return nil
		},
	}
}

// newRemoteSetupCmd bootstraps a Voodu server end-to-end: it verifies
// SSH, optionally scps a prebuilt binary, seeds the server-side
// directory layout + ~/.voodurc, and registers the matching git
// remote locally.
//
// Not covered here (by design): binary compilation, SSH key
// provisioning, default-plugin install, per-app setup. Compilation
// belongs in the release pipeline; keys are the user's responsibility;
// plugins land piecemeal via `voodu plugins install`; app directories
// are created on-demand by `voodu apply` (see ensureAppLayout).
//
// Production bootstrap (no `--binary`) is better served by piping
// `./install` over SSH directly — that script also installs Docker,
// systemd, etc. This command targets the dev-loop case: ship a
// freshly-built local binary to a host that already has the rest of
// the stack (Docker, systemd unit, voodu-controller running).
func newRemoteSetupCmd() *cobra.Command {
	var (
		identity    string
		binary      string
		installPath string
		skipSeed    bool
	)

	cmd := &cobra.Command{
		Use:   "setup NAME user@host",
		Short: "Bootstrap a Voodu server over SSH and register it as a git remote",
		Long: `Runs, in order:
  1. ssh preflight (BatchMode + ConnectTimeout)
  2. optional: scp --binary PATH to the server and install it
  3. seed the server-side dir layout + ~/.voodurc mode=server
  4. 'git remote add NAME user@host' locally (stores the target)

After this runs you can 'voodu apply -f voodu.hcl --remote NAME' from
any repo and it will ship over SSH to that host. The HCL owns which
apps get deployed; the remote just owns the destination.

For a clean-room install (Docker, systemd unit, voodu-controller
service), use the bundled installer instead:

  ssh user@host 'curl -fsSL https://raw.githubusercontent.com/thadeu/clowk-voodu/main/install | bash'

This 'remote setup' command is for the dev loop — ship a freshly-
built local binary with --binary to a host that already has the
controller running.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, host := args[0], args[1]

			if !strings.Contains(host, "@") {
				return fmt.Errorf("host must be user@hostname, got %q", host)
			}

			if err := sshPing(host, identity); err != nil {
				return err
			}

			fmt.Printf("✓ ssh to %s ok\n", host)

			if binary != "" {
				if err := installBinaryOverSSH(host, identity, binary, installPath); err != nil {
					return err
				}

				fmt.Printf("✓ installed %s → %s:%s\n", binary, host, installPath)
			}

			if !skipSeed {
				if err := seedServerLayout(host, identity); err != nil {
					return fmt.Errorf("seed server layout: %w", err)
				}

				fmt.Printf("✓ seeded /opt/voodu layout + ~/.voodurc on %s\n", host)
			}

			if _, err := remote.Lookup(name); err == nil {
				fmt.Printf("· git remote %q already configured\n", name)
			} else {
				out, err := exec.Command("git", "remote", "add", name, host).CombinedOutput()
				if err != nil {
					return fmt.Errorf("git remote add %s: %s", name, strings.TrimSpace(string(out)))
				}

				fmt.Printf("✓ git remote %q → %s\n", name, host)
			}

			fmt.Println()
			fmt.Printf("Done. Try: voodu apply -f voodu.hcl --remote %s\n", name)

			return nil
		},
	}

	cmd.Flags().StringVar(&identity, "identity", "", "SSH private key (-i)")
	cmd.Flags().StringVar(&binary, "binary", "", "upload this voodu binary to the server before seeding the layout")
	cmd.Flags().StringVar(&installPath, "install-path", "/usr/local/bin/voodu", "where to place --binary on the server")
	cmd.Flags().BoolVar(&skipSeed, "skip-seed", false, "do not seed /opt/voodu and ~/.voodurc on the remote")

	return cmd
}

// seedServerLayout runs the inline equivalent of what the bundled
// `install` script's server_setup() does, scoped to the dirs and
// ~/.voodurc the CLI's IsServerMode() check expects to find:
//
//   - /opt/voodu and its subdirs (apps, services, plugins, scripts,
//     state, volumes), owned by the SSH user
//   - ~/.voodurc with `mode=server`, so the CLI on the remote runs
//     locally instead of trying to SSH-forward to itself
//
// Used by `vd remote setup` as a cheap alternative to running the
// full installer over SSH — when the operator has already set up
// Docker + systemd + voodu-controller on the box and just wants to
// drop a freshly-built CLI binary in place.
func seedServerLayout(host, identity string) error {
	const script = `set -e
SUDO=""
if [ "$(id -u)" -ne 0 ]; then SUDO="sudo"; fi
ROOT="${VOODU_ROOT:-/opt/voodu}"
$SUDO install -m 0755 -d "$ROOT"
for sub in apps services plugins scripts state volumes; do
  $SUDO install -m 0755 -d "$ROOT/$sub"
done
USER_TO_OWN="$(whoami)"
if [ "$USER_TO_OWN" != "root" ]; then
  $SUDO chown -R "$USER_TO_OWN":"$USER_TO_OWN" "$ROOT"
fi
RC="$HOME/.voodurc"
if [ -f "$RC" ] && grep -q '^mode=' "$RC" 2>/dev/null; then
  sed -i 's/^mode=.*/mode=server/' "$RC"
else
  echo 'mode=server' >> "$RC"
fi
`

	return remoteRunShell(host, identity, script)
}

// sshPing runs `true` over SSH with a short timeout. BatchMode prevents
// password prompts that would hang non-interactive invocations.
func sshPing(host, identity string) error {
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5"}

	if identity != "" {
		args = append(args, "-i", identity)
	}

	args = append(args, host, "true")

	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh preflight to %s failed: %s", host, strings.TrimSpace(string(out)))
	}

	return nil
}

// installBinaryOverSSH scps the local binary and moves it into place
// with sudo. We use sudo unconditionally because installPath defaults
// to /usr/local/bin; users on sudoless hosts can point elsewhere via
// --install-path (e.g. $HOME/.local/bin/voodu).
func installBinaryOverSSH(host, identity, binary, installPath string) error {
	scpArgs := []string{"-q"}

	if identity != "" {
		scpArgs = append(scpArgs, "-i", identity)
	}

	tmpPath := "/tmp/voodu-install-" + randToken()
	scpArgs = append(scpArgs, binary, host+":"+tmpPath)

	out, err := exec.Command("scp", scpArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("scp %s: %s", binary, strings.TrimSpace(string(out)))
	}

	// If the install path is under the user's home, skip sudo; otherwise
	// assume /usr/local/bin or similar and require elevation.
	needSudo := !strings.HasPrefix(installPath, "/home/") && !strings.HasPrefix(installPath, "/root/") && !strings.Contains(installPath, "/.local/")

	moveCmd := fmt.Sprintf("chmod +x %s && mv %s %s", tmpPath, tmpPath, installPath)
	if needSudo {
		moveCmd = fmt.Sprintf("chmod +x %s && sudo mv %s %s", tmpPath, tmpPath, installPath)
	}

	return remoteRunShell(host, identity, moveCmd)
}

// remoteRunShell ssh-execs an arbitrary shell line. Used for install
// plumbing (chmod + mv with sudo) that doesn't map to `voodu` argv.
func remoteRunShell(host, identity, line string) error {
	args := []string{}

	if identity != "" {
		args = append(args, "-i", identity)
	}

	args = append(args, host, line)

	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("remote `%s`: %s", line, strings.TrimSpace(string(out)))
	}

	return nil
}

// randToken generates a short unique-ish suffix so parallel setups from
// different machines don't trample each other's /tmp uploads. Uses the
// process PID + nanosecond clock — not cryptographic, doesn't need to be.
func randToken() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}
