package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"gopkg.in/yaml.v3"

	"go.voodu.clowk.in/internal/controller"
)

// Format identifies the input serialization. Selection is by file
// extension; stdin requires an explicit Format.
type Format string

const (
	FormatHCL  Format = "hcl"
	FormatYAML Format = "yaml"
	// FormatJSON accepts a pre-parsed []controller.Manifest. It's what the
	// client uses to stream manifests to a remote over SSH: ${VAR}
	// interpolation must happen on the dev machine (the server doesn't
	// see local env vars), so the client parses HCL/YAML then re-emits
	// JSON for the remote to re-ingest.
	FormatJSON Format = "json"
)

// ParseFile reads a single manifest file and returns zero-or-more
// manifests. The format is picked from the extension (.hcl vs .yml/.yaml).
// ${VAR} interpolation is applied to the raw bytes before parsing so the
// same mechanism works for both formats.
func ParseFile(path string, vars map[string]string) ([]controller.Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	format, err := formatFromExt(path)
	if err != nil {
		return nil, err
	}

	mans, err := parseBytes(path, raw, format, vars)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return mans, nil
}

// ParseDir walks root recursively collecting .hcl/.yml/.yaml files.
// Files are parsed in lexicographic order so apply is deterministic.
func ParseDir(root string, vars map[string]string) ([]controller.Manifest, error) {
	var files []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		if _, err := formatFromExt(path); err != nil {
			return nil
		}

		files = append(files, path)

		return nil
	})
	if err != nil {
		return nil, err
	}

	var out []controller.Manifest

	for _, f := range files {
		mans, err := ParseFile(f, vars)
		if err != nil {
			return nil, err
		}

		out = append(out, mans...)
	}

	return out, nil
}

// ParseReader parses from an arbitrary reader with an explicit format.
// Used for stdin where we cannot sniff an extension.
func ParseReader(r io.Reader, format Format, vars map[string]string) ([]controller.Manifest, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	return parseBytes("<stdin>", raw, format, vars)
}

func parseBytes(source string, raw []byte, format Format, vars map[string]string) ([]controller.Manifest, error) {
	// JSON is already canonical — skip ${VAR} interpolation because the
	// client has already expanded env refs before re-emitting.
	if format == FormatJSON {
		return parseJSON(raw)
	}

	interp, err := Interpolate(string(raw), vars)
	if err != nil {
		return nil, err
	}

	switch format {
	case FormatHCL:
		return parseHCL(source, []byte(interp))
	case FormatYAML:
		return parseYAML([]byte(interp))
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

func parseJSON(raw []byte) ([]controller.Manifest, error) {
	var out []controller.Manifest

	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse json manifests: %w", err)
	}

	return out, nil
}

func formatFromExt(path string) (Format, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".hcl", ".vdu", ".vd":
		return FormatHCL, nil
	case ".yml", ".yaml":
		return FormatYAML, nil
	default:
		return "", fmt.Errorf("unsupported extension %q (want .hcl, .vdu, .vd, .yml, .yaml)", filepath.Ext(path))
	}
}

// hclRoot is the top-level shape of an HCL manifest file. Each kind has
// its own slice so hclsimple can tell blocks apart by label. HCL block
// structs list their fields explicitly — hcl/v2 doesn't walk anonymous
// embedded specs, so we mirror (not embed) the typed Spec shape.
type hclRoot struct {
	Deployments []hclDeployment `hcl:"deployment,block"`
	Databases   []hclDatabase   `hcl:"database,block"`
	Services    []hclService    `hcl:"service,block"`
	Ingresses   []hclIngress    `hcl:"ingress,block"`
}

type hclDeployment struct {
	Scope        string            `hcl:"scope,label"`
	Name         string            `hcl:"name,label"`
	Image        string            `hcl:"image,optional"`
	Workdir      string            `hcl:"workdir,optional"`
	Dockerfile   string            `hcl:"dockerfile,optional"`
	Path         string            `hcl:"path,optional"`
	Replicas     int               `hcl:"replicas,optional"`
	Command      []string          `hcl:"command,optional"`
	Env          map[string]string `hcl:"env,optional"`
	Ports        []string          `hcl:"ports,optional"`
	Volumes      []string          `hcl:"volumes,optional"`
	Network      string            `hcl:"network,optional"`
	Networks     []string          `hcl:"networks,optional"`
	NetworkMode  string            `hcl:"network_mode,optional"`
	Restart      string            `hcl:"restart,optional"`
	HealthCheck  string            `hcl:"health_check,optional"`
	PostDeploy   []string          `hcl:"post_deploy,optional"`
	KeepReleases int               `hcl:"keep_releases,optional"`

	Lang *hclLangBlock `hcl:"lang,block"`
}

