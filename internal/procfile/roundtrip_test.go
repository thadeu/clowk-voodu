package procfile

import (
	"encoding/json"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/manifest"
)

// TestEjectRoundTrips is the headline guarantee: the HCL produced by
// `--eject` parses back through the real manifest parser into the same
// resources ToManifests would have generated directly. This is what
// makes "later author HCL at the same (scope,name) overlays cleanly"
// true — the ejected file IS valid voodu HCL.
func TestEjectRoundTrips(t *testing.T) {
	procs, err := Parse(strings.NewReader(exampleB))
	if err != nil {
		t.Fatalf("parse procfile: %v", err)
	}

	opts := Options{Scope: "demo"}

	hcl, err := ToHCL(procs, opts)
	if err != nil {
		t.Fatalf("to hcl: %v", err)
	}

	parsed, err := manifest.ParseReader(strings.NewReader(hcl), manifest.FormatHCL, nil)
	if err != nil {
		t.Fatalf("parse ejected HCL: %v\n---\n%s", err, hcl)
	}

	direct, err := ToManifests(procs, opts)
	if err != nil {
		t.Fatalf("to manifests: %v", err)
	}

	if len(parsed) != len(direct) {
		t.Fatalf("ejected HCL parsed to %d manifests, ToManifests gave %d", len(parsed), len(direct))
	}

	index := func(ms []controller.Manifest) map[string]controller.Manifest {
		m := map[string]controller.Manifest{}
		for _, x := range ms {
			m[string(x.Kind)+"/"+x.Scope+"/"+x.Name] = x
		}

		return m
	}

	pIdx, dIdx := index(parsed), index(direct)

	for key, d := range dIdx {
		p, ok := pIdx[key]
		if !ok {
			t.Errorf("ejected HCL missing %s", key)

			continue
		}

		// Compare the command field — the load-bearing one (shell-wrap
		// + raw command). Both decode to the same spec shape per kind.
		var dCmd, pCmd []string

		switch d.Kind {
		case controller.KindDeployment:
			var ds, ps manifest.DeploymentSpec
			_ = json.Unmarshal(d.Spec, &ds)
			_ = json.Unmarshal(p.Spec, &ps)
			dCmd, pCmd = ds.Command, ps.Command

			if ps.Restart != ds.Restart {
				t.Errorf("%s restart: ejected %q, direct %q", key, ps.Restart, ds.Restart)
			}
			if ps.Env["PORT"] != ds.Env["PORT"] {
				t.Errorf("%s PORT: ejected %q, direct %q", key, ps.Env["PORT"], ds.Env["PORT"])
			}
		case controller.KindJob:
			var dj, pj manifest.JobSpec
			_ = json.Unmarshal(d.Spec, &dj)
			_ = json.Unmarshal(p.Spec, &pj)
			dCmd, pCmd = dj.Command, pj.Command
		}

		if strings.Join(pCmd, "\x00") != strings.Join(dCmd, "\x00") {
			t.Errorf("%s command: ejected %v, direct %v", key, pCmd, dCmd)
		}
	}
}
