// cmd_ssh.go is `vd ssh`: go to the box behind a remote without looking up
// its user, host and key first.
//
//	vd ssh connect -r prod                    # interactive shell
//	vd ssh run -r prod -- docker ps           # one command, then exit
//	vd ssh nohup -r prod -- ./backup.sh       # start in background, return at once
//
// The target comes from the same place every forwarded command uses: -r NAME,
// else the "voodu" git remote. The key comes from VOODU_SSH_IDENTITY, else the
// :identity suffix of the remote URL, else ssh's own defaults.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"go.voodu.clowk.in/internal/remote"
)

// sshBinary is the ssh client the commands run. Tests point it at a stub.
var sshBinary = "ssh"

// sshTarget is where a `vd ssh` command goes.
type sshTarget struct {
	host     string
	identity string
}

func newSSHCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "Open a shell or run a command on the host of a remote",
		Long: `ssh goes to the host behind a voodu remote, with the user, host and
key that the remote already has. No need to look them up in
'vd remote list' first.

  vd ssh connect -r prod                 # interactive shell
  vd ssh run -r prod -- docker ps        # run one command
  vd ssh run -r prod 'df -h | grep /dev' # one quoted argument = a shell line
  vd ssh nohup -r prod -- ./backup.sh    # start it in background and return

Without -r, the 'voodu' remote is used. Bare 'vd ssh' is 'vd ssh connect'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSSHConnect(cmd)
		},
	}

	cmd.AddCommand(newSSHConnectCmd(), newSSHRunCmd(), newSSHNohupCmd())

	return cmd
}

func newSSHConnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connect",
		Short: "Open an interactive shell on the host of a remote",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSSHConnect(cmd)
		},
	}
}

func newSSHRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run [flags] [--] COMMAND [ARGS...]",
		Short: "Run one command on the host of a remote",
		Long: `run executes one command on the host and returns its exit code.

Two forms:

  vd ssh run -r prod -- ls -la "/opt/voodu/my dir"   # argv: each word is quoted
  vd ssh run -r prod 'docker ps | grep web'          # one argument: a shell line

Flags of vd go before the command. All words after the command go to it.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveSSHTarget(cmd)

			if err != nil {
				return err
			}

			argv := sshRunArgs(target, remoteCommandLine(args), wantSSHTTY())

			return exitWith(runSSH(argv, true))
		},
	}

	cmd.Flags().SetInterspersed(false)

	return cmd
}

func newSSHNohupCmd() *cobra.Command {
	var logPath string

	cmd := &cobra.Command{
		Use:   "nohup [flags] [--] COMMAND [ARGS...]",
		Short: "Start a command in background on the host of a remote",
		Long: `nohup starts the command on the host with nohup and returns at once.
The command continues to run after the SSH session closes.

Its output goes to a log file on the host (--log, default
/tmp/vd-nohup-<time>.log). vd prints the PID and the log path:

  vd ssh nohup -r prod -- ./backup.sh
  vd ssh run -r prod -- tail -f /tmp/vd-nohup-20261127-145300.log`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveSSHTarget(cmd)

			if err != nil {
				return err
			}

			if logPath == "" {
				logPath = defaultNohupLog(time.Now())
			}

			line := nohupLine(remoteCommandLine(args), logPath)
			argv := sshRunArgs(target, line, false)

			out, code, err := captureSSH(argv)

			if err != nil {
				return err
			}

			if code != 0 {
				os.Exit(code)
			}

			fmt.Printf("%s started on %s (pid %s)\n", checkFlow(), target.host, strings.TrimSpace(out))
			fmt.Printf("  log:    %s\n", logPath)
			fmt.Printf("  follow: vd ssh run%s -- tail -f %s\n", remoteFlagHint(cmd), logPath)

			return nil
		},
	}

	cmd.Flags().StringVar(&logPath, "log", "", "log file on the host for the command output (default /tmp/vd-nohup-<time>.log)")
	cmd.Flags().SetInterspersed(false)

	return cmd
}

func runSSHConnect(cmd *cobra.Command) error {
	target, err := resolveSSHTarget(cmd)

	if err != nil {
		return err
	}

	return exitWith(runSSH(sshConnectArgs(target), true))
}

