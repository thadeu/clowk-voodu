package procfile

import (
	"encoding/json"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/manifest"
)

// exampleA / exampleB are the two real production Procfiles from the
// plan. They pin the contract: arbitrary process names, shell features,
// config-var references, and the release one-shot.
const exampleA = `
# a comment
web:     env RUBYOPT="-W0" bundle exec puma -p $PORT
worker:  env RUBYOPT="-W0" bundle exec sidekiq -C config/sidekiq.yml
sync:    bundle exec bin/sync
`

const exampleB = `app:       DB_POOL=$((MAX_THREADS+3)) bundle exec puma -C config/puma.rb
jobs:      DB_POOL=$((SIDEKIQ_CONCURRENCY_NUMBER+3)) bundle exec sidekiq -v -C config/sidekiq.yml
sync:      DB_POOL=$((CIRCUITRY_BATCH_SIZE+1)) bundle exec bin/sync
clockwork: bundle exec clockwork config/clockwork.rb
release:   bundle exec rails db:migrate
`

func TestParse_ExampleA(t *testing.T) {
	procs, err := Parse(strings.NewReader(exampleA))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	want := []Process{
		{Type: "web", RawCommand: `env RUBYOPT="-W0" bundle exec puma -p $PORT`},
		{Type: "worker", RawCommand: `env RUBYOPT="-W0" bundle exec sidekiq -C config/sidekiq.yml`},
		{Type: "sync", RawCommand: `bundle exec bin/sync`},
	}

	if len(procs) != len(want) {
		t.Fatalf("got %d procs, want %d: %+v", len(procs), len(want), procs)
	}

	for i, w := range want {
		if procs[i] != w {
			t.Errorf("proc[%d] = %+v, want %+v", i, procs[i], w)
		}
	}
}

func TestParse_ArbitraryHTTPNameAndComments(t *testing.T) {
	procs, err := Parse(strings.NewReader(exampleB))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// The HTTP process is "app", not "web" — no name convention.
	if procs[0].Type != "app" {
		t.Errorf("first proc type = %q, want app", procs[0].Type)
	}

	// Arithmetic + env prefixes preserved verbatim.
	if !strings.Contains(procs[0].RawCommand, "$((MAX_THREADS+3))") {
		t.Errorf("arithmetic not preserved: %q", procs[0].RawCommand)
	}

	if !procs[4].IsRelease() {
		t.Errorf("expected last proc to be release, got %q", procs[4].Type)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := map[string]string{
		"no colon":       "web bundle exec puma",
		"empty command":  "web:   ",
		"bad type":       "1web: foo",
		"duplicate type": "web: a\nweb: b",
		"empty procfile": "# only a comment\n\n",
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(src)); err == nil {
				t.Errorf("expected error for %q, got nil", src)
			}
		})
	}
}

func TestToManifests_DeploymentMapping(t *testing.T) {
	procs, err := Parse(strings.NewReader(exampleA))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	mans, err := ToManifests(procs, Options{Scope: "demo"})
	if err != nil {
		t.Fatalf("to manifests: %v", err)
	}

	if len(mans) != 3 {
		t.Fatalf("got %d manifests, want 3", len(mans))
	}

	// First deployment: web, port 5000.
	m := mans[0]
	if m.Kind != controller.KindDeployment {
		t.Errorf("kind = %q, want deployment", m.Kind)
	}
	if m.Scope != "demo" || m.Name != "web" {
		t.Errorf("identity = %s/%s, want demo/web", m.Scope, m.Name)
	}

	var spec manifest.DeploymentSpec
	if err := json.Unmarshal(m.Spec, &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}

	wantCmd := []string{"/bin/sh", "-c", `env RUBYOPT="-W0" bundle exec puma -p $PORT`}
	if len(spec.Command) != 3 || spec.Command[0] != "/bin/sh" || spec.Command[2] != wantCmd[2] {
		t.Errorf("command = %v, want %v", spec.Command, wantCmd)
	}
	if spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", spec.Replicas)
	}
	if spec.Env["PORT"] != "5000" {
		t.Errorf("PORT env = %q, want 5000", spec.Env["PORT"])
	}
	if len(spec.Ports) != 1 || spec.Ports[0] != "5000" {
		t.Errorf("ports = %v, want [5000]", spec.Ports)
	}
	// No env_from — shared config comes from voodu's native scope-level
	// config (vd config <scope> set), not a referenced bucket. Wiring
	// env_from to a not-yet-created bucket would hard-fail the deploy.
	if len(spec.EnvFrom) != 0 {
		t.Errorf("env_from = %v, want empty (scope-level config handles shared vars)", spec.EnvFrom)
	}
	if spec.Restart != "on-failure" {
		t.Errorf("restart = %q, want on-failure", spec.Restart)
	}
	if spec.Image != "" || spec.Build != nil {
		t.Errorf("expected terse build (no image, no build block); got image=%q build=%v", spec.Image, spec.Build)
	}

	// Ports increment per deployment.
	var second manifest.DeploymentSpec
	_ = json.Unmarshal(mans[1].Spec, &second)
	if second.Env["PORT"] != "5001" {
		t.Errorf("second deployment PORT = %q, want 5001", second.Env["PORT"])
	}
}

