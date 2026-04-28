package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.voodu.clowk.in/pkg/plugin"
)

// Result is the outcome of a single plugin invocation. When the plugin
// emitted a JSON envelope, Envelope is populated and Raw holds the
// parsed Data portion; otherwise Raw is the verbatim stdout.
type Result struct {
	ExitCode int
	Raw      []byte           // full stdout (verbatim) — always set
	Stderr   []byte           // captured stderr
	Envelope *plugin.Envelope // non-nil when stdout parsed as an envelope
}

// RunOptions controls a single invocation.
type RunOptions struct {
	Command string
	Args    []string

	// Env is merged on top of inherited os.Environ and the plugin's own
	// env declared in plugin.yml. Callers use this to inject per-invocation
	// context (app name, node name, etcd client url).
	Env map[string]string

	Stdin   []byte
	Timeout time.Duration
}

// DefaultTimeout is used when RunOptions.Timeout is zero. Plugins are
// usually quick (read config, docker inspect, a few HTTP calls), so a
// generous default keeps everything simple without hanging forever.
const DefaultTimeout = 60 * time.Second

// Run executes one command from a loaded plugin and captures its output.
// The executable is resolved from the plugin's Commands map, which
// already encodes the bin/ > commands/ precedence.
func (p *LoadedPlugin) Run(ctx context.Context, opts RunOptions) (*Result, error) {
	path, ok := p.Commands[opts.Command]
	if !ok {
		return nil, fmt.Errorf("plugin %q: command %q not found", p.Manifest.Name, opts.Command)
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, path, opts.Args...)
	cmd.Env = buildEnv(p, opts.Env)
	cmd.Dir = p.Dir

	if opts.Stdin != nil {
		cmd.Stdin = bytes.NewReader(opts.Stdin)
	}

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	res := &Result{
		Raw:    stdout.Bytes(),
		Stderr: stderr.Bytes(),
	}

	if env := parseEnvelope(res.Raw); env != nil {
		res.Envelope = env
	}

	// Distinguish a timeout (context hit its deadline and killed the
	// process, which bubbles up as an ExitError) from a regular non-zero
	// exit. Plugin authors rely on the difference.
	if runCtx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("plugin %q: timeout after %s", p.Manifest.Name, timeout)
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}

		return res, fmt.Errorf("plugin %q: %w", p.Manifest.Name, err)
	}

	return res, nil
}

// buildEnv layers (1) the inherited process environment, (2) plugin.yml
// env block, (3) per-invocation Env. Last write wins so callers can
// override anything the plugin declared.
func buildEnv(p *LoadedPlugin, per map[string]string) []string {
	pairs := os.Environ()

	add := func(k, v string) {
		pairs = append(pairs, k+"="+v)
	}

	add(plugin.EnvPluginDir, p.Dir)

	for k, v := range p.Manifest.Env {
		add(k, v)
	}

	for k, v := range per {
		add(k, v)
	}

	return pairs
}

// parseEnvelope returns a non-nil envelope if stdout looks like JSON
// matching the plugin protocol. The detection is strict: the first
// non-whitespace byte must be '{' AND the blob must unmarshal into an
// Envelope with a non-empty Status. Anything else falls back to raw
// text passthrough so shell-only plugins that print bare strings
// still work.
func parseEnvelope(raw []byte) *plugin.Envelope {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}

	var env plugin.Envelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil
	}

	if env.Status == "" {
		return nil
	}

	return &env
}

// CombinedOutput returns stdout followed by stderr, useful for logging
// or when rendering plain-text plugins.
func (r *Result) CombinedOutput() string {
	var b strings.Builder

	b.Write(r.Raw)

	if len(r.Stderr) > 0 {
		if b.Len() > 0 && !bytes.HasSuffix(r.Raw, []byte("\n")) {
			b.WriteByte('\n')
		}

		b.Write(r.Stderr)
	}

	return b.String()
}