type hclLangBlock struct {
	Name       string            `hcl:"name,optional"`
	Version    string            `hcl:"version,optional"`
	Entrypoint string            `hcl:"entrypoint,optional"`
	BuildArgs  map[string]string `hcl:"build_args,optional"`
}

func (b hclDeployment) spec() DeploymentSpec {
	s := DeploymentSpec{
		Image:        b.Image,
		Workdir:      b.Workdir,
		Dockerfile:   b.Dockerfile,
		Path:         b.Path,
		Replicas:     b.Replicas,
		Command:      b.Command,
		Env:          b.Env,
		Ports:        b.Ports,
		Volumes:      b.Volumes,
		Network:      b.Network,
		Networks:     b.Networks,
		NetworkMode:  b.NetworkMode,
		Restart:      b.Restart,
		HealthCheck:  b.HealthCheck,
		PostDeploy:   b.PostDeploy,
		KeepReleases: b.KeepReleases,
	}

	if b.Lang != nil {
		s.Lang = &LangSpec{
			Name:       b.Lang.Name,
			Version:    b.Lang.Version,
			Entrypoint: b.Lang.Entrypoint,
			BuildArgs:  b.Lang.BuildArgs,
		}
	}

	s.applyDefaults()

	return s
}

type hclDatabase struct {
	Name    string            `hcl:"name,label"`
	Engine  string            `hcl:"engine"`
	Version string            `hcl:"version,optional"`
	Storage string            `hcl:"storage,optional"`
	Backup  *hclBackup        `hcl:"backup,block"`
	Params  map[string]string `hcl:"params,optional"`
}

type hclBackup struct {
	Schedule  string `hcl:"schedule,optional"`
	Retention string `hcl:"retention,optional"`
	Target    string `hcl:"target,optional"`
}

func (b hclDatabase) spec() DatabaseSpec {
	out := DatabaseSpec{Engine: b.Engine, Version: b.Version, Storage: b.Storage, Params: b.Params}

	if b.Backup != nil {
		out.Backup = &DatabaseBackup{
			Schedule: b.Backup.Schedule, Retention: b.Backup.Retention, Target: b.Backup.Target,
		}
	}

	return out
}

type hclService struct {
	Name   string   `hcl:"name,label"`
	Target string   `hcl:"target"`
	Port   int      `hcl:"port,optional"`
	Ports  []string `hcl:"ports,optional"`
}

func (b hclService) spec() ServiceSpec {
	return ServiceSpec{Target: b.Target, Port: b.Port, Ports: b.Ports}
}

type hclIngress struct {
	Scope     string              `hcl:"scope,label"`
	Name      string              `hcl:"name,label"`
	Host      string              `hcl:"host"`
	Service   string              `hcl:"service,optional"`
	Port      int                 `hcl:"port,optional"`
	TLS       *hclIngressTLS      `hcl:"tls,block"`
	Locations []hclIngressLocation `hcl:"location,block"`
	LB        *hclIngressLB       `hcl:"lb,block"`
}

type hclIngressLB struct {
	Policy   string `hcl:"policy,optional"`
	Interval string `hcl:"interval,optional"`
}

type hclIngressTLS struct {
	Enabled  bool   `hcl:"enabled,optional"`
	Provider string `hcl:"provider,optional"`
	Email    string `hcl:"email,optional"`
	OnDemand bool   `hcl:"on_demand,optional"`
	Ask      string `hcl:"ask,optional"`
}

type hclIngressLocation struct {
	Path  string `hcl:"path"`
	Strip bool   `hcl:"strip,optional"`
}

