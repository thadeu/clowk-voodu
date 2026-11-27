package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// stubSSH points sshBinary at a script that writes its argv, one per line,
// to the returned file and prints 4242 (a fake PID for nohup).
func stubSSH(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	out := filepath.Join(dir, "argv.txt")
	stub := filepath.Join(dir, "ssh")

	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + out + "\necho 4242\n"

	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	prev := sshBinary
	sshBinary = stub

	t.Cleanup(func() { sshBinary = prev })

	return out
}

// inRepoWithRemote runs the test inside a git repo that has a voodu remote
// named sp, the way an operator's project has one.
func inRepoWithRemote(t *testing.T, url string) {
	t.Helper()

	dir := t.TempDir()

	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "sp", url}} {
		c := exec.Command("git", args...)
		c.Dir = dir

		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}

	t.Chdir(dir)
	t.Setenv("VOODU_SSH_IDENTITY", "")
}

func readArgv(t *testing.T, path string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)

	if err != nil {
		t.Fatalf("ssh was not called: %v", err)
	}

	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func runVD(t *testing.T, args ...string) error {
	t.Helper()

	argv := rewriteColonSyntax(append([]string{"vd"}, args...))

	return dispatch(newRootCmd(), argv[1:])
}

func TestSSHConnectUsesRemoteHostAndKey(t *testing.T) {
	inRepoWithRemote(t, "ubuntu@10.0.0.5:/keys/sp.pem")

	argvFile := stubSSH(t)

	if err := runVD(t, "ssh", "connect", "-r", "sp"); err != nil {
		t.Fatal(err)
	}

	got := readArgv(t, argvFile)
	want := []string{"-o", "LogLevel=ERROR", "-i", "/keys/sp.pem", "ubuntu@10.0.0.5"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestSSHBareIsConnect(t *testing.T) {
	inRepoWithRemote(t, "ubuntu@10.0.0.5")

	argvFile := stubSSH(t)

	if err := runVD(t, "ssh", "-r", "sp"); err != nil {
		t.Fatal(err)
	}

	got := readArgv(t, argvFile)

	if got[len(got)-1] != "ubuntu@10.0.0.5" {
		t.Errorf("bare vd ssh should open a shell on the host, argv: %q", got)
	}
}

func TestSSHIdentityEnvWinsOverRemoteKey(t *testing.T) {
	inRepoWithRemote(t, "ubuntu@10.0.0.5:/keys/sp.pem")
	t.Setenv("VOODU_SSH_IDENTITY", "/keys/override.pem")

	argvFile := stubSSH(t)

	if err := runVD(t, "ssh", "connect", "-r", "sp"); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(readArgv(t, argvFile), " "); !strings.Contains(got, "-i /keys/override.pem") {
		t.Errorf("VOODU_SSH_IDENTITY should win: %s", got)
	}
}

func TestSSHUnknownRemoteFails(t *testing.T) {
	inRepoWithRemote(t, "ubuntu@10.0.0.5")

	argvFile := stubSSH(t)

	if err := runVD(t, "ssh", "connect", "-r", "nope"); err == nil {
		t.Fatal("want an error for a remote that does not exist")
	}

	if _, err := os.Stat(argvFile); err == nil {
		t.Error("ssh must not run when the remote does not resolve")
	}
}

// Words after the command go to the host, flags too, and a colon in them is
// not the vd colon syntax.
func TestSSHRunSendsCommandVerbatim(t *testing.T) {
	inRepoWithRemote(t, "ubuntu@10.0.0.5")

	argvFile := stubSSH(t)

	if err := runVD(t, "ssh", "run", "-r", "sp", "rails", "db:migrate", "-v", "my dir"); err != nil {
		t.Fatal(err)
	}

	got := readArgv(t, argvFile)
	want := []string{"-o", "LogLevel=ERROR", "-T", "ubuntu@10.0.0.5", "rails db:migrate -v 'my dir'"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestSSHRunWithDoubleDash(t *testing.T) {
	inRepoWithRemote(t, "ubuntu@10.0.0.5")

	argvFile := stubSSH(t)

	if err := runVD(t, "ssh:run", "-r", "sp", "--", "docker", "ps", "-a"); err != nil {
		t.Fatal(err)
	}

	got := readArgv(t, argvFile)

	if line := got[len(got)-1]; line != "docker ps -a" {
		t.Errorf("remote line = %q, want %q", line, "docker ps -a")
	}
}

func TestSSHNohupStartsInBackground(t *testing.T) {
	inRepoWithRemote(t, "ubuntu@10.0.0.5")

	argvFile := stubSSH(t)

	if err := runVD(t, "ssh", "nohup", "-r", "sp", "--log", "/tmp/job.log", "--", "./backup.sh", "--full"); err != nil {
		t.Fatal(err)
	}

	got := readArgv(t, argvFile)
	want := "nohup sh -c './backup.sh --full' > /tmp/job.log 2>&1 < /dev/null & echo $!"

	if line := got[len(got)-1]; line != want {
		t.Errorf("remote line:\n  got:  %s\n  want: %s", line, want)
	}

	if !reflect.DeepEqual(got[:3], []string{"-o", "LogLevel=ERROR", "-T"}) {
		t.Errorf("nohup must not ask for a TTY: %q", got)
	}
}

func TestRemoteCommandLine(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"one argument is a shell line", []string{"df -h | grep /dev"}, "df -h | grep /dev"},
		{"several arguments are an argv", []string{"ls", "-la", "my dir"}, "ls -la 'my dir'"},
		{"quotes in a word stay in it", []string{"echo", "it's"}, `echo 'it'\''s'`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := remoteCommandLine(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// The nohup line must return at once with the PID, and the command must
// continue to run after the shell that started it is gone.
func TestNohupLineDetachesAndLogs(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "out.log")
	line := nohupLine("sleep 0.5; echo done 'with quotes'", logPath)

	start := time.Now()
	out, err := exec.Command("sh", "-c", line).Output()

	if err != nil {
		t.Fatalf("sh: %v", err)
	}

	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("nohup line waited %s for the command; it must return at once", elapsed)
	}

	if pid := strings.TrimSpace(string(out)); pid == "" {
		t.Error("nohup line must print the PID")
	}

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(logPath)

		if strings.Contains(string(raw), "done with quotes") {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Error("the background command did not write its output to the log")
}

func TestDefaultNohupLog(t *testing.T) {
	at := time.Date(2026, 11, 27, 14, 53, 0, 0, time.UTC)

	if got := defaultNohupLog(at); got != "/tmp/vd-nohup-20261127-145300.log" {
		t.Errorf("got %q", got)
	}
}

func TestRewriteColonSyntaxLeavesSSHCommandAlone(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{
			[]string{"vd", "ssh", "run", "-r", "sp", "rails", "db:migrate"},
			[]string{"vd", "ssh", "run", "-r", "sp", "rails", "db:migrate"},
		},
		{
			[]string{"vd", "ssh:nohup", "bin/rails", "jobs:work"},
			[]string{"vd", "ssh", "nohup", "bin/rails", "jobs:work"},
		},
		{
			[]string{"vd", "config:set", "app/web", "FOO=bar"},
			[]string{"vd", "config", "app/web", "set", "FOO=bar"},
		},
	}

	for _, c := range cases {
		if got := rewriteColonSyntax(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("rewriteColonSyntax(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
