package procfile

import (
	"encoding/json"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/manifest"
)

// appJSONWithIngress is the operator's shape from the design discussion:
// ingress keyed by process, friendly `location` singular + `strip_prefix`.
const appJSONWithIngress = `{
  "scope": "ws",
  "ingress": {
    "web": {
      "host": "api.example.com",
      "tls": { "enabled": true, "email": "ops@example.com" },
      "location": { "path": "/api", "strip_prefix": true },
      "lb": { "policy": "round_robin", "interval": "10s" }
    }
  }
}`

func TestParseAppFile_Ingress(t *testing.T) {
	app, err := ParseAppFile([]byte(appJSONWithIngress))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if app.Scope != "ws" {
		t.Errorf("scope = %q, want ws", app.Scope)
	}

	ing, ok := app.Ingress["web"]
	if !ok {
		t.Fatal("ingress[web] missing")
	}

	if ing.Host != "api.example.com" {
		t.Errorf("host = %q", ing.Host)
	}
	if ing.TLS == nil || !ing.TLS.Enabled || ing.TLS.Email != "ops@example.com" {
		t.Errorf("tls = %+v", ing.TLS)
	}

	// Friendly `location` + `strip_prefix` must fold into IngressSpec's
	// locations[]/strip.
	spec := ing.toSpec("web", 5000)
	if len(spec.Locations) != 1 || spec.Locations[0].Path != "/api" || !spec.Locations[0].Strip {
		t.Errorf("locations = %+v (want one /api strip=true)", spec.Locations)
	}
}

func TestToManifests_EmitsIngress(t *testing.T) {
	procs := []Process{
		{Type: "web", RawCommand: "bundle exec puma -p $PORT"},
		{Type: "worker", RawCommand: "bundle exec sidekiq"},
	}

	app, _ := ParseAppFile([]byte(appJSONWithIngress))

	mans, err := ToManifests(procs, Options{Scope: "ws", Ingress: app.Ingress})
	if err != nil {
		t.Fatalf("to manifests: %v", err)
	}

	// 2 deployments + 1 ingress.
	var ingress *controller.Manifest

	for i := range mans {
		if mans[i].Kind == controller.KindIngress {
			ingress = &mans[i]
		}
	}

	if ingress == nil {
		t.Fatal("no ingress manifest emitted")
	}
	if ingress.Scope != "ws" || ingress.Name != "web" {
		t.Errorf("ingress identity = %s/%s, want ws/web", ingress.Scope, ingress.Name)
	}

	var spec manifest.IngressSpec
	if err := json.Unmarshal(ingress.Spec, &spec); err != nil {
		t.Fatalf("decode ingress spec: %v", err)
	}

	if spec.Host != "api.example.com" {
		t.Errorf("host = %q", spec.Host)
	}
	// service defaults to the process name; port auto-fills from web's
	// assigned port (5000).
	if spec.Service != "web" {
		t.Errorf("service = %q, want web (default from process)", spec.Service)
	}
	if spec.Port != 5000 {
		t.Errorf("port = %d, want 5000 (auto-filled from deployment)", spec.Port)
	}
	if spec.TLS == nil || !spec.TLS.Enabled {
		t.Errorf("tls = %+v", spec.TLS)
	}
}

// TestToManifests_IngressPortDrivesDeployment pins the "declare it once"
// rule: an ingress port for a process sets that process's PORT env +
// published port too, so the deployment, the app ($PORT), and the ingress
// all agree — no leftover auto-assigned "random" port to mismatch on.
func TestToManifests_IngressPortDrivesDeployment(t *testing.T) {
	procs := []Process{
		{Type: "web", RawCommand: "serve out -l $PORT"},
		{Type: "worker", RawCommand: "sidekiq"},
	}

	ingress := map[string]AppIngress{
		"web": {Host: "x.example.com", Port: 80},
	}

	mans, err := ToManifests(procs, Options{Scope: "ws", Ingress: ingress})
	if err != nil {
		t.Fatalf("to manifests: %v", err)
	}

	specOf := func(name string) (manifest.DeploymentSpec, bool) {
		for _, m := range mans {
			if m.Kind == controller.KindDeployment && m.Name == name {
				var s manifest.DeploymentSpec
				_ = json.Unmarshal(m.Spec, &s)

				return s, true
			}
		}

		return manifest.DeploymentSpec{}, false
	}

	web, ok := specOf("web")
	if !ok {
		t.Fatal("web deployment missing")
	}
	if web.Env["PORT"] != "80" || len(web.Ports) != 1 || web.Ports[0] != "80" {
		t.Errorf("web should be pinned to 80 by the ingress; got PORT=%q ports=%v", web.Env["PORT"], web.Ports)
	}

	// worker (no ingress) keeps the auto port — and the pinned web did
	// NOT consume an auto slot, so worker is BasePort, not BasePort+1.
	worker, _ := specOf("worker")
	if worker.Env["PORT"] != "5000" {
		t.Errorf("worker auto-port should be 5000 (web's pin didn't consume a slot); got %q", worker.Env["PORT"])
	}

	// ingress agrees.
	for _, m := range mans {
		if m.Kind == controller.KindIngress {
			var s manifest.IngressSpec
			_ = json.Unmarshal(m.Spec, &s)

			if s.Port != 80 {
				t.Errorf("ingress port = %d, want 80", s.Port)
			}
		}
	}
}