func (b hclIngress) spec() IngressSpec {
	out := IngressSpec{Host: b.Host, Service: b.Service, Port: b.Port}

	if b.TLS != nil {
		out.TLS = &IngressTLS{
			Enabled:  b.TLS.Enabled,
			Provider: b.TLS.Provider,
			Email:    b.TLS.Email,
			OnDemand: b.TLS.OnDemand,
			Ask:      b.TLS.Ask,
		}
	}

	if len(b.Locations) > 0 {
		out.Locations = make([]IngressLocation, 0, len(b.Locations))
		for _, loc := range b.Locations {
			out.Locations = append(out.Locations, IngressLocation{
				Path:  loc.Path,
				Strip: loc.Strip,
			})
		}
	}

	if b.LB != nil {
		out.LB = &IngressLB{Policy: b.LB.Policy, Interval: b.LB.Interval}
	}

	return out
}

func parseHCL(source string, raw []byte) ([]controller.Manifest, error) {
	// hclsimple.Decode routes by the source filename's extension, so
	// synthesize one for stdin/readers that lack a path.
	ext := filepath.Ext(source)

	if ext != ".hcl" && ext != ".vdu" && ext != ".vd" {
		source = source + ".hcl"
	}

	var root hclRoot

	if err := hclsimple.Decode(source, raw, nil, &root); err != nil {
		return nil, err
	}

	var out []controller.Manifest

	for _, b := range root.Deployments {
		m, err := encode(controller.KindDeployment, b.Scope, b.Name, b.spec())
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	for _, b := range root.Databases {
		m, err := encode(controller.KindDatabase, "", b.Name, b.spec())
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	for _, b := range root.Services {
		m, err := encode(controller.KindService, "", b.Name, b.spec())
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	for _, b := range root.Ingresses {
		m, err := encode(controller.KindIngress, b.Scope, b.Name, b.spec())
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	return out, nil
}

// yamlDoc mirrors the controller wire shape so users can hand-roll
// manifests that match what the API already consumes. Multi-doc files
// are supported via `---` separators. Scope is optional in the YAML
// surface — HCL is the preferred syntax for scoped kinds and is where
// the 2-label shape becomes natural; YAML users who need scope can
// still set it via the explicit `scope:` field.
type yamlDoc struct {
	Kind  string    `yaml:"kind"`
	Scope string    `yaml:"scope,omitempty"`
	Name  string    `yaml:"name"`
	Spec  yaml.Node `yaml:"spec"`
}

func parseYAML(raw []byte) ([]controller.Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))

	var out []controller.Manifest

	for {
		var doc yamlDoc

		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err
		}

		if doc.Kind == "" && doc.Name == "" {
			continue
		}

		kind, err := controller.ParseKind(doc.Kind)
		if err != nil {
			return nil, err
		}

		spec, err := decodeYAMLSpec(kind, doc.Name, doc.Spec)
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", doc.Kind, doc.Name, err)
		}

		m, err := encode(kind, doc.Scope, doc.Name, spec)
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	return out, nil
}

// decodeYAMLSpec enforces the per-kind schema when reading YAML. HCL gets
// this for free from hclsimple; YAML has to do it explicitly to keep
// error messages actionable.
func decodeYAMLSpec(kind controller.Kind, name string, node yaml.Node) (any, error) {
	switch kind {
	case controller.KindDeployment:
		var s DeploymentSpec
		if err := node.Decode(&s); err != nil {
			return s, err
		}

		s.applyDefaults()

		return s, nil

	case controller.KindDatabase:
		var s DatabaseSpec
		return s, node.Decode(&s)

	case controller.KindService:
		var s ServiceSpec
		return s, node.Decode(&s)

	case controller.KindIngress:
		var s IngressSpec
		return s, node.Decode(&s)

	default:
		return nil, fmt.Errorf("unknown kind %q", kind)
	}
}

// encode marshals a typed spec into the JSON-valued Manifest shape the
// controller expects, and applies minimum validation.
func encode(kind controller.Kind, scope, name string, spec any) (controller.Manifest, error) {
	if name == "" {
		return controller.Manifest{}, fmt.Errorf("%s: missing name", kind)
	}

	if controller.IsScoped(kind) && scope == "" {
		return controller.Manifest{}, fmt.Errorf("%s/%s: scope is required (use `%s \"scope\" \"%s\" { ... }`)", kind, name, kind, name)
	}

	b, err := json.Marshal(spec)
	if err != nil {
		return controller.Manifest{}, err
	}

	return controller.Manifest{
		Kind:  kind,
		Scope: scope,
		Name:  name,
		Spec:  json.RawMessage(b),
	}, nil
}
