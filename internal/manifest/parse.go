package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

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

	// docker-compose-shaped pass-through knobs. See DeploymentSpec for
	// full docs — these surface the same fields at the HCL layer.
	ExtraHosts []string `hcl:"extra_hosts,optional"`
	CapAdd     []string `hcl:"cap_add,optional"`
	EnvFile    []string `hcl:"env_file,optional"`
	EnvFrom    []string `hcl:"env_from,optional"`

	Build          *hclBuildBlock          `hcl:"build,block"`
	Release        *hclReleaseBlock        `hcl:"release,block"`
	DependsOn      *hclDependsOn           `hcl:"depends_on,block"`
	Resources      *hclResourcesBlock      `hcl:"resources,block"`
	Autoscale      *hclAutoscaleBlock      `hcl:"autoscale,block"`
	OnDeploy       *hclOnDeployBlock       `hcl:"on_deploy,block"`
	Logs           *hclLogsBlock           `hcl:"logs,block"`
	Probes         *hclProbesBlock         `hcl:"probes,block"`
	InitContainers []hclInitContainerBlock `hcl:"init_container,block"`
}

// hclOnDeployBlock is the HCL surface for the on_deploy webhook
// hooks. Both fields are optional; an empty block is a no-op (no
// posts). See manifest.OnDeploySpec for the wire shape and delivery
// contract (best-effort, 3-attempt exponential backoff).
type hclOnDeployBlock struct {
	Success string `hcl:"success,optional"`
	Failure string `hcl:"failure,optional"`
}

// onDeployBlockToSpec converts the HCL surface into the wire
// OnDeploySpec. nil-in / nil-out so callers don't synthesise an
// empty block when the operator omitted on_deploy entirely.
func onDeployBlockToSpec(b *hclOnDeployBlock) *OnDeploySpec {
	if b == nil {
		return nil
	}

	return &OnDeploySpec{
		Success: b.Success,
		Failure: b.Failure,
	}
}

// hclLogsBlock is the HCL surface for the docker-log-driver cap.
// Both fields are optional at the HCL level; applyDefaults fills
// either-or-both with the platform 10m/3 default when omitted.
// See manifest.LogsSpec for the wire-shape counterpart and the
// rationale for always emitting a non-nil block.
type hclLogsBlock struct {
	MaxSize  string `hcl:"max_size,optional"`
	MaxFiles int    `hcl:"max_files,optional"`
}

// logsBlockToSpec converts the HCL surface into the wire LogsSpec.
// nil-in / nil-out — applyLogsDefaults synthesises a fresh LogsSpec
// downstream when the operator omitted the block, so callers don't
// need to fabricate one here.
func logsBlockToSpec(b *hclLogsBlock) *LogsSpec {
	if b == nil {
		return nil
	}

	return &LogsSpec{
		MaxSize:  b.MaxSize,
		MaxFiles: b.MaxFiles,
	}
}

// hclProbesBlock is the HCL surface for the kubelet-style health
// checks. Three sub-blocks (liveness, readiness, startup), each an
// independent `hclProbeBlock`. Operators omit the ones they don't
// want. See manifest.ProbesSpec for the operator-facing contract.
type hclProbesBlock struct {
	Liveness  *hclProbeBlock `hcl:"liveness,block"`
	Readiness *hclProbeBlock `hcl:"readiness,block"`
	Startup   *hclProbeBlock `hcl:"startup,block"`
}

// hclProbeBlock is one probe's full HCL surface. The three action
// sub-blocks (http_get / tcp_socket / exec) are mutually exclusive
// — exactly one must be declared. Validation runs at conversion
// time via probe.Spec.Validate(), so the operator gets a parse-time
// error rather than a runtime probe-loop misbehaviour.
type hclProbeBlock struct {
	HTTPGet   *hclHTTPGetAction   `hcl:"http_get,block"`
	TCPSocket *hclTCPSocketAction `hcl:"tcp_socket,block"`
	Exec      *hclExecAction      `hcl:"exec,block"`

	InitialDelay     string `hcl:"initial_delay,optional"`
	Period           string `hcl:"period,optional"`
	Timeout          string `hcl:"timeout,optional"`
	FailureThreshold int    `hcl:"failure_threshold,optional"`
	SuccessThreshold int    `hcl:"success_threshold,optional"`
}

type hclHTTPGetAction struct {
	Path        string            `hcl:"path"`
	Port        int               `hcl:"port"`
	Scheme      string            `hcl:"scheme,optional"`
	HTTPHeaders map[string]string `hcl:"http_headers,optional"`
}

type hclTCPSocketAction struct {
	Port int `hcl:"port"`
}