// TestToManifests_IngressCustomFields pins the FULL custom-ingress
// surface through the app.json → IngressSpec mapping: lb, the plural
// `locations` array (multiple), the on-demand TLS fields, and explicit
// service/port overrides. This is the only Procfile-specific code path
// for these fields — once they land on IngressSpec the reconcile + caddy
// path is identical to HCL — so this guards a future appfile.go refactor
// from silently dropping one.
func TestToManifests_IngressCustomFields(t *testing.T) {
	const appJSON = `{
	  "scope": "ws",
	  "ingress": {
	    "web": {
	      "service": "api",
	      "port": 8080,
	      "host": "app.example.com",
	      "tls": { "on_demand": true, "ask": "https://api/allow", "provider": "internal" },
	      "lb": { "policy": "least_conn", "interval": "7s" },
	      "locations": [
	        { "path": "/api", "strip": true },
	        { "path": "/static", "strip_prefix": true }
	      ]
	    }
	  }
	}`

	app, err := ParseAppFile([]byte(appJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	procs := []Process{{Type: "web", RawCommand: "puma"}}

	mans, err := ToManifests(procs, Options{Scope: "ws", Ingress: app.Ingress})
	if err != nil {
		t.Fatalf("to manifests: %v", err)
	}

	var spec *manifest.IngressSpec

	for i := range mans {
		if mans[i].Kind == controller.KindIngress {
			var s manifest.IngressSpec
			if err := json.Unmarshal(mans[i].Spec, &s); err != nil {
				t.Fatalf("decode ingress spec: %v", err)
			}

			spec = &s
		}
	}

	if spec == nil {
		t.Fatal("no ingress manifest emitted")
	}

	if spec.Service != "api" {
		t.Errorf("service = %q, want api (explicit override)", spec.Service)
	}

	if spec.Port != 8080 {
		t.Errorf("port = %d, want 8080 (explicit override)", spec.Port)
	}

	if spec.TLS == nil || !spec.TLS.OnDemand || spec.TLS.Ask != "https://api/allow" || spec.TLS.Provider != "internal" {
		t.Errorf("tls on-demand fields not carried: %+v", spec.TLS)
	}

	if spec.LB == nil || spec.LB.Policy != "least_conn" || spec.LB.Interval != "7s" {
		t.Errorf("lb = %+v, want policy=least_conn interval=7s", spec.LB)
	}

	if len(spec.Locations) != 2 {
		t.Fatalf("locations = %d, want 2", len(spec.Locations))
	}

	if spec.Locations[0].Path != "/api" || !spec.Locations[0].Strip {
		t.Errorf("locations[0] = %+v, want /api strip=true", spec.Locations[0])
	}

	// strip_prefix is an alias that must fold into Strip.
	if spec.Locations[1].Path != "/static" || !spec.Locations[1].Strip {
		t.Errorf("locations[1] = %+v, want /static strip=true (via strip_prefix alias)", spec.Locations[1])
	}
}

func TestToManifests_IngressUnknownProcess(t *testing.T) {
	procs := []Process{{Type: "web", RawCommand: "puma"}}

	ingress := map[string]AppIngress{
		"nope": {Host: "x.example.com"},
	}

	if _, err := ToManifests(procs, Options{Scope: "ws", Ingress: ingress}); err == nil {
		t.Error("expected error for ingress referencing unknown process")
	}
}

func TestToManifests_IngressPortOverride(t *testing.T) {
	procs := []Process{{Type: "web", RawCommand: "puma"}}

	ingress := map[string]AppIngress{
		"web": {Host: "x.example.com", Port: 8080},
	}

	mans, err := ToManifests(procs, Options{Scope: "ws", Ingress: ingress})
	if err != nil {
		t.Fatalf("to manifests: %v", err)
	}

	for i := range mans {
		if mans[i].Kind != controller.KindIngress {
			continue
		}

		var spec manifest.IngressSpec
		_ = json.Unmarshal(mans[i].Spec, &spec)

		if spec.Port != 8080 {
			t.Errorf("port = %d, want 8080 (explicit override)", spec.Port)
		}
	}
}

func TestToHCL_RendersIngress(t *testing.T) {
	procs := []Process{{Type: "web", RawCommand: "bundle exec puma -p $PORT"}}

	app, _ := ParseAppFile([]byte(appJSONWithIngress))

	hcl, err := ToHCL(procs, Options{Scope: "ws", Ingress: app.Ingress})
	if err != nil {
		t.Fatalf("to hcl: %v", err)
	}

	for _, want := range []string{
		`ingress "ws" "web"`,
		`host    = "api.example.com"`,
		`tls {`,
		`enabled = true`,
		`location {`,
		`strip = true`,
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("ingress HCL missing %q\n---\n%s", want, hcl)
		}
	}

	// And it must re-parse through the real HCL parser.
	if _, err := manifest.ParseReader(strings.NewReader(hcl), manifest.FormatHCL, nil); err != nil {
		t.Fatalf("ejected HCL with ingress did not re-parse: %v\n---\n%s", err, hcl)
	}
}
