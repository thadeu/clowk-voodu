package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunPlainTextPlugin(t *testing.T) {
	p := newTestPlugin(t, map[string]string{
		"greet": "#!/bin/bash\necho \"hello $1\"\n",
	})

	res, err := p.Run(context.Background(), RunOptions{
		Command: "greet",
		Args:    []string{"world"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if res.ExitCode != 0 {
		t.Fatalf("exit %d, stderr=%s", res.ExitCode, res.Stderr)
	}

	if strings.TrimSpace(string(res.Raw)) != "hello world" {
		t.Errorf("stdout: got %q", res.Raw)
	}

	if res.Envelope != nil {
		t.Errorf("plain text should not produce envelope: %+v", res.Envelope)
	}
}

func TestRunJSONEnvelope(t *testing.T) {
	p := newTestPlugin(t, map[string]string{
		"ok": "#!/bin/bash\necho '{\"status\":\"ok\",\"data\":{\"port\":5432}}'\n",
	})

	res, err := p.Run(context.Background(), RunOptions{Command: "ok"})
	if err != nil {
		t.Fatal(err)
	}

	if res.Envelope == nil {
		t.Fatalf("expected envelope, got raw %q", res.Raw)
	}

	if res.Envelope.Status != "ok" {
		t.Errorf("status: %q", res.Envelope.Status)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	p := newTestPlugin(t, map[string]string{
		"fail": "#!/bin/bash\necho 'bye' >&2\nexit 3\n",
	})

	res, err := p.Run(context.Background(), RunOptions{Command: "fail"})
	if err != nil {
		t.Fatalf("unexpected exec error: %v", err)
	}

	if res.ExitCode != 3 {
		t.Errorf("exit: got %d", res.ExitCode)
	}

	if !strings.Contains(string(res.Stderr), "bye") {
		t.Errorf("stderr: %q", res.Stderr)
	}
}

func TestRunInjectsEnv(t *testing.T) {
	p := newTestPlugin(t, map[string]string{
		"envcheck": "#!/bin/bash\necho \"$VOODU_PLUGIN_DIR $VOODU_NODE $CUSTOM\"\n",
	})

	p.Manifest.Env = map[string]string{"CUSTOM": "from-manifest"}

	res, err := p.Run(context.Background(), RunOptions{
		Command: "envcheck",
		Env:     map[string]string{"VOODU_NODE": "voodu-0"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := strings.TrimSpace(string(res.Raw))
	if !strings.Contains(got, p.Dir) || !strings.Contains(got, "voodu-0") || !strings.Contains(got, "from-manifest") {
		t.Errorf("env not propagated: %q", got)
	}
}

func TestRunTimeout(t *testing.T) {
	p := newTestPlugin(t, map[string]string{
		"slow": "#!/bin/bash\nsleep 10\n",
	})

	_, err := p.Run(context.Background(), RunOptions{
		Command: "slow",
		Timeout: 100 * time.Millisecond,
	})

	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestRunUnknownCommand(t *testing.T) {
	p := newTestPlugin(t, map[string]string{"known": "#!/bin/bash\n"})

	_, err := p.Run(context.Background(), RunOptions{Command: "unknown"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("want not-found, got %v", err)
	}
}

// TestRunEntrypointPrependsCommandName pins the entrypoint
// dispatch contract: when plugin.yml declares a single binary,
// every command invocation reaches the binary with the command
// name as argv[1]. Plugin's internal switch / cobra router uses
// argv[1] to dispatch, so the prepend is mandatory.
func TestRunEntrypointPrependsCommandName(t *testing.T) {
	dir := t.TempDir()

	// Plugin emits "$@" verbatim so the test can read the args
	// it received from the controller.
	if err := os.WriteFile(filepath.Join(dir, "plugin.yml"), []byte(`
name: demo
entrypoint: bin/demo
commands:
  - name: foo
  - name: bar:baz
`), 0644); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(binDir, "demo"),
		[]byte("#!/bin/bash\necho \"received: $*\"\n"), 0755); err != nil {
		t.Fatal(err)
	}

	p, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	t.Run("simple command", func(t *testing.T) {
		res, err := p.Run(context.Background(), RunOptions{
			Command: "foo",
			Args:    []string{"alpha", "beta"},
		})
		if err != nil {
			t.Fatal(err)
		}

		got := strings.TrimSpace(string(res.Raw))
		want := "received: foo alpha beta"

		if got != want {
			t.Errorf("\n  got:  %q\n  want: %q", got, want)
		}
	})

	t.Run("colon command", func(t *testing.T) {
		// Multi-segment command (heroku-style nested) reaches
		// the binary as a single argv element. Plugin's router
		// receives `bar:baz` as argv[1].
		res, err := p.Run(context.Background(), RunOptions{
			Command: "bar:baz",
			Args:    []string{"ref", "--flag"},
		})
		if err != nil {
			t.Fatal(err)
		}

		got := strings.TrimSpace(string(res.Raw))
		want := "received: bar:baz ref --flag"

		if got != want {
			t.Errorf("\n  got:  %q\n  want: %q", got, want)
		}
	})
}

// TestRunLegacyDoesNotPrependCommandName confirms back-compat:
// per-command bin/<cmd> shims still get the args verbatim, with
// no command-name prepend. The shim file IS the dispatch already
// — adding a prepend would break every existing plugin that uses
// the old shape (voodu-redis, voodu-caddy, etc.).
func TestRunLegacyDoesNotPrependCommandName(t *testing.T) {
	p := newTestPlugin(t, map[string]string{
		"echo-args": "#!/bin/bash\necho \"received: $*\"\n",
	})

	res, err := p.Run(context.Background(), RunOptions{
		Command: "echo-args",
		Args:    []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := strings.TrimSpace(string(res.Raw))
	want := "received: alpha beta"

	if got != want {
		t.Errorf("legacy mode must NOT prepend command name:\n  got:  %q\n  want: %q",
			got, want)
	}
}

func newTestPlugin(t *testing.T, commands map[string]string) *LoadedPlugin {
	t.Helper()

	dir := t.TempDir()

	cmds := filepath.Join(dir, "commands")
	if err := os.Mkdir(cmds, 0755); err != nil {
		t.Fatal(err)
	}

	for name, body := range commands {
		path := filepath.Join(cmds, name)

		if err := os.WriteFile(path, []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}

	p, err := LoadFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	p.Manifest.Name = "test"

	return p
}