type hclExecAction struct {
	Command []string `hcl:"command"`
}

// probesBlockToSpec converts the HCL surface into the wire ProbesSpec.
// nil-in / nil-out so callers don't synthesise an empty block when
// the operator omitted probes entirely.
func probesBlockToSpec(b *hclProbesBlock) *ProbesSpec {
	if b == nil {
		return nil
	}

	out := &ProbesSpec{}

	if b.Liveness != nil {
		out.Liveness = probeBlockToSpec(b.Liveness)
	}

	if b.Readiness != nil {
		out.Readiness = probeBlockToSpec(b.Readiness)
	}

	if b.Startup != nil {
		out.Startup = probeBlockToSpec(b.Startup)
	}

	return out
}

func probeBlockToSpec(b *hclProbeBlock) *ProbeSpec {
	if b == nil {
		return nil
	}

	out := &ProbeSpec{
		InitialDelay:     b.InitialDelay,
		Period:           b.Period,
		Timeout:          b.Timeout,
		FailureThreshold: b.FailureThreshold,
		SuccessThreshold: b.SuccessThreshold,
	}

	if b.HTTPGet != nil {
		out.HTTPGet = &HTTPGetAction{
			Path:        b.HTTPGet.Path,
			Port:        b.HTTPGet.Port,
			Scheme:      b.HTTPGet.Scheme,
			HTTPHeaders: b.HTTPGet.HTTPHeaders,
		}
	}

	if b.TCPSocket != nil {
		out.TCPSocket = &TCPSocketAction{Port: b.TCPSocket.Port}
	}

	if b.Exec != nil {
		out.Exec = &ExecAction{Command: b.Exec.Command}
	}

	return out
}

// validateProbes runs the same "exactly one selector" rule the
// probe package's Spec.Validate() uses. Done here so a malformed
// manifest fails at apply time with a clear error rather than at
// runtime when the probe runner starts. Each error names the
// triggering probe (liveness/readiness/startup) so the operator
// can find the bad block fast.
func validateProbes(p *ProbesSpec) error {
	if p == nil {
		return nil
	}

	if err := validateProbeShape(p.Liveness, "liveness"); err != nil {
		return err
	}

	if err := validateProbeShape(p.Readiness, "readiness"); err != nil {
		return err
	}

	if err := validateProbeShape(p.Startup, "startup"); err != nil {
		return err
	}

	return nil
}

func validateProbeShape(p *ProbeSpec, kind string) error {
	if p == nil {
		return nil
	}

	count := 0
	if p.HTTPGet != nil {
		count++

		if p.HTTPGet.Path == "" {
			return fmt.Errorf("probes.%s.http_get.path is required", kind)
		}

		if !strings.HasPrefix(p.HTTPGet.Path, "/") {
			return fmt.Errorf("probes.%s.http_get.path must start with '/'", kind)
		}

		if p.HTTPGet.Port <= 0 {
			return fmt.Errorf("probes.%s.http_get.port is required (got %d)", kind, p.HTTPGet.Port)
		}
	}

	if p.TCPSocket != nil {
		count++

		if p.TCPSocket.Port <= 0 {
			return fmt.Errorf("probes.%s.tcp_socket.port is required (got %d)", kind, p.TCPSocket.Port)
		}
	}

	if p.Exec != nil {
		count++

		if len(p.Exec.Command) == 0 {
			return fmt.Errorf("probes.%s.exec.command must be non-empty", kind)
		}
	}

	switch count {
	case 0:
		return fmt.Errorf("probes.%s requires exactly one of http_get / tcp_socket / exec", kind)
	case 1:
		// ok
	default:
		return fmt.Errorf("probes.%s: only one of http_get / tcp_socket / exec may be declared", kind)
	}

	return nil
}

// hclInitContainerBlock is the HCL surface for one init container.
// Block label is the init's name (becomes part of the docker
// container name suffix). See manifest.InitContainerSpec for the
// inheritance rules — most fields are optional because they
// default to the parent deployment's values.
type hclInitContainerBlock struct {
	Name      string             `hcl:"name,label"`
	Image     string             `hcl:"image,optional"`
	Command   []string           `hcl:"command,optional"`
	Timeout   string             `hcl:"timeout,optional"`
	Retries   int                `hcl:"retries,optional"`
	Resources *hclResourcesBlock `hcl:"resources,block"`
}

