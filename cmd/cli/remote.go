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
		Long: `Voodu reuses git remotes as the source of truth for where to send
commands. A remote is a label mapped to a user@host:app triple:

    voodu remote add api ubuntu@prod.example.com:api
    voodu apply -f voodu.hcl -a api    # deploys to prod.example.com
    voodu config:set FOO=bar -a api    # forwards to prod.example.com

Commands with -a APP pick the remote named APP when one exists;
otherwise they fall back to the default remote named "voodu".`,
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
		Use:   "add NAME user@host:APP",
		Short: "Register a new Voodu remote (delegates to git remote add)",
		Args:  cobra.ExactArgs(2),
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
		Short: "List Voodu remotes (those with user@host:app URLs)",
		RunE: func(cmd *cobra.Command, args []string) error {
			infos, err := remote.ListAll()
			if err != nil {
				return err
			}

			if len(infos) == 0 {
				fmt.Println("no voodu remotes configured")
				fmt.Println("add one with: voodu remote add <name> <user@host:app>")

				return nil
			}

			for _, info := range infos {
				fmt.Printf("%-16s %s:%s\n", info.RemoteName, info.Host, info.App)
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
// SSH, optionally scps a prebuilt binary, runs `voodu setup` on the far
// side, creates an app, and registers the matching git remote locally.
//
// Not covered here (by design): binary compilation, SSH key
// provisioning, default-plugin install. Compilation belongs in the
// release pipeline; keys are the user's responsibility; plugins land
// piecemeal via `voodu plugins install`.
func newRemoteSetupCmd() *cobra.Command {
	var (
		identity    string
		binary      string
		installPath string
		skipSetup   bool
		skipApp     bool
	)

	cmd := &cobra.Command{
		Use:   "setup NAME user@host APP",
		Short: "Bootstrap a Voodu server over SSH and register it as a git remote",
		Long: `Runs, in order:
  1. ssh preflight (BatchMode + ConnectTimeout)
  2. optional: scp --binary PATH to the server and install it
  3. 'voodu setup' on the remote (idempotent)
  4. 'voodu apps create APP' on the remote (ignored if it exists)
  5. 'git remote add NAME user@host:APP' locally (stores the target)

After this runs you can 'voodu apply -f voodu.hcl -a APP' from this
repo and it will ship over SSH to APP on HOST.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, host, app := args[0], args[1], args[2]

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

			if !skipSetup {
				if err := remoteRun(host, identity, "voodu", "setup"); err != nil {
					return fmt.Errorf("remote voodu setup: %w", err)
				}

				fmt.Printf("✓ voodu setup ran on %s\n", host)
			}

			if !skipApp {
				// `apps create` is not idempotent, so swallow "already
				// exists" sort of errors by checking via apps list first.
				if exists, err := remoteAppExists(host, identity, app); err != nil {
					return err
				} else if !exists {
					if err := remoteRun(host, identity, "voodu", "apps", "create", app); err != nil {
						return fmt.Errorf("remote voodu apps create: %w", err)
					}

					fmt.Printf("✓ app %q created on %s\n", app, host)
				} else {
					fmt.Printf("· app %q already exists on %s\n", app, host)
				}
			}

			url := host + ":" + app

			if _, err := remote.Lookup(name); err == nil {
				fmt.Printf("· git remote %q already configured\n", name)
			} else {
				out, err := exec.Command("git", "remote", "add", name, url).CombinedOutput()
				if err != nil {
					return fmt.Errorf("git remote add %s: %s", name, strings.TrimSpace(string(out)))
				}

				fmt.Printf("✓ git remote %q → %s\n", name, url)
			}

			fmt.Println()
			fmt.Printf("Done. Try: voodu apply -f voodu.hcl -a %s\n", app)
			fmt.Printf("       or: voodu config list -a %s\n", app)

			return nil
		},
	}

	cmd.Flags().StringVar(&identity, "identity", "", "SSH private key (-i)")
	cmd.Flags().StringVar(&binary, "binary", "", "upload this voodu binary to the server before running setup")
	cmd.Flags().StringVar(&installPath, "install-path", "/usr/local/bin/voodu", "where to place --binary on the server")
	cmd.Flags().BoolVar(&skipSetup, "skip-setup", false, "do not run 'voodu setup' on the remote")
	cmd.Flags().BoolVar(&skipApp, "skip-app-create", false, "do not run 'voodu apps create APP' on the remote")

	return cmd
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

// remoteRun ssh-execs a voodu subcommand directly — uses the same shell
// quoting the forward path uses.
func remoteRun(host, identity string, parts ...string) error {
	info := &remote.Info{Host: host}

	code, err := remote.Forward(info, parts[1:], remote.ForwardOptions{
		RemoteBinary: parts[0],
		Identity:     identity,
	})
	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("%s exited %d", strings.Join(parts, " "), code)
	}

	return nil
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

// remoteAppExists checks whether APP is already in `voodu apps list`.
// Used before `apps create` because the subcommand errors on conflict
// and we want setup to be idempotent.
func remoteAppExists(host, identity, app string) (bool, error) {
	args := []string{}

	if identity != "" {
		args = append(args, "-i", identity)
	}

	args = append(args, host, "voodu apps list")

	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err != nil {
		// A non-zero from `apps list` likely means voodu isn't installed
		// yet; surface the raw output so the user can see why.
		return false, fmt.Errorf("remote voodu apps list: %s", strings.TrimSpace(string(out)))
	}

	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == app {
			return true, nil
		}
	}

	return false, nil
}

// randToken generates a short unique-ish suffix so parallel setups from
// different machines don't trample each other's /tmp uploads. Uses the
// process PID + nanosecond clock — not cryptographic, doesn't need to be.
func randToken() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}
