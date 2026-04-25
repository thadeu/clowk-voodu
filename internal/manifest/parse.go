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

// hclExts is the set of file extensions treated as HCL manifests. All
// four are equivalent — .hcl for tool compatibility, the voodu-branded
// ones for projects that want the manifest to read like a first-class
// config (web.voodu, api.vdu, etc.).
var hclExts = map[string]bool{
	".hcl":   true,
	".voodu": true,
	".vdu":   true,
	".vd":    true,
}

func formatFromExt(path string) (Format, error) {
	ext := strings.ToLower(filepath.Ext(path))

	switch {
	case hclExts[ext]:
		return FormatHCL, nil
	case ext == ".yml", ext == ".yaml":
		return FormatYAML, nil
	default:
		return "", fmt.Errorf("unsupported extension %q (want .hcl, .voodu, .vdu, .vd, .yml, .yaml)", filepath.Ext(path))
	}
}

// hclRoot is the top-level shape of an HCL manifest file. Each kind has
// its own slice so hclsimple can tell blocks apart by label. HCL block
// structs list their fields explicitly — hcl/v2 doesn't walk anonymous
// embedded specs, so we mirror (not embed) the typed Spec shape.
//
// `app` is authoring sugar — not a controller kind. Each app block in
// the source expands into one deployment + one ingress with the same
// (scope, name); the controller never sees an "app". This keeps the
// runtime contract simple (every manifest is one of the canonical
// kinds) while letting users declare the overwhelmingly common
// "1 deployment ↔ 1 ingress" pair in a single block.
type hclRoot struct {
	Apps        []hclApp        `hcl:"app,block"`
	Deployments []hclDeployment `hcl:"deployment,block"`
	Databases   []hclDatabase   `hcl:"database,block"`
	Ingresses   []hclIngress    `hcl:"ingress,block"`
	Jobs        []hclJob        `hcl:"job,block"`
	CronJobs    []hclCronJob    `hcl:"cronjob,block"`
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

// hclApp is authoring sugar for the "deployment + ingress with the
// same (scope, name)" pair — by far the most common shape for web
// apps. It is NOT a controller kind: the parser expands every app
// into two manifests (one deployment, one ingress) and the rest of
// the system never knows the original block was an app. That way
// `voodu describe`, diff, prune, --force and all client commands
// keep working against the canonical kinds without special cases.
//
// Field selection rules:
//
//   - Every deployment field is exposed verbatim (image, replicas,
//     env, ports, lang block, …). The deployment side of the app is
//     a regular deployment.
//
//   - On the ingress side, only `host`, `tls`, `location`, and `lb`
//     are exposed. We omit `service` and `port` deliberately:
//
//     • `service` would always equal the app's own name (same
//       identity), so making the user repeat it is busywork. The
//       ingress handler already defaults service to the ingress
//       name — that default lands on the deployment we synthesise
//       next to it.
//
//     • `port` would normally come from the ingress, but the
//       deployment's `ports` is right here in the same block. The
//       ingress handler derives the upstream port from the
//       deployment's first container port when ingress.Port == 0.
//       One source of truth — the user types `ports = ["3000"]`
//       once and both sides agree.
//
// If you genuinely need two ingresses on the same deployment, or a
// custom service-name pointer, drop the `app` shorthand and write
// `deployment` + `ingress` separately. The parser doesn't try to
// cover every shape — it covers the 90% case in fewer lines.
type hclApp struct {
	Scope string `hcl:"scope,label"`
	Name  string `hcl:"name,label"`

	// Deployment-side fields — verbatim copy of hclDeployment minus
	// the labels (which the app already carries). Kept in sync by
	// hand: HCL doesn't walk embedded structs, and a code-gen step
	// would be heavier than the duplication.
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

	// Ingress-side fields. Host is required (no host = no reason to
	// be an app, write a plain deployment instead).
	Host      string               `hcl:"host"`
	TLS       *hclIngressTLS       `hcl:"tls,block"`
	Locations []hclIngressLocation `hcl:"location,block"`
	LB        *hclIngressLB        `hcl:"lb,block"`
}

// deploymentSpec extracts the deployment half of the app block. The
// shape is identical to what hclDeployment.spec() produces — same
// defaults, same lang handling — so a deployment authored as `app`
// reconciles byte-for-byte the same as one authored as `deployment`.
func (b hclApp) deploymentSpec() DeploymentSpec {
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

// ingressSpec extracts the ingress half. Service is left empty so
// the controller's default-service-to-name path lands on the
// deployment we just emitted alongside. Port stays zero so the
// deployment's ports[0] is auto-picked at reconcile.
func (b hclApp) ingressSpec() IngressSpec {
	out := IngressSpec{Host: b.Host}

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

// hclJob is the HCL surface for a one-shot container. Two labels —
// scope and name — match the deployment shape so identity is consistent
// across kinds. Build-mode fields (Workdir/Dockerfile/Path/Lang) ride
// alongside Image because jobs frequently reuse the surrounding repo's
// build pipeline (rake task, alembic migration, …) without wanting a
// separate registry image.
type hclJob struct {
	Scope       string            `hcl:"scope,label"`
	Name        string            `hcl:"name,label"`
	Image       string            `hcl:"image,optional"`
	Workdir     string            `hcl:"workdir,optional"`
	Dockerfile  string            `hcl:"dockerfile,optional"`
	Path        string            `hcl:"path,optional"`
	Command     []string          `hcl:"command,optional"`
	Env         map[string]string `hcl:"env,optional"`
	Volumes     []string          `hcl:"volumes,optional"`
	Network     string            `hcl:"network,optional"`
	Networks    []string          `hcl:"networks,optional"`
	NetworkMode string            `hcl:"network_mode,optional"`
	Timeout     string            `hcl:"timeout,optional"`

	Lang *hclLangBlock `hcl:"lang,block"`
}

func (b hclJob) spec() JobSpec {
	s := JobSpec{
		Image:       b.Image,
		Workdir:     b.Workdir,
		Dockerfile:  b.Dockerfile,
		Path:        b.Path,
		Command:     b.Command,
		Env:         b.Env,
		Volumes:     b.Volumes,
		Network:     b.Network,
		Networks:    b.Networks,
		NetworkMode: b.NetworkMode,
		Timeout:     b.Timeout,
	}

	if b.Lang != nil {
		s.Lang = &LangSpec{
			Name:       b.Lang.Name,
			Version:    b.Lang.Version,
			Entrypoint: b.Lang.Entrypoint,
			BuildArgs:  b.Lang.BuildArgs,
		}
	}

	return s
}

// hclCronJob is the HCL surface for a scheduled job. The shape mirrors
// the standalone job — same 2-label scope/name and same execution
// fields ride at the block root — but adds the schedule controls
// (schedule, timezone, suspend, concurrency_policy, history limits).
//
// We deliberately flatten the job spec into the cronjob root rather
// than nesting a `job { ... }` sub-block: from an authoring POV the
// "this is a job that runs on a schedule" mental model produces a
// single block, and HCL's optional nested blocks make the alternative
// noisy. The handler reconstructs a JobSpec at apply time by copying
// the matching fields.
type hclCronJob struct {
	Scope string `hcl:"scope,label"`
	Name  string `hcl:"name,label"`

	Schedule          string `hcl:"schedule"`
	Timezone          string `hcl:"timezone,optional"`
	Suspend           bool   `hcl:"suspend,optional"`
	ConcurrencyPolicy string `hcl:"concurrency_policy,optional"`

	SuccessfulHistoryLimit int `hcl:"successful_history_limit,optional"`
	FailedHistoryLimit     int `hcl:"failed_history_limit,optional"`

	Image       string            `hcl:"image,optional"`
	Workdir     string            `hcl:"workdir,optional"`
	Dockerfile  string            `hcl:"dockerfile,optional"`
	Path        string            `hcl:"path,optional"`
	Command     []string          `hcl:"command,optional"`
	Env         map[string]string `hcl:"env,optional"`
	Volumes     []string          `hcl:"volumes,optional"`
	Network     string            `hcl:"network,optional"`
	Networks    []string          `hcl:"networks,optional"`
	NetworkMode string            `hcl:"network_mode,optional"`
	Timeout     string            `hcl:"timeout,optional"`

	Lang *hclLangBlock `hcl:"lang,block"`
}

func (b hclCronJob) spec() CronJobSpec {
	job := JobSpec{
		Image:       b.Image,
		Workdir:     b.Workdir,
		Dockerfile:  b.Dockerfile,
		Path:        b.Path,
		Command:     b.Command,
		Env:         b.Env,
		Volumes:     b.Volumes,
		Network:     b.Network,
		Networks:    b.Networks,
		NetworkMode: b.NetworkMode,
		Timeout:     b.Timeout,
	}

	if b.Lang != nil {
		job.Lang = &LangSpec{
			Name:       b.Lang.Name,
			Version:    b.Lang.Version,
			Entrypoint: b.Lang.Entrypoint,
			BuildArgs:  b.Lang.BuildArgs,
		}
	}

	return CronJobSpec{
		Schedule:               b.Schedule,
		Job:                    job,
		ConcurrencyPolicy:      b.ConcurrencyPolicy,
		Timezone:               b.Timezone,
		Suspend:                b.Suspend,
		SuccessfulHistoryLimit: b.SuccessfulHistoryLimit,
		FailedHistoryLimit:     b.FailedHistoryLimit,
	}
}

func parseHCL(source string, raw []byte) ([]controller.Manifest, error) {
	// hclsimple.Decode hard-codes its dispatch on ".hcl" / ".hcl.json",
	// so any other extension (our branded .voodu/.vdu/.vd, or a stdin
	// source with no extension at all) gets rewritten to a synthetic
	// .hcl path before the call. The original name is kept for error
	// messages when it already ended in .hcl.
	if strings.ToLower(filepath.Ext(source)) != ".hcl" {
		source = source + ".hcl"
	}

	var root hclRoot

	if err := hclsimple.Decode(source, raw, nil, &root); err != nil {
		return nil, err
	}

	var out []controller.Manifest

	// Apps expand into a deployment + ingress pair with the same
	// (scope, name). Emit the pair first so the canonical kinds are
	// already in `out` when collision detection runs at the end — that
	// way duplicate (kind, scope, name) errors point at the standalone
	// block as the redeclaration regardless of where it sits in the
	// source.
	for _, b := range root.Apps {
		dep, err := encode(controller.KindDeployment, b.Scope, b.Name, b.deploymentSpec())
		if err != nil {
			return nil, err
		}

		out = append(out, dep)

		ing, err := encode(controller.KindIngress, b.Scope, b.Name, b.ingressSpec())
		if err != nil {
			return nil, err
		}

		out = append(out, ing)
	}

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

	for _, b := range root.Ingresses {
		m, err := encode(controller.KindIngress, b.Scope, b.Name, b.spec())
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	for _, b := range root.Jobs {
		m, err := encode(controller.KindJob, b.Scope, b.Name, b.spec())
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	for _, b := range root.CronJobs {
		m, err := encode(controller.KindCronJob, b.Scope, b.Name, b.spec())
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	if err := assertNoDuplicateIdentities(out); err != nil {
		return nil, err
	}

	return out, nil
}

// assertNoDuplicateIdentities rejects files that declare the same
// (kind, scope, name) tuple more than once. Catches two authoring
// mistakes in a single sweep:
//
//   - Two standalone blocks of the same kind colliding — e.g. two
//     `deployment "x" "y" {}` in one file. hcl/v2 doesn't error on
//     these (shape depends on labels, not uniqueness), so without
//     this check the latter declaration would silently win.
//
//   - An `app "x" "y"` paired with a standalone `deployment "x" "y"`
//     or `ingress "x" "y"`. The app already emitted the canonical
//     pair; the standalone is either a duplicate (busywork) or an
//     override attempt (better expressed by dropping the `app`
//     shorthand and writing both blocks explicitly). Either way,
//     the user should see it instead of guessing why their override
//     didn't take effect.
//
// The error doesn't point at line numbers — hcl.Decode's position
// info is gone once we project into Manifest, and threading it
// through would mean re-reading the source. The kind/scope/name
// tuple is enough to grep for.
func assertNoDuplicateIdentities(mans []controller.Manifest) error {
	seen := make(map[string]struct{}, len(mans))

	for _, m := range mans {
		key := string(m.Kind) + "/" + m.Scope + "/" + m.Name

		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate identity %s: declared more than once (check `app` blocks and standalone %s blocks for collisions)", key, m.Kind)
		}

		seen[key] = struct{}{}
	}

	return nil
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

	case controller.KindIngress:
		var s IngressSpec
		return s, node.Decode(&s)

	case controller.KindJob:
		var s JobSpec
		return s, node.Decode(&s)

	case controller.KindCronJob:
		var s CronJobSpec
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
