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
	got := buildRemoteCommand("voodu", []string{"config", "set", "FOO=bar baz", "-a", "api"})
	want := "voodu config set 'FOO=bar baz' -a api"

	if got != want {
		t.Errorf("buildRemoteCommand:\n  got:  %s\n  want: %s", got, want)
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

	info := &Info{Host: "ubuntu@example.com", App: "api"}

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
	if len(lines) != 2 {
		t.Fatalf("expected host + command, got %v", lines)
	}

	if lines[0] != "ubuntu@example.com" {
		t.Errorf("host arg: %q", lines[0])
	}

	if lines[1] != "voodu config set K=V -a api" {
		t.Errorf("remote cmd: %q", lines[1])
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

	info := &Info{Host: "u@h", App: "api"}

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

func TestForwardPassesIdentityAndTTY(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "args.txt")

	stub := filepath.Join(tmp, "ssh-stub")

	script := "#!/bin/bash\nprintf '%s\\n' \"$@\" > " + out + "\n"
	if err := os.WriteFile(stub, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	info := &Info{Host: "u@h", App: "api"}

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