// initContainerBlocksToSpec converts the HCL block list into the
// wire-shape []InitContainerSpec, preserving declaration order
// (execution order is semantic — k8s init containers run
// sequentially top-to-bottom).
//
// Empty input → nil so callers can distinguish "operator declared
// no inits" from "operator declared an empty list" in describe
// output (the latter is impossible in HCL anyway, but the nil-vs-
// empty-slice difference matters for json round-trips).
func initContainerBlocksToSpec(blocks []hclInitContainerBlock) []InitContainerSpec {
	if len(blocks) == 0 {
		return nil
	}

	out := make([]InitContainerSpec, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, InitContainerSpec{
			Name:      b.Name,
			Image:     b.Image,
			Command:   b.Command,
			Timeout:   b.Timeout,
			Retries:   b.Retries,
			Resources: resourcesBlockToSpec(b.Resources),
		})
	}

	return out
}

// validateInitContainers enforces parse-time invariants the HCL
// shape can't express:
//
//   - Name non-empty + charset [a-z0-9-] + starts with letter/digit
//     (mirrors resource name rules; the value flows into a docker
//     container name segment)
//   - Names are unique within the deployment's init list
//   - Command non-empty (an init with no command would just run
//     the image's CMD — almost always a misconfig)
//   - Retries in [0, 5] (5+ is a chronic-failure-loop antipattern)
//   - Timeout parseable (empty / valid time.ParseDuration; an
//     unparseable value would surface only at runtime when the
//     init goroutine tried to honour it)
//
// `kind` is "deployment" / "statefulset" / "app" — flows into
// the error message so the operator can find the offending block.
func validateInitContainers(inits []InitContainerSpec, kind string) error {
	if len(inits) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(inits))

	for i, ic := range inits {
		if ic.Name == "" {
			return fmt.Errorf("%s: init_container[%d] missing name", kind, i)
		}

		if !initContainerNameRE.MatchString(ic.Name) {
			return fmt.Errorf("%s: init_container %q invalid name (must match [a-z0-9][a-z0-9-]*)", kind, ic.Name)
		}

		if _, dup := seen[ic.Name]; dup {
			return fmt.Errorf("%s: duplicate init_container name %q", kind, ic.Name)
		}

		seen[ic.Name] = struct{}{}

		if len(ic.Command) == 0 {
			return fmt.Errorf("%s: init_container %q requires command", kind, ic.Name)
		}

		if ic.Retries < 0 || ic.Retries > 5 {
			return fmt.Errorf("%s: init_container %q retries must be in [0, 5] (got %d)", kind, ic.Name, ic.Retries)
		}

		if ic.Timeout != "" {
			if _, err := time.ParseDuration(ic.Timeout); err != nil {
				return fmt.Errorf("%s: init_container %q invalid timeout %q: %w", kind, ic.Name, ic.Timeout, err)
			}
		}
	}

	return nil
}

// initContainerNameRE matches a valid init container name. Same
// shape as resource names — lowercase, digits, hyphens; must start
// with a letter/digit. The value flows into a docker container
// name segment so we mirror docker's name rules.
var initContainerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// hclAutoscaleBlock is the HCL surface for the M7 CPU-based
// horizontal autoscale block on deployments. See manifest.AutoscaleSpec
// for the operator-facing contract and the decision-band rationale.
//
// All fields are HCL-optional; semantic validation (min >= 1,
// max >= min, cpu_target in (0,100], mutex against `replicas`)
// lands in the per-block spec() conversion below so error messages
// can name the surrounding deployment.
type hclAutoscaleBlock struct {
	Min          int    `hcl:"min,optional"`
	Max          int    `hcl:"max,optional"`
	CPUTarget    int    `hcl:"cpu_target,optional"`
	CooldownUp   string `hcl:"cooldown_up,optional"`
	CooldownDown string `hcl:"cooldown_down,optional"`
}

// autoscaleBlockToSpec converts the HCL surface into the wire-shape
// AutoscaleSpec. nil-in / nil-out so callers don't synthesise an empty
// block when the operator omitted autoscale entirely.
func autoscaleBlockToSpec(b *hclAutoscaleBlock) *AutoscaleSpec {
	if b == nil {
		return nil
	}

	return &AutoscaleSpec{
		Min:          b.Min,
		Max:          b.Max,
		CPUTarget:    b.CPUTarget,
		CooldownUp:   b.CooldownUp,
		CooldownDown: b.CooldownDown,
	}
}

