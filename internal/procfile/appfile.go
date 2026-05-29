package procfile

import (
	"encoding/json"
	"fmt"

	"go.voodu.clowk.in/internal/manifest"
)

// AppFile is the schema of `.voodu/app.json` — the project-link file that
// pins a scope and, optionally, declares per-process ingress (the bit a
// Procfile can't express). It grows into a small app manifest over time.
//
// Ingress is keyed by PROCESS NAME (the Procfile line / deployment it
// attaches to). The key is the routable identity: it becomes the ingress
// manifest's name, the default upstream service, and the source of the
// default port. Multiple processes can each declare an ingress:
//
//	{
//	  "scope": "ws",
//	  "ingress": {
//	    "web":   { "host": "app.example.com", "tls": { "enabled": true } },
//	    "admin": { "host": "admin.example.com", "tls": { "enabled": true } }
//	  }
//	}
type AppFile struct {
	Scope   string                `json:"scope"`
	Ingress map[string]AppIngress `json:"ingress,omitempty"`
}

// AppIngress is the app.json-facing ingress shape. It mirrors the
// controller's IngressSpec field-for-field (host/service/port) and
// reuses the spec sub-structs verbatim for tls/lb (so those are 1:1
// JSON). Locations accept BOTH the spec-native `locations` array and a
// friendly singular `location` object; `strip_prefix` is accepted as an
// alias for the spec's `strip`. toSpec() converts to a real IngressSpec.
type AppIngress struct {
	Host    string               `json:"host"`
	Service string               `json:"service,omitempty"`
	Port    int                  `json:"port,omitempty"`
	TLS     *manifest.IngressTLS `json:"tls,omitempty"`
	LB      *manifest.IngressLB  `json:"lb,omitempty"`

	Location  *AppLocation  `json:"location,omitempty"`  // friendly singular
	Locations []AppLocation `json:"locations,omitempty"` // spec-native array
}

// AppLocation is one routed path. `strip` is the spec field; `strip_prefix`
// is an accepted alias (either sets IngressLocation.Strip).
type AppLocation struct {
	Path        string `json:"path"`
	Strip       bool   `json:"strip,omitempty"`
	StripPrefix bool   `json:"strip_prefix,omitempty"`
}

func (l AppLocation) toSpec() manifest.IngressLocation {
	return manifest.IngressLocation{
		Path:  l.Path,
		Strip: l.Strip || l.StripPrefix,
	}
}

// toSpec converts an AppIngress into a controller IngressSpec.
// defaultService / defaultPort are the routable process's own name and
// assigned port — used when the operator didn't override them.
func (a AppIngress) toSpec(defaultService string, defaultPort int) manifest.IngressSpec {
	service := a.Service
	if service == "" {
		service = defaultService
	}

	port := a.Port
	if port == 0 {
		port = defaultPort
	}

	var locs []manifest.IngressLocation
	if a.Location != nil {
		locs = append(locs, a.Location.toSpec())
	}

	for _, l := range a.Locations {
		locs = append(locs, l.toSpec())
	}

	return manifest.IngressSpec{
		Host:      a.Host,
		Service:   service,
		Port:      port,
		TLS:       a.TLS,
		LB:        a.LB,
		Locations: locs,
	}
}

// ParseAppFile decodes `.voodu/app.json` bytes. An empty/zero AppFile is
// valid (no scope, no ingress).
func ParseAppFile(data []byte) (*AppFile, error) {
	var a AppFile

	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("parse app.json: %w", err)
	}

	return &a, nil
}
