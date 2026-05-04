package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
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
		// Pre-escape server-side refs (`${ref.…}`, `${asset.…}`)
		// so HCL doesn't choke on them as template variables.
		// They land in the manifest as literal `${…}` and the
		// controller resolves them at reconcile time.
		return parseHCL(source, []byte(escapeServerSideRefsForHCL(interp)))
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

// HCL block structs list their fields explicitly — hcl/v2 doesn't
// walk anonymous embedded specs, so we mirror (not embed) the typed
// Spec shape.
//
// `app` is authoring sugar — not a controller kind. Each app block
// in the source expands into one deployment + one ingress with the
// same (scope, name); the controller never sees an "app". This
// keeps the runtime contract simple (every manifest is one of the
// canonical kinds) while letting users declare the overwhelmingly
// common "1 deployment ↔ 1 ingress" pair in a single block.
//
// Block-name dispatch happens in parseHCL via the hclsyntax-level
// iteration. Unknown block types (`postgres`, `redis`, …) are not
// rejected — they're emitted as Manifests with Kind = block type
// and Spec = JSON of the block's attributes. The server side
// (M-D1+) decides whether a plugin is registered to expand them.

type hclDeployment struct {
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

	Lang      *hclLangBlock    `hcl:"lang,block"`
	Release   *hclReleaseBlock `hcl:"release,block"`
	DependsOn *hclDependsOn    `hcl:"depends_on,block"`
}

// hclDependsOn is the HCL surface for explicit dependencies. The
// only sub-attribute today is `assets` — a list of ref strings the
// consumer depends on. See manifest.DependsOn for the wire-shape
// counterpart and the rationale for declaring deps that aren't
// visible as textual `${asset.…}` refs.
type hclDependsOn struct {
	Assets []string `hcl:"assets,optional"`
}

type hclLangBlock struct {
	Name       string            `hcl:"name,optional"`
	Version    string            `hcl:"version,optional"`
	Entrypoint string            `hcl:"entrypoint,optional"`
	BuildArgs  map[string]string `hcl:"build_args,optional"`
}