// resolveSSHTarget picks the remote (-r, else the "voodu" remote) and the key,
// with the same precedence as the forwarded commands.
func resolveSSHTarget(cmd *cobra.Command) (sshTarget, error) {
	name, _ := cmd.Flags().GetString("remote")

	info, err := remote.Resolve(name)

	if err != nil {
		return sshTarget{}, err
	}

	if info == nil {
		return sshTarget{}, fmt.Errorf("no remote: pass -r NAME, or add one with 'vd remote add voodu user@host'")
	}

	identity := os.Getenv("VOODU_SSH_IDENTITY")

	if identity == "" {
		identity = info.Identity
	}

	return sshTarget{host: info.Host, identity: identity}, nil
}

// sshBaseArgs are the options every `vd ssh` call shares.
//
// LogLevel=ERROR removes the "Connection to X closed." line after a TTY
// session, and keeps auth and host-key errors.
func sshBaseArgs(t sshTarget) []string {
	args := []string{"-o", "LogLevel=ERROR"}

	if sshBinary == "ssh" {
		args = append(args, remote.ControlMasterArgs()...)
	}

	if t.identity != "" {
		args = append(args, "-i", t.identity)
	}

	return args
}

func sshConnectArgs(t sshTarget) []string {
	return append(sshBaseArgs(t), t.host)
}

// sshRunArgs runs line on the host. tty asks for a terminal, so that sudo
// prompts, colors and full-screen tools (htop) work. -T keeps a terminal
// out of the way otherwise.
func sshRunArgs(t sshTarget, line string, tty bool) []string {
	args := sshBaseArgs(t)

	if tty {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
	}

	return append(args, t.host, line)
}

// remoteCommandLine turns the words after `run`/`nohup` into the line the
// remote shell executes.
//
// One argument is a shell line, sent as it is, so pipes and redirects work:
// vd ssh run 'df -h | grep /dev'. Several arguments are an argv: each word is
// quoted, so a word with a space stays one word on the host. Plain ssh joins
// the words with spaces and loses that.
func remoteCommandLine(args []string) string {
	if len(args) == 1 {
		return args[0]
	}

	parts := make([]string, len(args))

	for i, a := range args {
		parts[i] = remote.ShellEscape(a)
	}

	return strings.Join(parts, " ")
}

// nohupLine starts line in background and prints its PID.
//
// All three standard streams are redirected: an open stdin or stdout keeps
// the SSH channel open, and ssh would wait for the command to end.
func nohupLine(line, logPath string) string {
	return fmt.Sprintf("nohup sh -c %s > %s 2>&1 < /dev/null & echo $!",
		remote.ShellEscape(line),
		remote.ShellEscape(logPath),
	)
}

func defaultNohupLog(now time.Time) string {
	return "/tmp/vd-nohup-" + now.Format("20060102-150405") + ".log"
}

// remoteFlagHint repeats -r in the follow-up hint, so the operator can paste it.
func remoteFlagHint(cmd *cobra.Command) string {
	name, _ := cmd.Flags().GetString("remote")

	if name == "" {
		return ""
	}

	return " -r " + name
}

func wantSSHTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// runSSH runs ssh with the terminal attached and returns its exit code.
func runSSH(argv []string, attachStdin bool) (int, error) {
	c := exec.Command(sshBinary, argv...)

	if attachStdin {
		c.Stdin = os.Stdin
	}

	c.Stdout = os.Stdout
	c.Stderr = os.Stderr

	return exitCodeOf(c.Run())
}

// captureSSH runs ssh and returns its stdout. stderr stays on the terminal.
func captureSSH(argv []string) (string, int, error) {
	c := exec.Command(sshBinary, argv...)
	c.Stderr = os.Stderr

	out, err := c.Output()
	code, err := exitCodeOf(err)

	return string(out), code, err
}

func exitCodeOf(err error) (int, error) {
	if err == nil {
		return 0, nil
	}

	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), nil
	}

	return 1, fmt.Errorf("ssh: %w", err)
}

// exitWith gives the remote exit code to the shell of the operator. ssh
// already printed the reason, so vd adds nothing.
func exitWith(code int, err error) error {
	if err != nil {
		return err
	}

	if code != 0 {
		os.Exit(code)
	}

	return nil
}