func TestToManifests_ReleaseIsJob(t *testing.T) {
	procs, err := Parse(strings.NewReader(exampleB))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	mans, err := ToManifests(procs, Options{Scope: "demo"})
	if err != nil {
		t.Fatalf("to manifests: %v", err)
	}

	var release *controller.Manifest
	port5003Seen := false

	for i := range mans {
		if mans[i].Name == "release" {
			release = &mans[i]
		}
	}

	if release == nil {
		t.Fatal("release manifest not found")
	}
	if release.Kind != controller.KindJob {
		t.Errorf("release kind = %q, want job", release.Kind)
	}

	var jobSpec manifest.JobSpec
	if err := json.Unmarshal(release.Spec, &jobSpec); err != nil {
		t.Fatalf("decode job spec: %v", err)
	}
	if jobSpec.Command[0] != "/bin/sh" {
		t.Errorf("release command not shell-wrapped: %v", jobSpec.Command)
	}
	if len(jobSpec.EnvFrom) != 0 {
		t.Errorf("release env_from = %v, want empty (scope-level config)", jobSpec.EnvFrom)
	}

	// release must NOT consume a port slot — the 4 deployments get
	// 5000..5003, no gap from release.
	for i := range mans {
		if mans[i].Kind != controller.KindDeployment {
			continue
		}
		var d manifest.DeploymentSpec
		_ = json.Unmarshal(mans[i].Spec, &d)
		if d.Env["PORT"] == "5003" {
			port5003Seen = true
		}
	}
	if !port5003Seen {
		t.Error("expected a deployment on port 5003 (release should not consume a port slot)")
	}
}

func TestToManifests_RequiresScope(t *testing.T) {
	if _, err := ToManifests([]Process{{Type: "web", RawCommand: "x"}}, Options{}); err == nil {
		t.Error("expected error when scope is empty")
	}
}

func TestToHCL_RoundTrips(t *testing.T) {
	procs, err := Parse(strings.NewReader(exampleB))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	hcl, err := ToHCL(procs, Options{Scope: "demo"})
	if err != nil {
		t.Fatalf("to hcl: %v", err)
	}

	for _, want := range []string{
		`deployment "demo" "app"`,
		`job "demo" "release"`,
		`restart  = "on-failure"`,
		`vd config demo set KEY=value`, // scope-level config hint
		`# ingress "demo" "app"`,       // stub targets first HTTP proc
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("eject HCL missing %q\n---\n%s", want, hcl)
		}
	}

	// A command with embedded quotes (example A's `RUBYOPT="-W0"`) must
	// be escaped, not emitted raw.
	procsA, err := Parse(strings.NewReader(exampleA))
	if err != nil {
		t.Fatalf("parse A: %v", err)
	}

	hclA, err := ToHCL(procsA, Options{Scope: "demo"})
	if err != nil {
		t.Fatalf("to hcl A: %v", err)
	}

	if !strings.Contains(hclA, `RUBYOPT=\"-W0\"`) {
		t.Errorf("expected escaped quotes in command; HCL:\n%s", hclA)
	}
}
