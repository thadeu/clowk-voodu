// Package procfile turns a Heroku/Dokku-style Procfile into voodu
// manifests, the migration on-ramp for operators who already have a
// Procfile and don't want to hand-author HCL before their first deploy.
//
// A Procfile is a set of `type: command` lines:
//
//	web:     env RUBYOPT="-W0" bundle exec puma -p $PORT
//	worker:  bundle exec sidekiq -C config/sidekiq.yml
//	release: bundle exec rails db:migrate
//
// Mapping contract (see the plan / package tests for the rationale):
//
//   - Every line becomes its own resource at identity (scope, type),
//     so a later same-(scope,name) HCL apply overlays cleanly.
//   - The HTTP process has NO reliable name (`web`, `app`, …), so we
//     do NOT special-case any process for routing or ports. Instead
//     every deployment gets a PORT env + published port from a base
//     range, covering both `puma -p $PORT` (command reads $PORT) and
//     `puma -C config/puma.rb` (config file reads ENV['PORT']).
//   - The ONE recognized convention is `release:` — Heroku's universal
//     one-shot release phase. It maps to a voodu `job` (runs once,
//     exits), not a deployment.
//   - Commands are shell-wrapped (`/bin/sh -c`) because production
//     Procfiles lean on `VAR=val` prefixes, `$((arithmetic))`, `$PORT`,
//     and pipes — none of which expand under exec-form argv.
//   - Build is terse: no image, no build block → voodu auto-detects the
//     language from the source tree and builds the runtime image.
//   - App-wide config vars (Heroku `config:set`) use voodu's native
//     scope-level config: `vd config <scope> set KEY=val` merges into
//     EVERY resource in the scope automatically (the reconciler builds
//     each deployment's env via Store.ResolveConfig = scope-level +
//     app-level). No env_from wiring — that would just duplicate the
//     scope merge AND hard-fail the first deploy if the referenced
//     bucket doesn't exist yet.
//
// Host/TLS is deliberately NOT modelled here — runtime Procfile mode
// never creates an ingress. Operators who want a domain use `vd apply
// -f Procfile --eject` to scaffold HCL (ToHCL) and add an ingress block.
package procfile

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/manifest"
)

const (
	// BasePort is the first container port handed to the first
	// deployment; subsequent deployments increment from here. The value
	// is injected as both the PORT env var and the published container
	// port so an app reads it from `$PORT` or `ENV['PORT']` identically.
	BasePort = 5000

	// ReleaseType is the one process name with a recognized convention:
	// Heroku's one-shot release phase. A line with this type becomes a
	// voodu `job` rather than a long-running deployment.
	ReleaseType = "release"

	// RestartPolicy is set on every generated deployment. voodu's HCL
	// default is "unless-stopped", which would restart a clean exit-0 in
	// a loop; "on-failure" lets a crash recover while a deliberate exit-0
	// (e.g. a misplaced one-shot command) sits in the exited state until
	// the operator acts.
	RestartPolicy = "on-failure"
)

// procTypePattern constrains a process type to something usable as a
// voodu resource name: starts with a letter, then letters/digits/`-`/`_`.
var procTypePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// Process is one parsed Procfile line.
type Process struct {
	Type       string // e.g. "web", "app", "worker", "release"
	RawCommand string // verbatim command, shell features intact
}

// IsRelease reports whether this line uses the one-shot release
// convention (case-insensitive on the type).
func (p Process) IsRelease() bool {
	return strings.EqualFold(p.Type, ReleaseType)
}

// Options carries the caller-resolved knobs for the transform.
type Options struct {
	// Scope is the resource scope every generated manifest lands under.
	// The CLI resolves this from --app or a random short id; the
	// transform itself never invents it.
	Scope string

	// Ingress is the per-process ingress declared in .voodu/app.json,
	// keyed by process name. Each entry emits an `ingress` manifest
	// alongside that process's deployment. nil = no routing (the
	// Procfile default).
	Ingress map[string]AppIngress
}