// validateAutoscale runs the semantic checks the HCL surface can't
// express (numeric ranges, mutual exclusivity with replicas). Called
// from every per-kind spec() that supports autoscale so the same error
// shape surfaces for `deployment` and `app` blocks.
func validateAutoscale(as *AutoscaleSpec, replicas int) error {
	if as == nil {
		return nil
	}

	if replicas > 0 {
		return errAutoscaleReplicasMix
	}

	if as.Min < 1 {
		return fmt.Errorf("autoscale.min must be >= 1 (got %d)", as.Min)
	}

	if as.Max < as.Min {
		return fmt.Errorf("autoscale.max (%d) must be >= autoscale.min (%d)", as.Max, as.Min)
	}

	if as.CPUTarget <= 0 || as.CPUTarget > 100 {
		return fmt.Errorf("autoscale.cpu_target must be in (0, 100], got %d", as.CPUTarget)
	}

	return nil
}

// errAutoscaleReplicasMix surfaces when both `replicas = N` and an
// `autoscale {}` block are declared on the same deployment. The two
// are by definition exclusive: static pinning vs delegated control.
// Allowing both would either silently override one or freeze the
// autoscaler at the operator's count — either way a footgun.
var errAutoscaleReplicasMix = fmt.Errorf("replicas and autoscale are mutually exclusive — use min/max instead")

// hclBuildBlock is the HCL surface for the docker-compose-shaped
// `build { ... }` block. Lives on deployment/statefulset/job/cronjob/
// app, mutually exclusive with `image = "..."` (parse-time validated).
// See manifest.BuildSpec for the field contract.
//
// Lang is nested HERE (not at the resource root) because it's
// strictly a build-time concern — `name = "go"` picks the build
// handler, `version` flows into the generated Dockerfile, etc.
// Putting it inside build {} keeps each section internally coherent
// (root = runtime, build {} = build-time).
type hclBuildBlock struct {
	Context    string            `hcl:"context,optional"`
	Dockerfile string            `hcl:"dockerfile,optional"`
	Path       string            `hcl:"path,optional"`
	Args       map[string]string `hcl:"args,optional"`

	Lang *hclLangBlock `hcl:"lang,block"`
}

// buildBlockToSpec converts the HCL surface into the wire BuildSpec.
// nil-in / nil-out so callers don't synthesise an empty block when
// the operator omitted `build {}` entirely. applyDefaults handles
// the "implicit build mode at repo root" case downstream — see
// DeploymentSpec.applyDefaults for the synthesis path.
func buildBlockToSpec(b *hclBuildBlock) *BuildSpec {
	if b == nil {
		return nil
	}

	out := &BuildSpec{
		Context:    b.Context,
		Dockerfile: b.Dockerfile,
		Path:       b.Path,
		Args:       b.Args,
	}

	if b.Lang != nil {
		out.Lang = &LangSpec{
			Name:       b.Lang.Name,
			Version:    b.Lang.Version,
			Entrypoint: b.Lang.Entrypoint,
		}
	}

	return out
}

// hclDependsOn is the HCL surface for explicit dependencies. The
// only sub-attribute today is `assets` — a list of ref strings the
// consumer depends on. See manifest.DependsOn for the wire-shape
// counterpart and the rationale for declaring deps that aren't
// visible as textual `${asset.…}` refs.
type hclDependsOn struct {
	Assets []string `hcl:"assets,optional"`
}

// hclResourcesBlock is the HCL surface for `resources { limits {…} }`.
// Mirrors k8s shape; voodu currently honours only the `limits` half
// (kernel cap via cgroups → docker run --cpus / --memory). The
// `requests` half is reserved for a future multi-host scheduler.
type hclResourcesBlock struct {
	Limits *hclResourceLimits `hcl:"limits,block"`
}

type hclResourceLimits struct {
	CPU    string `hcl:"cpu,optional"`
	Memory string `hcl:"memory,optional"`
}

// resourcesBlockToSpec converts the HCL surface into the wire-shape
// ResourcesSpec. nil-in / nil-out so callers don't synthesise an
// empty block when the operator omitted resources entirely.
func resourcesBlockToSpec(b *hclResourcesBlock) *ResourcesSpec {
	if b == nil {
		return nil
	}

	out := &ResourcesSpec{}

	if b.Limits != nil {
		out.Limits = &ResourceLimits{
			CPU:    b.Limits.CPU,
			Memory: b.Limits.Memory,
		}
	}

	return out
}

