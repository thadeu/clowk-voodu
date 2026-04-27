package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bytes"
)

func TestShellEscape(t *testing.T) {
	cases := map[string]string{
		"":              "''",
		"simple":        "simple",
		"with-dash_x.y": "with-dash_x.y",
		"has space":     "'has space'",
		"has'quote":     `'has'\''quote'`,
		"$(danger)":     "'$(danger)'",
		"/path/to/f":    "/path/to/f",
		"FOO=bar":       "FOO=bar",
		"user@host":     "user@host",
	}

	for in, want := range cases {
		if got := shellEscape(in); got != want {
			t.Errorf("shellEscape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildRemoteCommand(t *testing.T) {
	got := buildRemoteCommand("voodu", []string{"config", "set", "FOO=bar baz", "-a", "api"}, nil)
	want := "voodu config set 'FOO=bar baz' -a api"

	if got != want {
		t.Errorf("buildRemoteCommand:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestBuildRemoteCommandWithEnv checks that env pairs are sorted,
// shell-escaped as whole KEY=VAL tokens, and prepended before the
// binary so the remote shell interprets them as per-command env —
// our workaround for sshd's AcceptEnv being unreliable in the wild.
// Note that shellEscape operates on the full "KEY=VAL" string, so an
// empty value (NO_COLOR=) stays unquoted (shell-safe chars only),
// while "WEIRD=a b" gets single-quoted as one unit because of the
// space. Both forms are valid shell assignments.
func TestBuildRemoteCommandWithEnv(t *testing.T) {
	env := map[string]string{
		"FORCE_COLOR": "1",
		"NO_COLOR":    "",
		"WEIRD":       "a b",
	}

	got := buildRemoteCommand("voodu", []string{"diff"}, env)
	want := "FORCE_COLOR=1 NO_COLOR= 'WEIRD=a b' voodu diff"

	if got != want {
		t.Errorf("buildRemoteCommand with env:\n  got:  %s\n  want: %s", got, want)
	}
}

// TestForwardInvokesSSH substitutes a stub ssh script and asserts the
// exact argv we'd have shipped — the full chain from Info + args to
// shell command, without needing a real server.
func TestForwardInvokesSSH(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "args.txt")

	stub := filepath.Join(tmp, "ssh-stub")

	script := "#!/bin/bash\nprintf '%s\\n' \"$@\" > " + out + "\nexit 42\n"
	if err := os.WriteFile(stub, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	info := &Info{Host: "ubuntu@example.com"}

	force := false

	code, err := Forward(info, []string{"config", "set", "K=V", "-a", "api"}, ForwardOptions{
		SSHBin:   stub,
		ForceTTY: &force,
	})
	if err != nil {
		t.Fatal(err)
	}

	if code != 42 {
		t.Errorf("exit code: got %d, want 42", code)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")

	// argv: [-o LogLevel=QUIET <host> <remote-cmd>] — the host and
	// remote command are always the last two entries; LogLevel and any
	// future ssh-side flags are prepended.
	if len(lines) < 2 {
		t.Fatalf("expected at least host + command, got %v", lines)
	}

	if host := lines[len(lines)-2]; host != "ubuntu@example.com" {
		t.Errorf("host arg: %q", host)
	}

	if remoteCmd := lines[len(lines)-1]; remoteCmd != "voodu config set K=V -a api" {
		t.Errorf("remote cmd: %q", remoteCmd)
	}
}

// TestForwardStreamsStdin asserts two things about the stdin path:
// (1) the bytes we hand to ForwardOptions.Stdin make it to the remote
// process verbatim; (2) -tt is suppressed so the stream isn't mangled
// by terminal line discipline.
func TestForwardStreamsStdin(t *testing.T) {
	tmp := t.TempDir()
	argsOut := filepath.Join(tmp, "args.txt")
	stdinOut := filepath.Join(tmp, "stdin.bin")

	stub := filepath.Join(tmp, "ssh-stub")
	script := "#!/bin/bash\nprintf '%s\\n' \"$@\" > " + argsOut + "\ncat > " + stdinOut + "\n"

	if err := os.WriteFile(stub, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`[{"kind":"deployment","name":"api"}]`)

	force := true

	info := &Info{Host: "u@h"}

	_, err := Forward(info, []string{"apply", "-f", "-", "--format", "json"}, ForwardOptions{
		SSHBin:   stub,
		ForceTTY: &force, // must be ignored when Stdin is set
		Stdin:    bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(stdinOut)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("stdin payload mismatch:\n  got:  %q\n  want: %q", got, payload)
	}

	argsRaw, _ := os.ReadFile(argsOut)

	if strings.Contains(string(argsRaw), "-tt") {
		t.Errorf("-tt should be suppressed when streaming stdin; argv was:\n%s", argsRaw)
	}
}

// TestForwardCapturesStdout verifies the Stdout override on
// ForwardOptions. Used by the apply orchestrator: phase 1's `diff -o
// json` must land in a local buffer so the client can parse it,
// rather than streaming to the user's terminal. Stderr always
// passes through regardless.
func TestForwardCapturesStdout(t *testing.T) {
	tmp := t.TempDir()

	stub := filepath.Join(tmp, "ssh-stub")
	// The stub emits a payload on stdout; the test captures it.
	script := "#!/bin/bash\nprintf 'HELLO-STDOUT'\n"

	if err := os.WriteFile(stub, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	info := &Info{Host: "u@h"}

	var buf bytes.Buffer

	force := false

	_, err := Forward(info, []string{"diff", "-o", "json"}, ForwardOptions{
		SSHBin:   stub,
		ForceTTY: &force,
		Stdout:   &buf,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := buf.String(); got != "HELLO-STDOUT" {
		t.Errorf("captured stdout: got %q, want %q", got, "HELLO-STDOUT")
	}
}

// TestForwardCapturesStderr is the symmetric counterpart to
// TestForwardCapturesStdout. It matters because docker buildx (the
// main consumer of the progress filter on the client) writes its
// `#N` stream to stderr, not stdout. Without this override the
// filter would only see half the build output.
func TestForwardCapturesStderr(t *testing.T) {
	tmp := t.TempDir()

	stub := filepath.Join(tmp, "ssh-stub")
	// The stub emits on stderr only; stdout stays empty.
	script := "#!/bin/bash\nprintf 'HELLO-STDERR' >&2\n"

	if err := os.WriteFile(stub, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	info := &Info{Host: "u@h"}

	var buf bytes.Buffer

	force := false

	_, err := Forward(info, []string{"receive-pack", "app"}, ForwardOptions{
		SSHBin:   stub,
		ForceTTY: &force,
		Stderr:   &buf,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := buf.String(); got != "HELLO-STDERR" {
		t.Errorf("captured stderr: got %q, want %q", got, "HELLO-STDERR")
	}
}

func TestForwardPassesIdentityAndTTY(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "args.txt")

	stub := filepath.Join(tmp, "ssh-stub")

	script := "#!/bin/bash\nprintf '%s\\n' \"$@\" > " + out + "\n"
	if err := os.WriteFile(stub, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	info := &Info{Host: "u@h"}

	force := true

	_, err := Forward(info, []string{"logs", "-f"}, ForwardOptions{
		SSHBin:   stub,
		Identity: "/tmp/key.pem",
		ForceTTY: &force,
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(out)

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")

	joined := strings.Join(lines, " ")

	for _, want := range []string{"-i", "/tmp/key.pem", "-tt", "u@h", "voodu logs -f"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in argv: %v", want, lines)
		}
	}
}

// TestForwardSetsLogLevelQuiet locks in the OpenSSH -o LogLevel=QUIET
// flag we use to suppress the client's "Connection to <host> closed."
// banner. Without this every interactive forward (logs, describe, get
// pods) would tail an extra noise line that has nothing to do with
// voodu's output.
func TestForwardSetsLogLevelQuiet(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "args.txt")

	stub := filepath.Join(tmp, "ssh-stub")

	script := "#!/bin/bash\nprintf '%s\\n' \"$@\" > " + out + "\n"
	if err := os.WriteFile(stub, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	info := &Info{Host: "u@h"}

	force := false

	_, err := Forward(info, []string{"version"}, ForwardOptions{
		SSHBin:   stub,
		ForceTTY: &force,
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(out)

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	joined := strings.Join(lines, " ")

	for _, want := range []string{"-o", "LogLevel=QUIET"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in argv: %v", want, lines)
		}
	}
}