// Parse reads a Procfile and returns its process lines in declaration
// order. Blank lines and `#` comment lines are ignored. Duplicate
// process types are rejected (they'd collide on resource identity).
func Parse(r io.Reader) ([]Process, error) {
	var (
		out  []Process
		seen = map[string]bool{}
		ln   int
	)

	sc := bufio.NewScanner(r)
	// Procfile lines can be long (a chained command with many flags);
	// bump the token cap well past the 64KiB default.
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		rawType, rawCmd, found := strings.Cut(line, ":")
		if !found {
			return nil, fmt.Errorf("procfile line %d: missing %q separator: %q", ln, ":", line)
		}

		typ := strings.TrimSpace(rawType)
		cmd := strings.TrimSpace(rawCmd)

		if !procTypePattern.MatchString(typ) {
			return nil, fmt.Errorf("procfile line %d: invalid process type %q (want letters/digits/-/_, starting with a letter)", ln, typ)
		}

		if cmd == "" {
			return nil, fmt.Errorf("procfile line %d: process %q has an empty command", ln, typ)
		}

		key := strings.ToLower(typ)
		if seen[key] {
			return nil, fmt.Errorf("procfile line %d: duplicate process type %q", ln, typ)
		}
		seen[key] = true

		out = append(out, Process{Type: typ, RawCommand: cmd})
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read procfile: %w", err)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("procfile is empty")
	}

	return out, nil
}

// ToManifests transforms parsed processes into voodu manifests:
//
//   - release:  → job  "<scope>" "release"   (one-shot)
//   - any other → deployment "<scope>" "<type>" (long-running)
//   - opts.Ingress[proc] → ingress "<scope>" "<proc>" routing to that
//     process's deployment (port auto-filled from its assigned port).
//
// Deployments get a PORT env + published port from the BasePort range
// and restart on-failure. Commands are shell-wrapped. Shared config vars
// come from scope-level config (vd config <scope> set), not env_from.
func ToManifests(procs []Process, opts Options) ([]controller.Manifest, error) {
	if opts.Scope == "" {
		return nil, fmt.Errorf("scope is required")
	}

	out := make([]controller.Manifest, 0, len(procs)+len(opts.Ingress))
	portByProc := make(map[string]int, len(procs))
	nextAuto := BasePort

	for _, p := range procs {
		if p.IsRelease() {
			spec := manifest.JobSpec{
				Command: shellWrap(p.RawCommand),
			}

			m, err := encode(controller.KindJob, opts.Scope, p.Type, spec)
			if err != nil {
				return nil, err
			}

			out = append(out, m)

			continue
		}

		// Port precedence: an ingress that declares a port for THIS
		// process pins it (PORT env + published port + the ingress all
		// agree — "declare it once"). Otherwise auto-assign from the
		// BasePort range. Pinned ports don't consume an auto slot, so
		// `web` on 80 leaves the next auto process on BasePort, not +1.
		port := 0
		if ing, ok := opts.Ingress[p.Type]; ok && ing.Port != 0 {
			port = ing.Port
		} else {
			port = nextAuto
			nextAuto++
		}

		portStr := strconv.Itoa(port)
		portByProc[p.Type] = port

		spec := manifest.DeploymentSpec{
			Command:  shellWrap(p.RawCommand),
			Replicas: 1,
			Env:      map[string]string{"PORT": portStr},
			Ports:    []string{portStr},
			Restart:  RestartPolicy,
		}

		m, err := encode(controller.KindDeployment, opts.Scope, p.Type, spec)
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	// Ingress from app.json, keyed by process. Sorted for deterministic
	// output. Each entry routes to its process's deployment; the port
	// defaults to that deployment's assigned port.
	procNames := make([]string, 0, len(opts.Ingress))
	for proc := range opts.Ingress {
		procNames = append(procNames, proc)
	}
	sort.Strings(procNames)

	for _, proc := range procNames {
		dp, ok := portByProc[proc]
		if !ok {
			return nil, fmt.Errorf("ingress %q: no matching Procfile process (must be a long-running deployment line, not %q/release)", proc, proc)
		}

		spec := opts.Ingress[proc].toSpec(proc, dp)
		if spec.Host == "" {
			return nil, fmt.Errorf("ingress %q: host is required", proc)
		}

		m, err := encode(controller.KindIngress, opts.Scope, proc, spec)
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	return out, nil
}

// shellWrap turns a raw Procfile command into exec-form argv that runs
// it through a POSIX shell, so `VAR=val` prefixes, `$((arithmetic))`,
// `$PORT`, and pipes all expand the way Heroku/foreman run them.
func shellWrap(raw string) []string {
	return []string{"/bin/sh", "-c", raw}
}

// encode marshals a typed spec into the on-the-wire controller.Manifest
// shape (mirrors the unexported helper in internal/manifest).
func encode(kind controller.Kind, scope, name string, spec any) (controller.Manifest, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return controller.Manifest{}, fmt.Errorf("encode %s/%s/%s: %w", kind, scope, name, err)
	}

	return controller.Manifest{
		Kind:  kind,
		Scope: scope,
		Name:  name,
		Spec:  json.RawMessage(b),
	}, nil
}