// hclLangBlock is the HCL surface for the nested `build { lang {...} }`
// runtime hint. Lives inside hclBuildBlock (lang is a build-time
// concern, not a runtime one). Fields mirror manifest.LangSpec — see
// there for the operator-facing contract. Build args live on the
// parent build block (`build.args = {...}`), not here.
type hclLangBlock struct {
	Name       string `hcl:"name,optional"`
	Version    string `hcl:"version,optional"`
	Entrypoint string `hcl:"entrypoint,optional"`
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

func (b hclDeployment) spec() (DeploymentSpec, error) {
	if b.Image != "" && b.Build != nil {
		return DeploymentSpec{}, errImageBuildExclusive
	}

	s := DeploymentSpec{
		Image:        b.Image,
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
		ExtraHosts:   b.ExtraHosts,
		CapAdd:       b.CapAdd,
		EnvFile:      b.EnvFile,
		EnvFrom:      b.EnvFrom,
		Build:        buildBlockToSpec(b.Build),
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

	s.Resources = resourcesBlockToSpec(b.Resources)
	s.Autoscale = autoscaleBlockToSpec(b.Autoscale)

	if err := validateAutoscale(s.Autoscale, s.Replicas); err != nil {
		return DeploymentSpec{}, err
	}

	s.OnDeploy = onDeployBlockToSpec(b.OnDeploy)
	s.Logs = logsBlockToSpec(b.Logs)
	s.Probes = probesBlockToSpec(b.Probes)

	if err := validateProbes(s.Probes); err != nil {
		return DeploymentSpec{}, err
	}

	s.InitContainers = initContainerBlocksToSpec(b.InitContainers)

	if err := validateInitContainers(s.InitContainers, "deployment"); err != nil {
		return DeploymentSpec{}, err
	}

	s.applyDefaults()

	return s, nil
}

// errImageBuildExclusive surfaces when an operator declares both
// `image = "..."` AND a `build {}` block on the same resource. The
// pipeline can't honour both: image-mode pulls from registry,
// build-mode builds from source. Operator picks one.
var errImageBuildExclusive = fmt.Errorf("`image` and `build {}` are mutually exclusive: use `image` to pull from a registry, or `build {}` to build from source — not both")

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

	// docker-compose-shaped pass-through knobs. Same semantics as
	// hclDeployment — see DeploymentSpec for full docs.
	ExtraHosts []string `hcl:"extra_hosts,optional"`
	CapAdd     []string `hcl:"cap_add,optional"`
	EnvFile    []string `hcl:"env_file,optional"`
	EnvFrom    []string `hcl:"env_from,optional"`

	Build          *hclBuildBlock          `hcl:"build,block"`
	Release        *hclReleaseBlock        `hcl:"release,block"`
	DependsOn      *hclDependsOn           `hcl:"depends_on,block"`
	Resources      *hclResourcesBlock      `hcl:"resources,block"`
	Autoscale      *hclAutoscaleBlock      `hcl:"autoscale,block"`
	OnDeploy       *hclOnDeployBlock       `hcl:"on_deploy,block"`
	Logs           *hclLogsBlock           `hcl:"logs,block"`
	Probes         *hclProbesBlock         `hcl:"probes,block"`
	InitContainers []hclInitContainerBlock `hcl:"init_container,block"`

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
func (b hclApp) deploymentSpec() (DeploymentSpec, error) {
	if b.Image != "" && b.Build != nil {
		return DeploymentSpec{}, errImageBuildExclusive
	}

	s := DeploymentSpec{
		Image:        b.Image,
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
		ExtraHosts:   b.ExtraHosts,
		CapAdd:       b.CapAdd,
		EnvFile:      b.EnvFile,
		EnvFrom:      b.EnvFrom,
		Build:        buildBlockToSpec(b.Build),
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

	// `app` carries the same `resources { limits { ... } }` block as
	// a standalone `deployment`, with identical kernel-cap semantics
	// (--cpus / --memory at docker run). Operators can budget CPU
	// and memory directly in the authoring-sugar form without
	// dropping back to the verbose deployment+ingress pair.
	s.Resources = resourcesBlockToSpec(b.Resources)
	s.Autoscale = autoscaleBlockToSpec(b.Autoscale)

	if err := validateAutoscale(s.Autoscale, s.Replicas); err != nil {
		return DeploymentSpec{}, err
	}

	s.OnDeploy = onDeployBlockToSpec(b.OnDeploy)
	s.Logs = logsBlockToSpec(b.Logs)
	s.Probes = probesBlockToSpec(b.Probes)

	if err := validateProbes(s.Probes); err != nil {
		return DeploymentSpec{}, err
	}

	s.InitContainers = initContainerBlocksToSpec(b.InitContainers)

	if err := validateInitContainers(s.InitContainers, "app"); err != nil {
		return DeploymentSpec{}, err
	}

	s.applyDefaults()

	return s, nil
}

// ingressSpec extracts the ingress half. Service is left empty so
// the controller's default-service-to-name path lands on the
// deployment we just emitted alongside. Port stays zero so the
// deployment's ports[0] is auto-picked at reconcile.
func (b hclApp) ingressSpec() IngressSpec {
	out := IngressSpec{Host: b.Host}

	out.TLS = tlsBlockToSpec(b.TLS)

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

	// docker-compose-shaped pass-through knobs. Same semantics as
	// hclDeployment — see DeploymentSpec for full docs.
	ExtraHosts []string `hcl:"extra_hosts,optional"`
	CapAdd     []string `hcl:"cap_add,optional"`
	EnvFile    []string `hcl:"env_file,optional"`

	Build          *hclBuildBlock          `hcl:"build,block"`
	VolumeClaims   []hclVolumeClaim        `hcl:"volume_claim,block"`
	DependsOn      *hclDependsOn           `hcl:"depends_on,block"`
	Resources      *hclResourcesBlock      `hcl:"resources,block"`
	Logs           *hclLogsBlock           `hcl:"logs,block"`
	InitContainers []hclInitContainerBlock `hcl:"init_container,block"`
	Probes         *hclProbesBlock         `hcl:"probes,block"`
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

func (b hclStatefulset) spec() (StatefulsetSpec, error) {
	if b.Image != "" && b.Build != nil {
		return StatefulsetSpec{}, errImageBuildExclusive
	}

	s := StatefulsetSpec{
		Image:       b.Image,
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
		ExtraHosts:  b.ExtraHosts,
		CapAdd:      b.CapAdd,
		EnvFile:     b.EnvFile,
		Build:       buildBlockToSpec(b.Build),
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

	s.Resources = resourcesBlockToSpec(b.Resources)
	s.Logs = logsBlockToSpec(b.Logs)

	s.InitContainers = initContainerBlocksToSpec(b.InitContainers)

	if err := validateInitContainers(s.InitContainers, "statefulset"); err != nil {
		return StatefulsetSpec{}, err
	}

	s.Probes = probesBlockToSpec(b.Probes)

	if err := validateProbes(s.Probes); err != nil {
		return StatefulsetSpec{}, err
	}

	s.applyDefaults()

	return s, nil
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

// hclIngressTLS surfaces the `tls {}` block in HCL. Enabled is a
// *bool (not bool) so the parser can tell "operator omitted the
// field" (nil) from "operator wrote `enabled = false`" (pointer to
// false). Declaring `tls {}` at all is the strong signal of intent
// — applyDefaults flips a nil Enabled to true, but an explicit
// `enabled = false` is honoured verbatim for the rare case the
// operator wants to keep the block around (with email / provider /
// etc.) but temporarily disable TLS issuance.
type hclIngressTLS struct {
	Enabled  *bool  `hcl:"enabled,optional"`
	Provider string `hcl:"provider,optional"`
	Email    string `hcl:"email,optional"`
	OnDemand bool   `hcl:"on_demand,optional"`
	Ask      string `hcl:"ask,optional"`
}

// tlsBlockToSpec converts the HCL surface into the wire-shape
// IngressTLS, applying the "block-present = TLS on" defaults:
//
//   - Enabled: nil → true. Operator declared `tls {}`, they want
//     TLS. An explicit `enabled = false` is honoured (escape hatch
//     for keeping the block declared while toggling TLS off).
//   - Provider: empty → "letsencrypt". The overwhelmingly common
//     case; operators who want voodu-internal certs (dev/staging)
//     or another ACME provider override explicitly.
//
// nil-in / nil-out so callers know whether the operator declared
// the block at all (no block = no TLS wire spec at all).
func tlsBlockToSpec(b *hclIngressTLS) *IngressTLS {
	if b == nil {
		return nil
	}

	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}

	provider := b.Provider
	if provider == "" {
		provider = "letsencrypt"
	}

	return &IngressTLS{
		Enabled:  enabled,
		Provider: provider,
		Email:    b.Email,
		OnDemand: b.OnDemand,
		Ask:      b.Ask,
	}
}

type hclIngressLocation struct {
	Path  string `hcl:"path"`
	Strip bool   `hcl:"strip,optional"`
}

func (b hclIngress) spec() IngressSpec {
	out := IngressSpec{Host: b.Host, Service: b.Service, Port: b.Port}

	out.TLS = tlsBlockToSpec(b.TLS)

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
	Command     []string          `hcl:"command,optional"`
	Env         map[string]string `hcl:"env,optional"`
	EnvFrom     []string          `hcl:"env_from,optional"`
	Volumes     []string          `hcl:"volumes,optional"`
	Network     string            `hcl:"network,optional"`
	Networks    []string          `hcl:"networks,optional"`
	NetworkMode string            `hcl:"network_mode,optional"`
	Timeout     string            `hcl:"timeout,optional"`

	// docker-compose-shaped pass-through knobs. Same semantics as
	// hclDeployment — see DeploymentSpec for full docs.
	ExtraHosts []string `hcl:"extra_hosts,optional"`
	CapAdd     []string `hcl:"cap_add,optional"`
	EnvFile    []string `hcl:"env_file,optional"`

	SuccessfulHistoryLimit int `hcl:"successful_history_limit,optional"`
	FailedHistoryLimit     int `hcl:"failed_history_limit,optional"`

	Build     *hclBuildBlock     `hcl:"build,block"`
	DependsOn *hclDependsOn      `hcl:"depends_on,block"`
	Resources *hclResourcesBlock `hcl:"resources,block"`
	Logs      *hclLogsBlock      `hcl:"logs,block"`
}

func (b hclJob) spec() (JobSpec, error) {
	if b.Image != "" && b.Build != nil {
		return JobSpec{}, errImageBuildExclusive
	}

	s := JobSpec{
		Image:                  b.Image,
		Command:                b.Command,
		Env:                    b.Env,
		EnvFrom:                b.EnvFrom,
		Volumes:                b.Volumes,
		Network:                b.Network,
		Networks:               b.Networks,
		NetworkMode:            b.NetworkMode,
		Timeout:                b.Timeout,
		ExtraHosts:             b.ExtraHosts,
		CapAdd:                 b.CapAdd,
		EnvFile:                b.EnvFile,
		Build:                  buildBlockToSpec(b.Build),
		SuccessfulHistoryLimit: b.SuccessfulHistoryLimit,
		FailedHistoryLimit:     b.FailedHistoryLimit,
	}

	if b.DependsOn != nil {
		s.DependsOn = &DependsOn{Assets: b.DependsOn.Assets}
	}

	s.Resources = resourcesBlockToSpec(b.Resources)
	s.Logs = logsBlockToSpec(b.Logs)

	s.applyDefaults()

	return s, nil
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
	Command     []string          `hcl:"command,optional"`
	Env         map[string]string `hcl:"env,optional"`
	EnvFrom     []string          `hcl:"env_from,optional"`
	Volumes     []string          `hcl:"volumes,optional"`
	Network     string            `hcl:"network,optional"`
	Networks    []string          `hcl:"networks,optional"`
	NetworkMode string            `hcl:"network_mode,optional"`
	Timeout     string            `hcl:"timeout,optional"`

	// docker-compose-shaped pass-through knobs. Same semantics as
	// hclDeployment — see DeploymentSpec for full docs.
	ExtraHosts []string `hcl:"extra_hosts,optional"`
	CapAdd     []string `hcl:"cap_add,optional"`
	EnvFile    []string `hcl:"env_file,optional"`

	Build     *hclBuildBlock     `hcl:"build,block"`
	DependsOn *hclDependsOn      `hcl:"depends_on,block"`
	Resources *hclResourcesBlock `hcl:"resources,block"`
	Logs      *hclLogsBlock      `hcl:"logs,block"`
}

func (b hclCronJob) spec() (CronJobSpec, error) {
	if b.Image != "" && b.Build != nil {
		return CronJobSpec{}, errImageBuildExclusive
	}

	job := JobSpec{
		Image:       b.Image,
		Command:     b.Command,
		Env:         b.Env,
		EnvFrom:     b.EnvFrom,
		Volumes:     b.Volumes,
		Network:     b.Network,
		Networks:    b.Networks,
		NetworkMode: b.NetworkMode,
		Timeout:     b.Timeout,
		ExtraHosts:  b.ExtraHosts,
		CapAdd:      b.CapAdd,
		EnvFile:     b.EnvFile,
		Build:       buildBlockToSpec(b.Build),
	}

	if b.DependsOn != nil {
		job.DependsOn = &DependsOn{Assets: b.DependsOn.Assets}
	}

	job.Resources = resourcesBlockToSpec(b.Resources)
	job.Logs = logsBlockToSpec(b.Logs)

	job.applyDefaults()

	return CronJobSpec{
		Schedule:               b.Schedule,
		Job:                    job,
		ConcurrencyPolicy:      b.ConcurrencyPolicy,
		Timezone:               b.Timezone,
		Suspend:                b.Suspend,
		SuccessfulHistoryLimit: b.SuccessfulHistoryLimit,
		FailedHistoryLimit:     b.FailedHistoryLimit,
	}, nil
}

// hclRegistry is the HCL surface for the private-registry pull-
// secret block. One label (the registry's name); url/username/token
// all required — see manifest.RegistrySpec for the operator-facing
// contract and the rationale for the singleton-per-host shape.
//
// `Password` is the alias path: operators reflexively write
// `password = "..."` when copying from older docker / ECR examples,
// and forcing them to translate it to `token` is busywork that
// scares off the on-ramp. The spec() conversion below picks
// whichever of the two is non-empty (with the explicit Token field
// winning if both happen to be set — see spec()).
//
// All fields are HCL-optional rather than required because gohcl
// surfaces missing-required-attribute errors with a generic
// diagnostic that doesn't mention the kind. We declare them
// optional here and validate explicitly in spec() so the error
// message reads `registry/ghcr: url is required` instead of the
// HCL parser's generic "The argument is required".
type hclRegistry struct {
	URL      string `hcl:"url,optional"`
	Username string `hcl:"username,optional"`
	Token    string `hcl:"token,optional"`
	Password string `hcl:"password,optional"`
}

// spec validates the required field set and folds the password
// alias into Token. Returns a typed error per missing field so
// the operator sees exactly what to add — `registry/ghcr: url is
// required` is a one-step fix; the gohcl-default "Missing required
// argument" is two steps (operator has to guess which kind the
// surface belongs to first).
func (b hclRegistry) spec() (RegistrySpec, error) {
	token := b.Token
	if token == "" {
		token = b.Password
	}

	if b.URL == "" {
		return RegistrySpec{}, fmt.Errorf("url is required")
	}

	if b.Username == "" {
		return RegistrySpec{}, fmt.Errorf("username is required")
	}

	if token == "" {
		return RegistrySpec{}, fmt.Errorf("token (or password) is required")
	}

	return RegistrySpec{
		URL:      b.URL,
		Username: b.Username,
		Token:    token,
	}, nil
}

// requireSingleLabel is the unscoped, one-label counterpart of
// requireScopedLabels. Registry blocks carry just their name as
// the single label (`registry "ghcr"`) — there's no scope segment
// because the underlying ~/.docker/config.json is global per host.
func requireSingleLabel(blk *hclsyntax.Block) (name string, err error) {
	if len(blk.Labels) != 1 {
		return "", fmt.Errorf("%s block needs exactly one label (the registry name), got %d", blk.Type, len(blk.Labels))
	}

	if blk.Labels[0] == "" {
		return "", fmt.Errorf("%s block: name label must be non-empty", blk.Type)
	}

	return blk.Labels[0], nil
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

		spec, err := d.spec()
		if err != nil {
			return nil, fmt.Errorf("deployment/%s/%s: %w", scope, name, err)
		}

		m, err := encode(controller.KindDeployment, scope, name, spec)
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

		spec, err := s.spec()
		if err != nil {
			return nil, fmt.Errorf("statefulset/%s/%s: %w", scope, name, err)
		}

		m, err := encode(controller.KindStatefulset, scope, name, spec)
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

		spec, err := j.spec()
		if err != nil {
			return nil, fmt.Errorf("job/%s/%s: %w", scope, name, err)
		}

		m, err := encode(controller.KindJob, scope, name, spec)
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

		spec, err := c.spec()
		if err != nil {
			return nil, fmt.Errorf("cronjob/%s/%s: %w", scope, name, err)
		}

		m, err := encode(controller.KindCronJob, scope, name, spec)
		if err != nil {
			return nil, err
		}

		return []controller.Manifest{m}, nil

	case "asset":
		return decodeAssetBlock(blk, source)

	case "registry":
		return decodeRegistryBlock(blk)

	default:
		return decodePluginBlock(blk)
	}
}

// decodeRegistryBlock turns `registry "ghcr" { url = ..., username
// = ..., token = ... }` into a single Manifest with Kind=registry,
// no scope, Name=label. See manifest.RegistrySpec for the wire-
// shape contract; the parser only enforces label arity and
// required-field presence (via hclRegistry.spec).
func decodeRegistryBlock(blk *hclsyntax.Block) ([]controller.Manifest, error) {
	name, err := requireSingleLabel(blk)
	if err != nil {
		return nil, err
	}

	var r hclRegistry
	if d := gohcl.DecodeBody(blk.Body, nil, &r); d.HasErrors() {
		return nil, d
	}

	spec, err := r.spec()
	if err != nil {
		return nil, fmt.Errorf("registry/%s: %w", name, err)
	}

	m, err := encode(controller.KindRegistry, "", name, spec)
	if err != nil {
		return nil, err
	}

	return []controller.Manifest{m}, nil
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

	depSpec, err := a.deploymentSpec()
	if err != nil {
		return nil, fmt.Errorf("app/%s/%s: %w", scope, name, err)
	}

	dep, err := encode(controller.KindDeployment, scope, name, depSpec)
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

	case controller.KindRegistry:
		var s RegistrySpec
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