// hclReleaseBlock is the HCL surface for the release phase. Each
// command field is a list so the operator can write the natural
// `command = ["rails", "db:migrate"]` shape; voodu hands the slice
// to docker exec verbatim (no shell interpretation unless explicit).
type hclReleaseBlock struct {
	Command     []string `hcl:"command,optional"`
	PreCommand  []string `hcl:"pre_command,optional"`
	PostCommand []string `hcl:"post_command,optional"`
	Timeout     string   `hcl:"timeout,optional"`
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

	if b.Release != nil {
		s.Release = &ReleaseSpec{
			Command:     b.Release.Command,
			PreCommand:  b.Release.PreCommand,
			PostCommand: b.Release.PostCommand,
			Timeout:     b.Release.Timeout,
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

	Lang      *hclLangBlock    `hcl:"lang,block"`
	Release   *hclReleaseBlock `hcl:"release,block"`
	DependsOn *hclDependsOn    `hcl:"depends_on,block"`

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

	if b.Release != nil {
		s.Release = &ReleaseSpec{
			Command:     b.Release.Command,
			PreCommand:  b.Release.PreCommand,
			PostCommand: b.Release.PostCommand,
			Timeout:     b.Release.Timeout,
		}
	}

	if b.DependsOn != nil {
		s.DependsOn = &DependsOn{Assets: b.DependsOn.Assets}
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

// hclStatefulset is the HCL surface for a statefulset workload —
// pods with stable ordinal identity, per-pod storage (volume claim
// templates from M-S2), and ordered rollout. Image-mode only on
// M-S0/M-S1; build-mode and release blocks are deferred since
// statefulsets in practice are databases / queues / caches with
// prebuilt registry images.
//
// Field selection is a strict subset of hclDeployment. Lang and
// Release are absent (not "optional" — adding them prematurely
// would advertise capabilities the handler doesn't yet honour).
// VolumeClaim becomes a block list in M-S2.
type hclStatefulset struct {
	Image       string            `hcl:"image,optional"`
	Workdir     string            `hcl:"workdir,optional"`
	Dockerfile  string            `hcl:"dockerfile,optional"`
	Path        string            `hcl:"path,optional"`
	Replicas    int               `hcl:"replicas,optional"`
	Command     []string          `hcl:"command,optional"`
	Env         map[string]string `hcl:"env,optional"`
	EnvFrom     []string          `hcl:"env_from,optional"`
	Ports       []string          `hcl:"ports,optional"`
	Volumes     []string          `hcl:"volumes,optional"`
	Network     string            `hcl:"network,optional"`
	Networks    []string          `hcl:"networks,optional"`
	NetworkMode string            `hcl:"network_mode,optional"`
	Restart     string            `hcl:"restart,optional"`
	HealthCheck string            `hcl:"health_check,optional"`

	Lang         *hclLangBlock    `hcl:"lang,block"`
	VolumeClaims []hclVolumeClaim `hcl:"volume_claim,block"`
	DependsOn    *hclDependsOn    `hcl:"depends_on,block"`
}

// hclVolumeClaim is one per-pod volume template. The block label
// (`volume_claim "data"`) becomes the claim name; remaining fields
// are body attributes. Only MountPath is mandatory — Size is
// informational on M-S2 (docker has no native quota enforcement).
type hclVolumeClaim struct {
	Name      string `hcl:"name,label"`
	MountPath string `hcl:"mount_path"`
	Size      string `hcl:"size,optional"`
}

func (b hclStatefulset) spec() StatefulsetSpec {
	s := StatefulsetSpec{
		Image:       b.Image,
		Workdir:     b.Workdir,
		Dockerfile:  b.Dockerfile,
		Path:        b.Path,
		Replicas:    b.Replicas,
		Command:     b.Command,
		Env:         b.Env,
		EnvFrom:     b.EnvFrom,
		Ports:       b.Ports,
		Volumes:     b.Volumes,
		Network:     b.Network,
		Networks:    b.Networks,
		NetworkMode: b.NetworkMode,
		Restart:     b.Restart,
		HealthCheck: b.HealthCheck,
	}

	if b.Lang != nil {
		s.Lang = &LangSpec{
			Name:       b.Lang.Name,
			Version:    b.Lang.Version,
			Entrypoint: b.Lang.Entrypoint,
			BuildArgs:  b.Lang.BuildArgs,
		}
	}

	if len(b.VolumeClaims) > 0 {
		s.VolumeClaims = make([]VolumeClaim, 0, len(b.VolumeClaims))
		for _, c := range b.VolumeClaims {
			s.VolumeClaims = append(s.VolumeClaims, VolumeClaim{
				Name:      c.Name,
				MountPath: c.MountPath,
				Size:      c.Size,
			})
		}
	}

	if b.DependsOn != nil {
		s.DependsOn = &DependsOn{Assets: b.DependsOn.Assets}
	}

	s.applyDefaults()

	return s
}

type hclIngress struct {
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
	Image       string            `hcl:"image,optional"`
	Workdir     string            `hcl:"workdir,optional"`
	Dockerfile  string            `hcl:"dockerfile,optional"`
	Path        string            `hcl:"path,optional"`
	Command     []string          `hcl:"command,optional"`
	Env         map[string]string `hcl:"env,optional"`
	EnvFrom     []string          `hcl:"env_from,optional"`
	Volumes     []string          `hcl:"volumes,optional"`
	Network     string            `hcl:"network,optional"`
	Networks    []string          `hcl:"networks,optional"`
	NetworkMode string            `hcl:"network_mode,optional"`
	Timeout     string            `hcl:"timeout,optional"`

	SuccessfulHistoryLimit int `hcl:"successful_history_limit,optional"`
	FailedHistoryLimit     int `hcl:"failed_history_limit,optional"`

	Lang      *hclLangBlock `hcl:"lang,block"`
	DependsOn *hclDependsOn `hcl:"depends_on,block"`
}

func (b hclJob) spec() JobSpec {
	s := JobSpec{
		Image:                  b.Image,
		Workdir:                b.Workdir,
		Dockerfile:             b.Dockerfile,
		Path:                   b.Path,
		Command:                b.Command,
		Env:                    b.Env,
		EnvFrom:                b.EnvFrom,
		Volumes:                b.Volumes,
		Network:                b.Network,
		Networks:               b.Networks,
		NetworkMode:            b.NetworkMode,
		Timeout:                b.Timeout,
		SuccessfulHistoryLimit: b.SuccessfulHistoryLimit,
		FailedHistoryLimit:     b.FailedHistoryLimit,
	}

	if b.Lang != nil {
		s.Lang = &LangSpec{
			Name:       b.Lang.Name,
			Version:    b.Lang.Version,
			Entrypoint: b.Lang.Entrypoint,
			BuildArgs:  b.Lang.BuildArgs,
		}
	}

	if b.DependsOn != nil {
		s.DependsOn = &DependsOn{Assets: b.DependsOn.Assets}
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
	EnvFrom     []string          `hcl:"env_from,optional"`
	Volumes     []string          `hcl:"volumes,optional"`
	Network     string            `hcl:"network,optional"`
	Networks    []string          `hcl:"networks,optional"`
	NetworkMode string            `hcl:"network_mode,optional"`
	Timeout     string            `hcl:"timeout,optional"`

	Lang      *hclLangBlock `hcl:"lang,block"`
	DependsOn *hclDependsOn `hcl:"depends_on,block"`
}

func (b hclCronJob) spec() CronJobSpec {
	job := JobSpec{
		Image:       b.Image,
		Workdir:     b.Workdir,
		Dockerfile:  b.Dockerfile,
		Path:        b.Path,
		Command:     b.Command,
		Env:         b.Env,
		EnvFrom:     b.EnvFrom,
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

	if b.DependsOn != nil {
		job.DependsOn = &DependsOn{Assets: b.DependsOn.Assets}
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

// parseHCL is the dynamic-block-aware HCL parser. It iterates over
// every top-level block in the file via hclsyntax (to keep
// "unknown" block types around — that's what the plugin system
// rides on) and dispatches per block type:
//
//   - Core block types (app, deployment, statefulset, ingress,
//     job, cronjob): decoded with gohcl into the typed hcl* struct
//     for the kind, then encoded into the wire-shape Manifest.
//
//   - Anything else: treated as a plugin block. Its labels are
//     interpreted as (scope, name) — 0 labels means unscoped
//     singleton (Name = block type), 1 label means name-only,
//     2 labels means scope+name. Its body's attributes are walked
//     and emitted as a JSON spec. Nested blocks become arrays
//     keyed by block type. The server decides whether a plugin
//     is registered for the kind; the parser stays agnostic.
//
// The synthetic .hcl extension dance survives — gohcl's diagnostic
// formatting picks up the source name and underlines the offending
// expressions exactly like hclsimple did.
func parseHCL(source string, raw []byte) ([]controller.Manifest, error) {
	if strings.ToLower(filepath.Ext(source)) != ".hcl" {
		source = source + ".hcl"
	}

	parser := hclparse.NewParser()

	file, diags := parser.ParseHCL(raw, source)
	if diags.HasErrors() {
		return nil, diags
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("unexpected HCL backend %T (want *hclsyntax.Body)", file.Body)
	}

	var out []controller.Manifest

	for _, blk := range body.Blocks {
		mans, err := dispatchHCLBlock(blk, source)
		if err != nil {
			return nil, err
		}

		out = append(out, mans...)
	}

	if err := assertNoDuplicateIdentities(out); err != nil {
		return nil, err
	}

	return out, nil
}

// dispatchHCLBlock routes one top-level block to the right
// decoder. Apps fan out to (deployment, ingress) so the dedup
// check downstream catches collisions cleanly; everything else is
// 1:1 (one block → one manifest, except plugin blocks where the
// wire kind is the block type itself).
func dispatchHCLBlock(blk *hclsyntax.Block, source string) ([]controller.Manifest, error) {
	switch blk.Type {
	case "app":
		return decodeAppBlock(blk)

	case "deployment":
		scope, name, err := requireScopedLabels(blk)
		if err != nil {
			return nil, err
		}

		var d hclDeployment
		if d2 := gohcl.DecodeBody(blk.Body, nil, &d); d2.HasErrors() {
			return nil, d2
		}

		m, err := encode(controller.KindDeployment, scope, name, d.spec())
		if err != nil {
			return nil, err
		}

		return []controller.Manifest{m}, nil

	case "statefulset":
		scope, name, err := requireScopedLabels(blk)
		if err != nil {
			return nil, err
		}

		var s hclStatefulset
		if d := gohcl.DecodeBody(blk.Body, nil, &s); d.HasErrors() {
			return nil, d
		}

		m, err := encode(controller.KindStatefulset, scope, name, s.spec())
		if err != nil {
			return nil, err
		}

		return []controller.Manifest{m}, nil

	case "ingress":
		scope, name, err := requireScopedLabels(blk)
		if err != nil {
			return nil, err
		}

		var i hclIngress
		if d := gohcl.DecodeBody(blk.Body, nil, &i); d.HasErrors() {
			return nil, d
		}

		m, err := encode(controller.KindIngress, scope, name, i.spec())
		if err != nil {
			return nil, err
		}

		return []controller.Manifest{m}, nil

	case "job":
		scope, name, err := requireScopedLabels(blk)
		if err != nil {
			return nil, err
		}

		var j hclJob
		if d := gohcl.DecodeBody(blk.Body, nil, &j); d.HasErrors() {
			return nil, d
		}

		m, err := encode(controller.KindJob, scope, name, j.spec())
		if err != nil {
			return nil, err
		}

		return []controller.Manifest{m}, nil

	case "cronjob":
		scope, name, err := requireScopedLabels(blk)
		if err != nil {
			return nil, err
		}

		var c hclCronJob
		if d := gohcl.DecodeBody(blk.Body, nil, &c); d.HasErrors() {
			return nil, d
		}

		m, err := encode(controller.KindCronJob, scope, name, c.spec())
		if err != nil {
			return nil, err
		}

		return []controller.Manifest{m}, nil

	case "asset":
		return decodeAssetBlock(blk, source)

	default:
		return decodePluginBlock(blk)
	}
}

// decodeAssetBlock collects every body attribute as one entry in
// the asset's `files` map. Unlike the other core kinds, asset
// has NO typed schema — the body is a flat key-to-source mapping
// where the operator chooses both the key name and the source
// kind:
//
//	asset "clowk-lp" "redis" {
//	  configuration = file("./redis/redis.conf")
//	  acls          = url("https://r2.example.com/acl")
//	  motd          = "Welcome to production"
//	}
//
// The CLI's HCL eval context (M-C1) implements `file()` and
// `url()` as functions returning typed objects with a `_source`
// discriminator the server later inspects to choose the
// materialisation strategy.
//
// Nested blocks are not supported on assets — there's nothing
// to nest. Operators who try get a clear error.
func decodeAssetBlock(blk *hclsyntax.Block, source string) ([]controller.Manifest, error) {
	// Asset is OPTIONALLY scoped:
	//
	//   asset "name" { … }              → unscoped (global). Any
	//                                     resource on the host can
	//                                     reference via 3-segment
	//                                     `${asset.<name>.<key>}`.
	//
	//   asset "scope" "name" { … }      → scoped. Referenced via
	//                                     4-segment `${asset.<scope>.
	//                                     <name>.<key>}` from any
	//                                     scope (cross-scope OK).
	//
	// Any other label count is a clear authoring error.
	var scope, name string

	switch len(blk.Labels) {
	case 1:
		name = blk.Labels[0]
	case 2:
		scope, name = blk.Labels[0], blk.Labels[1]
	default:
		return nil, fmt.Errorf("asset block: takes 1 label (unscoped) or 2 labels (scoped), got %d", len(blk.Labels))
	}

	if name == "" {
		return nil, fmt.Errorf("asset block: name must be non-empty")
	}

	if len(blk.Body.Blocks) > 0 {
		return nil, fmt.Errorf("asset/%s/%s: nested blocks are not supported (asset bodies are flat key/source pairs)", scope, name)
	}

	files := make(map[string]any, len(blk.Body.Attributes))

	for k, attr := range blk.Body.Attributes {
		val, diags := attr.Expr.Value(assetEvalContext(source))
		if diags.HasErrors() {
			return nil, diags
		}

		files[k] = ctyValueToGo(val)
	}

	specJSON, err := json.Marshal(map[string]any{"files": files})
	if err != nil {
		return nil, err
	}

	return []controller.Manifest{
		{
			Kind:  controller.KindAsset,
			Scope: scope,
			Name:  name,
			Spec:  specJSON,
		},
	}, nil
}

// requireScopedLabels enforces the scoped-kind label contract: two
// labels exactly, neither empty. Plain `deployment "scope" "name"
// {}` lands here cleanly; missing labels (or an extra one) errors
// at parse time so the operator sees the mistake before the wire.
func requireScopedLabels(blk *hclsyntax.Block) (scope, name string, err error) {
	if len(blk.Labels) != 2 {
		return "", "", fmt.Errorf("%s block needs scope and name labels (got %d)", blk.Type, len(blk.Labels))
	}

	scope, name = blk.Labels[0], blk.Labels[1]

	if scope == "" || name == "" {
		return "", "", fmt.Errorf("%s block: scope and name must both be non-empty", blk.Type)
	}

	return scope, name, nil
}

// decodeAppBlock fans `app "scope" "name" { ... }` out into the
// canonical (deployment, ingress) pair. The pair shares the
// (scope, name); downstream dedup catches an `app` followed by a
// standalone `deployment` of the same identity.
func decodeAppBlock(blk *hclsyntax.Block) ([]controller.Manifest, error) {
	scope, name, err := requireScopedLabels(blk)
	if err != nil {
		return nil, err
	}

	var a hclApp
	if d := gohcl.DecodeBody(blk.Body, nil, &a); d.HasErrors() {
		return nil, d
	}

	dep, err := encode(controller.KindDeployment, scope, name, a.deploymentSpec())
	if err != nil {
		return nil, err
	}

	ing, err := encode(controller.KindIngress, scope, name, a.ingressSpec())
	if err != nil {
		return nil, err
	}

	return []controller.Manifest{dep, ing}, nil
}

// decodePluginBlock turns an unknown block (`postgres`, `redis`,
// `mysql`, …) into a Manifest with Kind = block type. Labels map
// to (scope, name) per the convention:
//
//	postgres { … }                  Scope="", Name="postgres" (singleton)
//	postgres "main" { … }           Scope="", Name="main"
//	postgres "data" "main" { … }    Scope="data", Name="main"
//
// Spec is the JSON of the block's attributes, plus nested blocks
// rolled up under their type as either an object (single
// occurrence) or array (multiple). The server-side plugin
// registry (M-D1+) decides whether a plugin handles this kind;
// the parser stays agnostic — operators can pre-write HCL for
// plugins they haven't installed yet.
func decodePluginBlock(blk *hclsyntax.Block) ([]controller.Manifest, error) {
	var scope, name string

	switch len(blk.Labels) {
	case 0:
		// Singleton — block type IS the resource name. Useful
		// for "global" plugins that only ever exist once on a
		// host (a redis cache, a single mongo).
		name = blk.Type
	case 1:
		name = blk.Labels[0]
	case 2:
		scope, name = blk.Labels[0], blk.Labels[1]
	default:
		return nil, fmt.Errorf("%s block: too many labels (max 2: scope and name)", blk.Type)
	}

	if name == "" {
		return nil, fmt.Errorf("%s block: name must be non-empty", blk.Type)
	}

	specJSON, err := bodyToJSON(blk.Body)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", blk.Type, name, err)
	}

	return []controller.Manifest{
		{
			Kind:  controller.Kind(blk.Type),
			Scope: scope,
			Name:  name,
			Spec:  specJSON,
		},
	}, nil
}

// bodyToJSON walks an hclsyntax.Body collecting attributes (cty
// values converted to Go primitives + JSON-marshalled) and nested
// blocks (recursed into the same shape). Single-occurrence nested
// blocks become an object; multi-occurrence become an array of
// objects under the block type key.
//
// EvalContext is nil — plugin blocks accept literal values only
// in M-D0d. ${VAR} interpolation already happened at the
// pre-parse stage (see Interpolate); HCL-side functions / refs
// land in M-D2 alongside the expand pipeline.
func bodyToJSON(body *hclsyntax.Body) (json.RawMessage, error) {
	out := make(map[string]any, len(body.Attributes)+len(body.Blocks))

	for k, attr := range body.Attributes {
		val, diags := attr.Expr.Value(nil)
		if diags.HasErrors() {
			return nil, diags
		}

		goVal := ctyValueToGo(val)

		// Env is the one attribute we coerce per-value: in the
		// real world env vars are always strings, but operators
		// reflexively write `MAX_CONNS = 100` or `DEBUG = true`
		// without quotes. Without coercion, the JSON serialises
		// as a number/bool, the consumer kind's `Env map[string]
		// string` decode silently fails on the type mismatch,
		// and the env never reaches the container — invisible
		// failure mode (no error surfaced; container just runs
		// with empty env). Stringify here so HCL ergonomics
		// match the wire contract.
		if k == "env" {
			goVal = stringifyEnvMap(goVal)
		}

		out[k] = goVal
	}

	// Nested blocks: collapse to object when single occurrence,
	// array of objects when multiple. This matches how operators
	// expect to write `postgres "main" { backup { schedule = "..." } }`
	// (one backup) vs `redis "cluster" { node {…} node {…} }`
	// (multiple nodes).
	nestedByType := make(map[string][]map[string]any, len(body.Blocks))

	for _, nested := range body.Blocks {
		nestedSpec, err := bodyToJSON(nested.Body)
		if err != nil {
			return nil, err
		}

		var asMap map[string]any

		if err := json.Unmarshal(nestedSpec, &asMap); err != nil {
			return nil, fmt.Errorf("nested block %s: %w", nested.Type, err)
		}

		// Labels on nested blocks (e.g. `volume_claim "data" {…}`)
		// land under a synthetic "_labels" key so the plugin can
		// recover them. Skipped when the nested block has no
		// labels.
		if len(nested.Labels) > 0 {
			labels := make([]any, 0, len(nested.Labels))
			for _, lbl := range nested.Labels {
				labels = append(labels, lbl)
			}

			asMap["_labels"] = labels
		}

		nestedByType[nested.Type] = append(nestedByType[nested.Type], asMap)
	}

	for typ, list := range nestedByType {
		if len(list) == 1 {
			out[typ] = list[0]
		} else {
			out[typ] = list
		}
	}

	return json.Marshal(out)
}

// stringifyEnvMap walks a parsed env attribute (after
// ctyValueToGo) and coerces every leaf value to a string.
// Number → strconv-formatted, bool → "true"/"false", string
// passes through. Non-string-coercible values (nested objects,
// lists) are left as-is — the wire contract for env is
// map[string]string, so a downstream decoder will surface them
// as errors loudly rather than silently dropping the whole map.
//
// Why coerce here instead of in ctyValueToGo: the broader
// converter is kind-agnostic. Env is the one place where the
// JSON wire shape must be string-valued no matter what the
// operator typed. Doing the coercion at the bodyToJSON entry
// point keeps the converter pure and the special case localised.
func stringifyEnvMap(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}

	out := make(map[string]any, len(m))

	for k, raw := range m {
		switch x := raw.(type) {
		case string:
			out[k] = x

		case bool:
			out[k] = strconv.FormatBool(x)

		case float64:
			// Render whole numbers without the `.0` suffix that
			// strconv would apply for FormatFloat — operators
			// expect `1` to render as `"1"`, not `"1.0"`.
			if x == float64(int64(x)) {
				out[k] = strconv.FormatInt(int64(x), 10)
			} else {
				out[k] = strconv.FormatFloat(x, 'g', -1, 64)
			}

		case nil:
			// HCL `K = null` → empty string. Matches shell
			// convention where `export K=` sets K to "".
			out[k] = ""

		default:
			// Nested objects / lists — leave verbatim so the
			// downstream JSON decoder fails loudly with a type
			// mismatch operator can act on. Pretending to
			// coerce a list to a string by JSON-encoding it
			// would mask an authoring mistake.
			out[k] = raw
		}
	}

	return out
}

// ctyValueToGo collapses a cty.Value into the Go-side any types
// that json.Marshal handles natively. cty's type system is richer
// than JSON (numbers stay arbitrary-precision until rendered;
// tuples/objects vs lists/maps), so we collapse to the simpler
// shape — float64 for numbers, []any for sequences, map[string]any
// for keyed collections. Null becomes nil.
//
// This is enough for declarative plugin configs (image strings,
// numeric replicas, env maps, lists of ports). Plugins that need
// HCL-side functions (fileexists(), etc.) will get a richer eval
// context in M-D2.
func ctyValueToGo(val cty.Value) any {
	if val.IsNull() {
		return nil
	}

	ty := val.Type()

	switch {
	case ty.Equals(cty.String):
		return val.AsString()

	case ty.Equals(cty.Bool):
		return val.True()

	case ty.Equals(cty.Number):
		f, _ := val.AsBigFloat().Float64()
		return f

	case ty.IsListType(), ty.IsTupleType(), ty.IsSetType():
		out := make([]any, 0, val.LengthInt())

		for it := val.ElementIterator(); it.Next(); {
			_, v := it.Element()
			out = append(out, ctyValueToGo(v))
		}

		return out

	case ty.IsMapType(), ty.IsObjectType():
		out := make(map[string]any, val.LengthInt())

		for it := val.ElementIterator(); it.Next(); {
			k, v := it.Element()
			out[k.AsString()] = ctyValueToGo(v)
		}

		return out
	}

	return nil
}

// _ keeps hcl.Diagnostics referenced from the gohcl path imported
// (gohcl.DecodeBody returns diagnostics directly) — needed because
// the import only otherwise appears in gohcl call sites.
var _ = hcl.DiagError

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

	case controller.KindStatefulset:
		var s StatefulsetSpec
		if err := node.Decode(&s); err != nil {
			return s, err
		}

		s.applyDefaults()

		return s, nil

	case controller.KindIngress:
		var s IngressSpec
		return s, node.Decode(&s)

	case controller.KindJob:
		var s JobSpec
		return s, node.Decode(&s)

	case controller.KindCronJob:
		var s CronJobSpec
		return s, node.Decode(&s)

	case controller.KindAsset:
		var s AssetSpec
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
