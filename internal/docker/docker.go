// Package docker wraps the `docker` CLI invocations voodu uses for
// build, deploy and container lifecycle management. Single-binary
// wrappers over `docker run / ps / inspect / logs / exec` so the
// rest of the codebase stays out of shell quoting and exit-code
// translation.
package docker

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

const (
	VooduLabelKey   = "createdby"
	VooduLabelValue = "voodu"
)

// GetVooduLabels returns the docker --label arguments that mark a container
// as being managed by Voodu.
func GetVooduLabels() []string {
	return []string{fmt.Sprintf("%s=%s", VooduLabelKey, VooduLabelValue)}
}

// ContainerInfo represents information about a running container.
// Mirrors the columns `docker ps --format json` emits so callers
// can decode the daemon's output directly.
type ContainerInfo struct {
	ID      string `json:"ID"`
	Names   string `json:"Names"`
	Image   string `json:"Image"`
	Status  string `json:"Status"`
	Ports   string `json:"Ports"`
	Command string `json:"Command"`
	Created string `json:"CreatedAt"`

	Name         string `json:"name"`
	AppName      string `json:"app_name"`
	ProcessType  string `json:"process_type"`
	Number       int    `json:"number"`
	HostPort     int    `json:"host_port"`
	InternalPort int    `json:"internal_port"`
	CreatedAt    string `json:"created_at"`
}

type ContainerConfig struct {
	Name    string
	Image   string
	Ports   []string
	EnvFile string

	// ExtraEnvFiles holds additional --env-file paths emitted
	// BEFORE the primary EnvFile. Used by job/cronjob env_from
	// stacking — each entry resolves to another resource's
	// materialised env file, layered in declared order. Docker's
	// last-wins semantics mean EnvFile (passed last) overrides
	// every entry here, and within the slice itself later entries
	// override earlier ones.
	ExtraEnvFiles []string

	// Env carries per-container environment variables emitted as
	// `-e KEY=VAL` on `docker run`. Distinct from EnvFile: the file
	// holds operator secrets and changes between restarts, while Env
	// is reserved for platform-injected identity that is fixed for
	// the lifetime of THIS specific container — VOODU_REPLICA_ORDINAL,
	// VOODU_SCOPE, VOODU_NAME, etc. Per-container `-e` is emitted
	// AFTER `--env-file`, so platform identity wins over a malicious
	// or accidental override in the operator's env file.
	Env map[string]string

	// NetworkMode is the legacy single-network knob — kept for the
	// build-driven deploy path (DeploymentConfig) which only ever joins
	// one network. Controller-managed deployments set Networks instead.
	NetworkMode string

	// Networks, when non-empty, wins over NetworkMode. The first entry
	// becomes --network at `docker run`; additional entries are attached
	// to the running container via `docker network connect`. This is the
	// only way to get a container onto more than one bridge (docker
	// doesn't accept --network multiple times).
	Networks []string

	RestartPolicy string
	Volumes       []string
	WorkingDir    string
	Command       []string

	// Labels are extra `--label k=v` flags appended after the umbrella
	// createdby=voodu marker. Controller-managed containers populate
	// this with the structured voodu.* identity (scope, name, kind,
	// replica_id) so the reconciler can list-by-labels instead of
	// parsing names. Build-mode and the legacy DeploymentConfig path
	// can leave it empty — the umbrella label still lands.
	Labels []string

	// NetworkAliases optionally registers DNS names other containers
	// can use to reach this one inside its networks. Voodu populates
	// these from the resource's (scope, name) so apps can resolve each
	// other without hardcoding replica IDs — e.g. every replica of
	// `clowk-lp/web` gets aliases `web.clowk-lp` and `web.clowk-lp.voodu`,
	// and Docker DNS round-robins between them.
	//
	// Aliases are applied to every network the container joins:
	// the primary one via `--network-alias` on `docker run`, and any
	// secondary ones via `docker network connect --alias`. Ignored in
	// host/none network mode (no docker bridge to register against).
	NetworkAliases []string

	// AutoRemove sets `--rm` so the container is deleted as soon as the
	// process exits. Used by job-kind runs that should not leave
	// stopped artifacts behind. Defaults to false (long-running
	// deployments must persist across restarts).
	AutoRemove bool

	// TTY allocates a pseudo-terminal for the container (`docker run
	// -t`). Without a TTY most language runtimes (Ruby, Node, Bun,
	// Python) full-buffer stdout when it's a pipe — the operator
	// only sees logs in one big dump after the process exits, which
	// kills realtime streaming for release-phase migrations. Setting
	// TTY=true on those one-shot runs forces line-buffering so
	// `docker logs -f` flows naturally.
	TTY bool

	// CPULimit becomes `--cpus <CPULimit>` on docker run when
	// non-empty. Pre-validated by the manifest layer — the value
	// here is already in the docker-accepted decimal float form
	// ("0.5", "2", "1.5"). Empty = no limit (cgroup defaults to
	// the host's full CPU count).
	CPULimit string

	// MemoryLimitBytes becomes `--memory <bytes>` on docker run
	// when > 0. Pre-validated and converted from k8s suffixes
	// (4Gi, 512Mi, 1G, etc.) by the manifest layer. 0 = no limit.
	MemoryLimitBytes int64

	// ExtraHosts emits one `--add-host name:ip` per entry, stacked
	// on top of the always-injected `host.docker.internal:host-gateway`.
	// Entries already match the `name:ip` shape (validated upstream
	// by the manifest layer). Useful for pointing the container at
	// internal services that don't have docker DNS — legacy DB on a
	// fixed IP, internal API on a VLAN, host.docker.internal pinning
	// to a specific interface, etc. docker-compose's `extra_hosts`.
	//
	// Skipped under host/none network mode: the container shares the
	// host's resolver, so /etc/hosts edits don't apply.
	ExtraHosts []string
	// CapAdd emits one `--cap-add CAP_NAME` per entry. Values are
	// bare capability names without the `CAP_` prefix (`SYS_NICE`,
	// `NET_ADMIN`, `IPC_LOCK`, …). Matches docker-compose's
	// `cap_add`. Pre-validated by the manifest layer so we splice
	// as-is.
	CapAdd []string

	// Ulimits is the per-resource `--ulimit <key>=<value>` table.
	// Each entry produces one --ulimit flag. Voodu's platform defaults
	// (nofile=65536:65536, nproc=4096:4096) apply for any key the
	// operator does NOT declare here; declared keys win and replace
	// the default on a per-key basis. Values flow verbatim to docker
	// — "N" (soft=hard) or "soft:hard" shapes both accepted.
	Ulimits map[string]string

	// DockerOptions is a raw pass-through bypass. Each entry is
	// appended verbatim to the `docker run` argv between the managed
	// flags voodu emits and the image. No parsing, no validation.
	// Use for compose-only knobs voodu doesn't model (--shm-size,
	// --pids-limit, --sysctl, --device, etc.). Operators MUST avoid
	// flags voodu already manages (--name, --network, --restart,
	// --env-file, --cpus, --memory, --ulimit, --label, --add-host,
	// --cap-add, --log-opt) — duplicates will make docker reject the
	// container at create time.
	DockerOptions []string

	// LogMaxSize and LogMaxFiles cap the docker json-file log
	// driver per container — equivalent to docker-compose
	// `logging.options.max-size` / `max-file`. Both must be set
	// for the cap to land; if either is empty/zero we skip both
	// `--log-opt` flags and inherit the daemon default
	// (unbounded). The controller's handler always populates
	// both fields from the manifest's `logs {}` block (with
	// platform 10m/3 defaults applied), so production
	// containers never run unbounded by accident.
	//
	// Value format mirrors docker's own: "10m", "1g", "500k"
	// for size; plain integer for file count. Pre-validated by
	// the manifest layer — splice as-is.
	LogMaxSize  string
	LogMaxFiles int
}

// DeploymentConfig represents deployment configuration.
type DeploymentConfig struct {
	AppName       string
	ImageTag      string
	EnvFile       string
	ReleaseDir    string
	ZeroDowntime  bool
	HealthTimeout int
	NetworkMode   string
	DockerPorts   []string
	Volumes       []string
}

// CreateContainer creates a new container with the given configuration.
func CreateContainer(cfg ContainerConfig) error {
	args := buildRunArgs(cfg)

	cmd := exec.Command("docker", args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("failed to create container %s: %v, output: %s", cfg.Name, err, string(output))
	}

	// Attach any secondary networks. Docker only accepts one --network
	// at create time, so fanning out here is the only path to multi-
	// homed containers. If a connect fails we roll the container back —
	// a half-joined container is worse than no container because it
	// silently misses whichever network the operator declared.
	//
	// Skip entirely in host/none mode: attaching a bridge to a
	// host-networked container is either rejected by docker or silently
	// ignored, and either way it's operator confusion we don't want.
	primaryNet := ""
	if cfg.NetworkMode != "" {
		primaryNet = cfg.NetworkMode
	} else if len(cfg.Networks) > 0 {
		primaryNet = cfg.Networks[0]
	}

	if cfg.NetworkMode == "" && len(cfg.Networks) > 1 {
		for _, net := range cfg.Networks[1:] {
			if net == "" || net == primaryNet {
				continue
			}

			if err := ConnectNetwork(cfg.Name, net, cfg.NetworkAliases); err != nil {
				_ = RemoveContainer(cfg.Name, true)
				return fmt.Errorf("failed to connect container %s to network %s: %w", cfg.Name, net, err)
			}
		}
	}

	return nil
}

// buildRunArgs assembles the `docker run` argument list from a
// ContainerConfig. Extracted from CreateContainer so tests can
// inspect the exact flags the daemon would receive without
// actually shelling out to docker. Production CreateContainer
// calls this then exec's `docker` with the result; tests call it
// directly and assert on the slice (e.g. that --log-opt lands
// when LogMaxSize is set).
//
// Side effects: none. The function is pure — same config in,
// same args out, every time. Argument ordering is stable so test
// expectations against `len(args)` / `args[i]` stay reproducible.
func buildRunArgs(cfg ContainerConfig) []string {
	args := []string{"run", "-d", "--name", cfg.Name}

	if cfg.AutoRemove {
		args = append(args, "--rm")
	}

	for _, label := range GetVooduLabels() {
		args = append(args, "--label", label)
	}

	// Caller-supplied labels (typically voodu.scope=…, voodu.name=…,
	// voodu.kind=…). The umbrella createdby=voodu above stays for the
	// existing ListContainers filter; structured labels ride alongside.
	for _, label := range cfg.Labels {
		args = append(args, "--label", label)
	}

	if cfg.RestartPolicy != "" {
		args = append(args, "--restart", cfg.RestartPolicy)
	}

	// host.docker.internal:host-gateway makes the host reachable
	// from inside the container under a stable name. On Mac/Win
	// Docker Desktop this is built-in; on Linux it requires the
	// explicit --add-host flag (Docker 20.10+). We always pass it
	// so plugin-authored entrypoint scripts (sentinel's failover
	// hook, M-S4 preflight, future probes) can call back to the
	// controller via VOODU_CONTROLLER_URL = http://host.docker.internal:<port>
	// regardless of the host OS.
	//
	// Skipped for host/none network modes — those don't use
	// docker's resolver, so --add-host has no effect (and host
	// mode IS the host, no need for the alias).
	if cfg.NetworkMode != "host" && cfg.NetworkMode != "none" {
		args = append(args, "--add-host", "host.docker.internal:host-gateway")

		// Operator-declared extra hosts ride on top of the always-injected
		// platform alias. Each entry is already validated as "name:ip".
		// host/none modes skip both branches — see the field comment for
		// rationale.
		for _, h := range cfg.ExtraHosts {
			if h == "" {
				continue
			}

			args = append(args, "--add-host", h)
		}
	}

	// Capabilities are NOT gated on network mode — they're a kernel
	// concern, independent of how the container connects.
	for _, c := range cfg.CapAdd {
		if c == "" {
			continue
		}

		args = append(args, "--cap-add", c)
	}

	// NetworkMode (host/none) wins when explicitly set — it bypasses
	// docker's bridge stack entirely, so Networks is irrelevant. The
	// handler validates mutual exclusivity before we get here; this
	// branch just picks which field to trust. Otherwise fall through
	// to the bridge path: first Networks entry becomes --network, and
	// extras are attached post-run via docker network connect.
	primaryNet := ""
	if cfg.NetworkMode != "" {
		primaryNet = cfg.NetworkMode
	} else if len(cfg.Networks) > 0 {
		primaryNet = cfg.Networks[0]
	}

	if primaryNet != "" {
		args = append(args, "--network", primaryNet)
	}

	// Network aliases register DNS names other containers can resolve
	// over Docker's embedded resolver. Only meaningful in bridge mode
	// (host/none have no embedded DNS to register against). At create
	// time `--network-alias` only applies to the primary --network;
	// secondary networks pick up their own --alias flags via
	// ConnectNetwork below.
	if cfg.NetworkMode != "host" && cfg.NetworkMode != "none" {
		for _, alias := range cfg.NetworkAliases {
			args = append(args, "--network-alias", alias)
		}
	}

	// Port publishing is meaningless (and rejected with a warning by
	// modern docker) when the container shares the host's net stack:
	// in host mode the container's listening ports ARE the host's
	// ports, so there's no NAT rule to install. Same for `none` mode
	// where the container has no network namespace at all. Skipping
	// here avoids the "Published ports are discarded" warning that
	// confuses operators reading the daemon log.
	if cfg.NetworkMode != "host" && cfg.NetworkMode != "none" {
		for _, port := range cfg.Ports {
			args = append(args, "-p", port)
		}
	}

	// ExtraEnvFiles come BEFORE the primary EnvFile so the latter
	// (the resource's own merged env) wins on conflicts. Within
	// the slice, declared order is preserved — later entries
	// override earlier ones, matching the operator's `env_from =
	// [a, b, c]` mental model.
	for _, ef := range cfg.ExtraEnvFiles {
		if ef == "" {
			continue
		}

		args = append(args, "--env-file", ef)
	}

	if cfg.EnvFile != "" {
		args = append(args, "--env-file", cfg.EnvFile)
	}

	// Per-container env vars come AFTER --env-file so docker resolves
	// the file first and these `-e` flags override any colliding key.
	// Platform identity (VOODU_REPLICA_ORDINAL/SCOPE/NAME) MUST win
	// over the operator's env file even if the operator accidentally
	// declared a key with the same name. Iterate in sorted key order
	// so the rendered `docker run` line is deterministic — easier to
	// diff in tests and in logs.
	if len(cfg.Env) > 0 {
		keys := make([]string, 0, len(cfg.Env))

		for k := range cfg.Env {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		for _, k := range keys {
			args = append(args, "-e", k+"="+cfg.Env[k])
		}
	}

	for _, volume := range cfg.Volumes {
		args = append(args, "-v", volume)
	}

	if cfg.WorkingDir != "" {
		args = append(args, "-w", cfg.WorkingDir)
	}

	// Ulimit table: start from platform defaults and let operator
	// overrides win per-key. Defaults still apply for any key the
	// operator did not declare, so the existing "tight sockets + lots
	// of files" baseline is preserved unless explicitly opted out.
	ulimits := map[string]string{
		"nofile": "65536:65536",
		"nproc":  "4096:4096",
	}

	for k, v := range cfg.Ulimits {
		if k == "" {
			continue
		}

		ulimits[k] = v
	}

	ulimitKeys := make([]string, 0, len(ulimits))
	for k := range ulimits {
		ulimitKeys = append(ulimitKeys, k)
	}

	sort.Strings(ulimitKeys)

	for _, k := range ulimitKeys {
		args = append(args, "--ulimit", k+"="+ulimits[k])
	}

	// Resource caps via cgroups. Empty/zero values mean "no limit"
	// — docker daemon's defaults apply (effectively unbounded
	// until the host runs out). Pre-validated by the manifest
	// layer, so any non-empty value is safe to splice as-is.
	if cfg.CPULimit != "" {
		args = append(args, "--cpus", cfg.CPULimit)
	}

	if cfg.MemoryLimitBytes > 0 {
		args = append(args, "--memory", fmt.Sprintf("%d", cfg.MemoryLimitBytes))
	}

	// Log driver cap via docker's json-file `--log-opt` flags.
	// Both fields must be populated for the cap to actually
	// land — otherwise we'd emit a partial spec (just max-size
	// without max-file, or vice-versa) which docker accepts but
	// is operator-confusing. The controller's handler always
	// fills both before calling here (with platform defaults
	// when the operator omitted the block), so the typical
	// production path emits both flags.
	if cfg.LogMaxSize != "" && cfg.LogMaxFiles > 0 {
		args = append(args, "--log-opt", "max-size="+cfg.LogMaxSize)
		args = append(args, "--log-opt", "max-file="+strconv.Itoa(cfg.LogMaxFiles))
	}

	// TTY allocation forces line-buffered stdout in the container's
	// process. Plumbed in for release-phase one-shots; long-running
	// deployments leave it off (default) since they don't need a
	// terminal and a pty per replica is wasted kernel state.
	if cfg.TTY {
		args = append(args, "-t")
	}

	// Raw bypass: operator-supplied flags flow verbatim. Splicing
	// here (after every managed flag, before the image) means
	// operator flags are docker's "later wins" tiebreaker if anyone
	// duplicates a flag voodu already emitted — but the contract
	// documents this as undefined behaviour, so duplicates are an
	// operator bug.
	for _, opt := range cfg.DockerOptions {
		if opt == "" {
			continue
		}

		args = append(args, opt)
	}

	args = append(args, cfg.Image)

	if len(cfg.Command) > 0 {
		args = append(args, cfg.Command...)
	}

	return args
}

// ConnectNetwork attaches an existing container to an additional docker
// network. Used by CreateContainer to fan out multi-network specs, but
// exported so the reconciler (or future drift logic) can rewire
// membership without recreating the container.
//
// aliases register DNS names on the new network — equivalent to the
// `--network-alias` set at `docker run` for the primary network.
// Pass nil/empty to attach without any alias (the container is still
// reachable by its docker name, just not by the synthetic <name>.<scope>
// shape). Aliases must be repeated per-network because Docker scopes
// them to the network they were declared on.
func ConnectNetwork(container, network string, aliases []string) error {
	args := []string{"network", "connect"}

	for _, a := range aliases {
		args = append(args, "--alias", a)
	}

	args = append(args, network, container)

	cmd := exec.Command("docker", args...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker network connect %s %s: %v, output: %s", network, container, err, string(output))
	}

	return nil
}

// ListContainers returns list of containers in JSON format.
// By default, only lists containers with Voodu labels to avoid conflicts.
func ListContainers(all bool) ([]ContainerInfo, error) {
	return ListContainersFiltered(all, nil)
}

// ListContainersFiltered runs `docker ps` scoped to voodu-managed
// containers (via createdby=voodu) plus any extra label filters the
// caller supplies.
//
// Each entry in extraLabels is a `key=value` pair appended as
// `--filter label=…`. Docker AND-combines multiple label filters, so
// passing `[voodu.kind=deployment, voodu.scope=softphone]` returns
// only the deployment containers in that scope. Used by the
// reconciler (ListByIdentity) and `voodu get pods`.
func ListContainersFiltered(all bool, extraLabels []string) ([]ContainerInfo, error) {
	args := []string{"ps"}

	if all {
		args = append(args, "-a")
	}

	args = append(args, "--format", "json", "--filter", fmt.Sprintf("label=%s=%s", VooduLabelKey, VooduLabelValue))

	for _, lbl := range extraLabels {
		if lbl == "" {
			continue
		}

		args = append(args, "--filter", "label="+lbl)
	}

	cmd := exec.Command("docker", args...)
	output, err := cmd.Output()

	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %v", err)
	}

	var containers []ContainerInfo
	lines := strings.Split(string(output), "\n")

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var container ContainerInfo

		if err := json.Unmarshal([]byte(line), &container); err != nil {
			continue
		}

		containers = append(containers, container)
	}

	return containers, nil
}

// InspectLabels returns the labels of a single container as a flat
// map. Used by the reconciler / `voodu describe` to recover the
// structured voodu.* identity. `docker ps --format json` does not
// emit labels, so this is the second hop.
//
// Returns (nil, nil) when the container does not exist — distinct
// from (nil, err) which is a real failure to inspect.
func InspectLabels(name string) (map[string]string, error) {
	if !ContainerExists(name) {
		return nil, nil
	}

	cmd := exec.Command("docker", "inspect", name, "--format", "{{json .Config.Labels}}")

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect %s labels: %w", name, err)
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return map[string]string{}, nil
	}

	var labels map[string]string

	if err := json.Unmarshal([]byte(trimmed), &labels); err != nil {
		return nil, fmt.Errorf("parse labels for %s: %w", name, err)
	}

	return labels, nil
}

// ContainerDetail is the rich-inspect blob `voodu describe pod`
// renders. Subset of `docker inspect` flattened into a flat,
// CLI-friendly shape so the controller doesn't ship the full,
// 200-field daemon JSON to the operator.
//
// All fields are omitempty so a partially-populated container (just
// created, not yet started, no networks attached) renders cleanly
// with missing sections elided.
type ContainerDetail struct {
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Image string `json:"image,omitempty"`

	// State mirrors the running/stopped/exited bookkeeping. ExitCode
	// is meaningful only when Running is false; StartedAt / FinishedAt
	// are RFC3339 strings as docker emits them.
	State ContainerState `json:"state"`

	// Config from the container's Config block.
	Command    []string          `json:"command,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`

	// HostConfig — restart policy is the only knob `voodu describe`
	// cares about today; the rest stay in the docker JSON for now.
	RestartPolicy string `json:"restart_policy,omitempty"`

	// Networks lists every network the container is attached to.
	// Mounts is a flat shape — bind/volume distinction lives in Type.
	// Ports flatten the docker port-binding map: "80/tcp" → "0.0.0.0:8080".
	Networks map[string]ContainerNetwork `json:"networks,omitempty"`
	Mounts   []ContainerMount            `json:"mounts,omitempty"`
	Ports    []ContainerPort             `json:"ports,omitempty"`
}

type ContainerState struct {
	Status     string `json:"status,omitempty"`
	Running    bool   `json:"running"`
	ExitCode   int    `json:"exit_code,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Restarts   int    `json:"restarts,omitempty"`
}

type ContainerNetwork struct {
	NetworkID string `json:"network_id,omitempty"`
	IPAddress string `json:"ip_address,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	Aliases   []string `json:"aliases,omitempty"`
}

type ContainerMount struct {
	Type        string `json:"type,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
	Mode        string `json:"mode,omitempty"`
	RW          bool   `json:"rw,omitempty"`
}

type ContainerPort struct {
	Container string `json:"container,omitempty"`
	HostIP    string `json:"host_ip,omitempty"`
	HostPort  string `json:"host_port,omitempty"`
}

// InspectContainer returns the rich `docker inspect` blob flattened
// into a CLI-friendly shape. Returns (nil, nil) when the container
// doesn't exist — distinct from (nil, err) which is a real inspect
// failure (docker daemon down, malformed output, etc.).
//
// The full `docker inspect` is enormous (200+ fields) and most are
// noise for an operator. This helper keeps just the parts `voodu
// describe pod` actually renders. New fields can be added one by one.
func InspectContainer(name string) (*ContainerDetail, error) {
	if !ContainerExists(name) {
		return nil, nil
	}

	cmd := exec.Command("docker", "inspect", name)

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", name, err)
	}

	// `docker inspect <name>` returns a JSON array (always one element
	// for a single name). Decode into a permissive intermediate shape
	// then map to the flat detail.
	var raw []dockerInspectRaw

	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse inspect %s: %w", name, err)
	}

	if len(raw) == 0 {
		return nil, nil
	}

	r := raw[0]

	d := &ContainerDetail{
		ID:    r.ID,
		Name:  strings.TrimPrefix(r.Name, "/"),
		Image: r.Config.Image,
		State: ContainerState{
			Status:     r.State.Status,
			Running:    r.State.Running,
			ExitCode:   r.State.ExitCode,
			StartedAt:  r.State.StartedAt,
			FinishedAt: r.State.FinishedAt,
			Restarts:   r.RestartCount,
		},
		Command:    r.Config.Cmd,
		Entrypoint: r.Config.Entrypoint,
		WorkingDir: r.Config.WorkingDir,
		Labels:     r.Config.Labels,
	}

	if len(r.Config.Env) > 0 {
		d.Env = make(map[string]string, len(r.Config.Env))

		for _, kv := range r.Config.Env {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				d.Env[kv[:i]] = kv[i+1:]
			} else {
				d.Env[kv] = ""
			}
		}
	}

	if r.HostConfig.RestartPolicy.Name != "" {
		d.RestartPolicy = r.HostConfig.RestartPolicy.Name
	}

	if len(r.NetworkSettings.Networks) > 0 {
		d.Networks = make(map[string]ContainerNetwork, len(r.NetworkSettings.Networks))

		for k, n := range r.NetworkSettings.Networks {
			d.Networks[k] = ContainerNetwork{
				NetworkID: n.NetworkID,
				IPAddress: n.IPAddress,
				Gateway:   n.Gateway,
				Aliases:   n.Aliases,
			}
		}
	}

	for _, m := range r.Mounts {
		d.Mounts = append(d.Mounts, ContainerMount{
			Type:        m.Type,
			Source:      m.Source,
			Destination: m.Destination,
			Mode:        m.Mode,
			RW:          m.RW,
		})
	}

	for portKey, bindings := range r.NetworkSettings.Ports {
		if len(bindings) == 0 {
			d.Ports = append(d.Ports, ContainerPort{Container: portKey})
			continue
		}

		for _, b := range bindings {
			d.Ports = append(d.Ports, ContainerPort{
				Container: portKey,
				HostIP:    b.HostIP,
				HostPort:  b.HostPort,
			})
		}
	}

	return d, nil
}

// dockerInspectRaw is the permissive shape we decode `docker inspect`
// into. Only the subfields we actually surface are listed — extra
// fields in the docker JSON are ignored without warning.
type dockerInspectRaw struct {
	ID              string                  `json:"Id"`
	Name            string                  `json:"Name"`
	RestartCount    int                     `json:"RestartCount"`
	State           dockerInspectState      `json:"State"`
	Config          dockerInspectConfig     `json:"Config"`
	HostConfig      dockerInspectHostConfig `json:"HostConfig"`
	NetworkSettings dockerInspectNetSet     `json:"NetworkSettings"`
	Mounts          []dockerInspectMount    `json:"Mounts"`
}

type dockerInspectState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	ExitCode   int    `json:"ExitCode"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

type dockerInspectConfig struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Entrypoint []string          `json:"Entrypoint"`
	Env        []string          `json:"Env"`
	Labels     map[string]string `json:"Labels"`
	WorkingDir string            `json:"WorkingDir"`
}

type dockerInspectHostConfig struct {
	RestartPolicy struct {
		Name string `json:"Name"`
	} `json:"RestartPolicy"`
}

type dockerInspectNetSet struct {
	Networks map[string]dockerInspectNetwork    `json:"Networks"`
	Ports    map[string][]dockerInspectPortBind `json:"Ports"`
}

type dockerInspectNetwork struct {
	NetworkID string   `json:"NetworkID"`
	IPAddress string   `json:"IPAddress"`
	Gateway   string   `json:"Gateway"`
	Aliases   []string `json:"Aliases"`
}

type dockerInspectPortBind struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerInspectMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
}

// ContainerExists checks if a container exists.
// First checks containers with Voodu labels, then falls back to direct name check for backwards compatibility.
func ContainerExists(name string) bool {
	containers, err := ListContainers(true)
	if err == nil {
		for _, container := range containers {
			if strings.Contains(container.Name, name) {
				return true
			}
		}
	}

	cmd := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}", "--filter", fmt.Sprintf("name=%s", name))
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == name {
			return true
		}
	}

	return false
}

// GetContainerImage returns the image tag of the named container via
// `docker inspect`. Empty string + nil error means no such container;
// callers distinguish "not running" from "unreadable" by checking the
// error.
func GetContainerImage(name string) (string, error) {
	if !ContainerExists(name) {
		return "", nil
	}

	cmd := exec.Command("docker", "inspect", name, "--format", "{{.Config.Image}}")

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", name, err)
	}

	return strings.TrimSpace(string(out)), nil
}

// GetContainerImageID returns the image ID (sha256) the container was
// created from. Distinct from GetContainerImage (which returns the tag)
// because tags are mutable: `vd-web:latest` today is a different image
// than `vd-web:latest` after a rebuild. The ID is the stable identity
// and the only reliable way to detect "tag got rewritten, container
// needs to restart from the new image".
//
// Empty string + nil error means no such container.
func GetContainerImageID(name string) (string, error) {
	if !ContainerExists(name) {
		return "", nil
	}

	cmd := exec.Command("docker", "inspect", name, "--format", "{{.Image}}")

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", name, err)
	}

	return strings.TrimSpace(string(out)), nil
}

// TagImage points dst at the same image ID currently aliased by
// src. Wraps `docker tag <src> <dst>`. Idempotent — re-tagging
// an existing tag at the same image is a no-op as far as docker
// is concerned (just updates the tag→id map in place).
//
// Used by `vd rollback` to re-point <app>:latest at an older
// <app>:<buildID> tag without rebuilding. Errors when src isn't
// known to the local daemon (image was GC'd; caller falls back
// to a from-source rebuild).
func TagImage(src, dst string) error {
	cmd := exec.Command("docker", "tag", src, dst)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker tag %s %s: %s", src, dst, strings.TrimSpace(string(out)))
	}

	return nil
}

// ImageExists reports whether a local image matches `ref`. Same
// shape as GetImageID's "missing" path (empty string == not
// present), but expressed as a bool so callers don't have to write
// the cmp themselves. Used by rollback to decide between fast-path
// retag and slow-path rebuild.
func ImageExists(ref string) bool {
	id, err := GetImageID(ref)
	if err != nil {
		return false
	}

	return id != ""
}

// RemoveImageTag deletes a single tag without touching other tags
// pointing at the same image content. `docker rmi` removes the
// tag→id entry; the underlying image stays as long as another tag
// (or running container) references it.
//
// Used by the release-history GC to drop <app>:<buildID> tags
// for records that fell off maxReleaseHistory. Idempotent — a
// missing tag returns nil (no-op) rather than an error so the GC
// doesn't trip on a double-cleanup.
func RemoveImageTag(ref string) error {
	if !ImageExists(ref) {
		return nil
	}

	cmd := exec.Command("docker", "rmi", "--no-prune", ref)

	out, err := cmd.CombinedOutput()
	if err != nil {
		// "No such image" between the existence check and the rmi
		// call is a benign race; surface anything else.
		raw := strings.TrimSpace(string(out))
		if strings.Contains(raw, "No such image") {
			return nil
		}

		return fmt.Errorf("docker rmi %s: %s", ref, raw)
	}

	return nil
}

// GetImageID resolves a tag (or any image reference) to its current
// sha256 ID. Pair with GetContainerImageID to detect build-mode drift:
// after `docker build -t vd-web:latest`, the tag points at a new ID
// but existing containers still reference the old one.
//
// Empty string + nil error means the image doesn't exist locally.
func GetImageID(ref string) (string, error) {
	cmd := exec.Command("docker", "inspect", ref, "--format", "{{.Id}}")

	out, err := cmd.Output()
	if err != nil {
		// Exit code 1 with "No such object" on stderr is the common
		// "image not pulled/built yet" case — treat it as non-fatal so
		// the caller can transient-retry.
		if strings.Contains(string(out), "No such") {
			return "", nil
		}

		return "", nil
	}

	return strings.TrimSpace(string(out)), nil
}

// ContainerIsRunning checks if a container is currently running.
func ContainerIsRunning(name string) bool {
	cmd := exec.Command("docker", "ps", "--format", "{{.Names}}", "--filter", fmt.Sprintf("name=%s", name))
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == name {
			return true
		}
	}

	return false
}

// StopContainer stops a running container.
func StopContainer(name string) error {
	cmd := exec.Command("docker", "stop", name)

	return cmd.Run()
}

// RestartContainer runs `docker restart <name>` — SIGTERM, grace,
// SIGKILL, then start. Keeps the same container ID and labels,
// just bounces the process inside. Used by the probe runner's
// liveness-failure path (kubelet-style "process is alive but
// deadlocked") where we want to bounce without losing identity.
//
// Distinct from RecreateActiveContainer / Recreate (which destroy
// and rebuild the container from spec). Restart in place preserves
// any in-container debugging state the operator might be inspecting.
func RestartContainer(name string) error {
	cmd := exec.Command("docker", "restart", name)

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker restart %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}

	return nil
}

// ContainerIP returns the first non-empty IP address from the
// container's network list — typically the voodu0 bridge IP. Used by
// the probe runner to address HTTP/TCP probes directly on the
// container's network without going through host-side port
// publishing (which would only expose ports the operator declared,
// and on a randomly-allocated host port that's harder to compute).
//
// Returns ("", nil) when the container exists but has no networks
// attached (rare — host-network mode, transient stop/start). Returns
// the IP+ nil on success. Errors only on inspect failure.
func ContainerIP(name string) (string, error) {
	det, err := InspectContainer(name)
	if err != nil {
		return "", err
	}

	if det == nil {
		return "", nil
	}

	// Prefer voodu0 if present (the platform bridge); fall back to
	// whatever's first. Order matters only when the container has
	// multiple networks — a deployment on voodu0 + a custom bridge —
	// and we want probes to traverse the platform bus, not the
	// app's overlay.
	if n, ok := det.Networks["voodu0"]; ok && n.IPAddress != "" {
		return n.IPAddress, nil
	}

	for _, n := range det.Networks {
		if n.IPAddress != "" {
			return n.IPAddress, nil
		}
	}

	return "", nil
}

// IsRunning reports whether the container is currently in the running
// state. Distinct from ContainerExists: a stopped container exists but
// is not running, and the reconciler treats that as "needs a start".
// Returns (false, nil) when the container doesn't exist — the caller
// can decide whether that's an error in its context.
func IsRunning(name string) (bool, error) {
	cmd := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", name)

	out, err := cmd.Output()
	if err != nil {
		// `docker inspect` exits non-zero when the container is missing.
		// Treat "missing" as "not running" and let the caller distinguish
		// via ContainerExists if it cares.
		return false, nil
	}

	return strings.TrimSpace(string(out)) == "true", nil
}

// StartContainer runs `docker start` on an existing, stopped container.
// Used by the reconciler's replay path to recover containers whose
// restart policy was "no" and which therefore stayed dead after a host
// reboot.
func StartContainer(name string) error {
	cmd := exec.Command("docker", "start", name)

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker start %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}

	return nil
}

// UpdateRestartPolicy sets the restart policy on an existing container.
// Used to repair containers that were created before voodu defaulted
// to "unless-stopped" — those containers wouldn't survive a reboot,
// and we want the next reconcile to fix them in place without a
// destructive recreate.
func UpdateRestartPolicy(name, policy string) error {
	cmd := exec.Command("docker", "update", "--restart", policy, name)

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker update --restart %s %s: %v: %s", policy, name, err, strings.TrimSpace(string(out)))
	}

	return nil
}

// WaitContainer blocks until the named container exits and returns the
// process exit code. Backed by `docker wait`, which prints the integer
// exit code on stdout when the container terminates. Used by the job
// runner: jobs are containers we spawn synchronously and observe to
// completion.
//
// Returns an error when docker can't wait on the container (already
// removed, daemon down) or when the output isn't a parseable integer
// — the caller treats that as a failed run distinct from a non-zero
// exit, since we can't trust any value.
func WaitContainer(name string) (int, error) {
	cmd := exec.Command("docker", "wait", name)

	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("docker wait %s: %w", name, err)
	}

	trimmed := strings.TrimSpace(string(out))

	code, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("parse exit code from docker wait %s: %q: %w", name, trimmed, err)
	}

	return code, nil
}

// ExecOptions tunes the `docker exec` invocation. TTY allocates a
// pty (kubectl exec -t style); Interactive keeps stdin open
// (kubectl exec -i). Working/User let the caller override the
// runtime defaults baked into the container's image. WorkingDir
// defaults to whatever the image declared.
type ExecOptions struct {
	TTY         bool
	Interactive bool
	WorkingDir  string
	User        string
	Env         []string

	// Stdin/Stdout/Stderr wire the streams to the docker exec child.
	// Leave any nil to inherit the parent's; for TTY mode the caller
	// typically points all three at the local terminal.
	//
	// In TTY mode, stderr is merged into stdout by the pty (the
	// kernel's tty discipline doesn't keep them separate). The
	// Stderr field is ignored; we route everything to Stdout —
	// matches how `docker exec -t` and kubectl-exec behave.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Cols/Rows describe the operator's terminal size for PTY
	// allocation. Only consulted when TTY=true. Both zero → use the
	// pty's default (24x80 from the kernel). Sticky for the duration
	// of the session — mid-session resizes (SIGWINCH) need a wire
	// protocol that doesn't exist yet, so the operator's window
	// dimensions at exec start are what the remote shell sees.
	Cols uint16
	Rows uint16
}

// ExecContainer runs a command inside an already-running container —
// kubectl exec semantics. Returns the child's exit code via the
// usual exec.ExitError conversion: 0 on clean exit, non-zero when the
// command exits with a status, error when docker itself fails to
// start (container missing, daemon down).
//
// The function blocks until the command exits. Streaming back to the
// caller happens through opts.Stdout/Stderr — the API handler points
// these at hijacked HTTP connections so the CLI sees output in real
// time. With TTY enabled, a single multiplexed stream is used and
// stderr is unset; without TTY, stdout and stderr are separate so
// CLI consumers can keep them apart.
func ExecContainer(name string, command []string, opts ExecOptions) (int, error) {
	if name == "" {
		return 0, fmt.Errorf("exec: container name is required")
	}

	if len(command) == 0 {
		return 0, fmt.Errorf("exec: command is required")
	}

	args := []string{"exec"}

	if opts.Interactive {
		args = append(args, "-i")
	}

	if opts.TTY {
		args = append(args, "-t")
	}

	if opts.WorkingDir != "" {
		args = append(args, "-w", opts.WorkingDir)
	}

	if opts.User != "" {
		args = append(args, "-u", opts.User)
	}

	for _, kv := range opts.Env {
		args = append(args, "-e", kv)
	}

	args = append(args, name)
	args = append(args, command...)

	cmd := exec.Command("docker", args...)

	if opts.TTY {
		return execContainerTTY(cmd, name, opts)
	}

	return execContainerPipe(cmd, name, opts)
}

// execContainerPipe is the non-TTY path: stdout/stderr stay separate,
// stdin is wired through a pipe we own (StdinPipe) so cmd.Wait
// doesn't deadlock on a never-EOFing opts.Stdin (typically the
// hijacked HTTP conn). See ExecContainer for the full rationale.
func execContainerPipe(cmd *exec.Cmd, name string, opts ExecOptions) (int, error) {
	if opts.Stdout != nil {
		cmd.Stdout = opts.Stdout
	}

	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	}

	var stdinWriter io.WriteCloser

	if opts.Stdin != nil {
		var err error

		stdinWriter, err = cmd.StdinPipe()
		if err != nil {
			return 1, fmt.Errorf("stdin pipe: %w", err)
		}
	}

	if err := cmd.Start(); err != nil {
		if stdinWriter != nil {
			_ = stdinWriter.Close()
		}

		return 1, fmt.Errorf("docker exec %s: %w", name, err)
	}

	if stdinWriter != nil {
		// Fire-and-forget. The goroutine exits when EITHER:
		//   - opts.Stdin returns EOF/error (operator typed Ctrl-D,
		//     or the upstream conn was closed by the API handler);
		//   - stdinWriter.Write fails because the docker exec
		//     process closed its stdin pipe on exit.
		//
		// Either way, no shared state with cmd.Wait below.
		go func() {
			_, _ = io.Copy(stdinWriter, opts.Stdin)
			_ = stdinWriter.Close()
		}()
	}

	err := cmd.Wait()

	if stdinWriter != nil {
		_ = stdinWriter.Close()
	}

	if err == nil {
		return 0, nil
	}

	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), nil
	}

	return 1, fmt.Errorf("docker exec %s: %w", name, err)
}

// execContainerTTY is the PTY-mode path: docker exec is launched
// with -it, and we allocate a pty pair so its stdin/stdout/stderr
// are all genuine ttys. The master end of the pty becomes the
// bidirectional channel between the operator's terminal (over the
// hijacked HTTP conn) and the remote shell.
//
// Two goroutines bridge bytes:
//
//	conn → ptmx → docker stdin → shell stdin    (operator's keypresses)
//	shell stdout → docker stdout → ptmx → conn  (program output)
//
// Stderr is merged into stdout by the kernel pty (tty discipline
// doesn't multiplex), so opts.Stderr is ignored. Same shape kubectl
// exec -t produces.
//
// Window size: when opts.Cols/Rows are non-zero, we set the pty
// dimensions before any output is written so the shell starts up
// at the operator's terminal size. Mid-session resize (SIGWINCH)
// needs a wire protocol that doesn't exist yet — the dimensions
// at exec start are sticky for the session.
func execContainerTTY(cmd *exec.Cmd, name string, opts ExecOptions) (int, error) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return 1, fmt.Errorf("pty start %s: %w", name, err)
	}

	defer func() { _ = ptmx.Close() }()

	if opts.Cols > 0 || opts.Rows > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{
			Rows: opts.Rows,
			Cols: opts.Cols,
		})
	}

	// pty → conn (output). Exits when ptmx returns EOF (the slave
	// end was closed by the kernel after the child exited).
	if opts.Stdout != nil {
		go func() {
			_, _ = io.Copy(opts.Stdout, ptmx)
		}()
	}

	// conn → pty (input). Same lifecycle gotcha as the non-TTY
	// path: opts.Stdin (hijacked HTTP conn) only EOFs when the
	// upstream handler closes the conn. The goroutine parks on
	// conn.Read; cmd.Wait doesn't see this goroutine because it's
	// ours, not Go's internal stdin-feeder. Once the handler's
	// deferred conn.Close runs, the goroutine wakes up and exits.
	if opts.Stdin != nil {
		go func() {
			_, _ = io.Copy(ptmx, opts.Stdin)
		}()
	}

	err = cmd.Wait()
	if err == nil {
		return 0, nil
	}

	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), nil
	}

	return 1, fmt.Errorf("docker exec %s: %w", name, err)
}

// LogsStream spawns `docker logs [-f] [--tail N] <name>` and returns
// the merged stdout+stderr as a ReadCloser. The caller MUST Close the
// returned reader to reap the docker process — otherwise we leak a
// zombie per call.
//
// Tail = 0 means "all logs"; positive values translate to `--tail N`.
// Follow streams new lines as they arrive (until the container exits
// or the reader is closed). Both modes work on stopped containers
// because docker keeps the json-file driver's log around until the
// container is removed — this is the whole point of dropping
// AutoRemove on jobs.
//
// Stderr is interleaved with stdout in the returned stream because
// `docker logs` already merges them at the daemon level. Splitting
// them would require two pipes and callers always want them
// interleaved anyway (just like a tail of a normal process).
func LogsStream(name string, follow bool, tail int) (io.ReadCloser, error) {
	args := []string{"logs"}

	if follow {
		args = append(args, "-f")
	}

	if tail > 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}

	args = append(args, name)

	cmd := exec.Command("docker", args...)

	// Merge stdout + stderr onto a SINGLE OS pipe so the reader
	// gets both streams interleaved in arrival order. Earlier
	// version used StdoutPipe/StderrPipe separately and read
	// stdout first; for daemons that log only to stderr (postgres,
	// redis, jobs writing to stderr by convention) the reader
	// blocked on stdout forever and `vd logs -f` showed only
	// the initial buffered output — never streamed live. With
	// the merged pipe, daemon stderr lines surface immediately.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("docker logs %s: pipe: %w", name, err)
	}

	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()

		return nil, fmt.Errorf("docker logs %s: start: %w", name, err)
	}

	// CRITICAL: close the WRITER end in the parent. The child
	// inherited a duplicate of pw; closing in the parent doesn't
	// affect the child's stdout/stderr but ensures `pr.Read`
	// returns EOF when the child exits and closes its end. Without
	// this, the parent's pw stays open and the reader blocks
	// forever after the child exits — `vd logs` (without -f)
	// against an exited container would never return.
	_ = pw.Close()

	return &dockerLogsReader{
		cmd:    cmd,
		merged: pr,
	}, nil
}

// dockerLogsReader is a thin io.ReadCloser over the merged
// stdout+stderr pipe. The actual reading is one os.Read against
// the pipe; the cmd-side merge happens at process start (see
// LogsStream).
type dockerLogsReader struct {
	cmd    *exec.Cmd
	merged io.ReadCloser

	once sync.Once
}

// Read returns whatever the docker process has written so far
// across BOTH stdout and stderr, interleaved in OS-write order.
// Atomic per write call, not per line — same trade-off `docker
// logs name 2>&1` makes on a shell.
func (r *dockerLogsReader) Read(p []byte) (int, error) {
	return r.merged.Read(p)
}

// Close kills the docker process and reaps it. Idempotent — the
// HTTP handler may call it multiple times if the client disconnects
// mid-stream.
func (r *dockerLogsReader) Close() error {
	r.once.Do(func() {
		_ = r.merged.Close()

		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}

		// Wait reaps the process; the error is uninteresting (we just
		// killed it) so we discard it.
		_ = r.cmd.Wait()
	})

	return nil
}

// RemoveContainer removes a container.
func RemoveContainer(name string, force bool) error {
	args := []string{"rm"}

	if force {
		args = append(args, "-f")
	}

	args = append(args, name)

	cmd := exec.Command("docker", args...)

	return cmd.Run()
}

// GetContainerPort extracts port from environment file.
func GetContainerPort(envFile string, defaultPort int) int {
	if !fileExists(envFile) {
		return defaultPort
	}

	content, err := os.ReadFile(envFile)

	if err != nil {
		return defaultPort
	}

	lines := strings.Split(string(content), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "PORT=") {
			portStr := strings.TrimSpace(strings.TrimPrefix(line, "PORT="))

			if port, err := strconv.Atoi(portStr); err == nil {
				return port
			}
		}
	}

	return defaultPort
}

// IsZeroDowntimeEnabled checks if zero downtime deployment is enabled.
func IsZeroDowntimeEnabled(envFile string) bool {
	if !fileExists(envFile) {
		return true
	}

	content, err := os.ReadFile(envFile)

	if err != nil {
		return true
	}

	lines := strings.Split(string(content), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "ZERO_DOWNTIME=") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "ZERO_DOWNTIME="))

			switch strings.ToLower(value) {
			case "0", "false", "no", "off", "n":
				return false
			case "1", "true", "yes", "on", "y":
				return true
			default:
				return true
			}
		}
	}

	return true
}

// WaitForContainerHealth waits for container to be healthy.
func WaitForContainerHealth(name string, timeout int) error {
	startTime := time.Now()
	maxWait := time.Duration(timeout) * time.Second

	fmt.Printf("-----> Waiting for container to be healthy (max %ds)...\n", timeout)

	for {
		if time.Since(startTime) > maxWait {
			return fmt.Errorf("container failed to become healthy within %ds", timeout)
		}

		cmd := exec.Command("docker", "inspect", name, "--format", "{{.State.Health.Status}}")
		output, err := cmd.Output()
		if err != nil {
			time.Sleep(3 * time.Second)
			fmt.Println("-----> Container ready (no health check configured)")

			return nil
		}

		status := strings.TrimSpace(string(output))
		elapsed := int(time.Since(startTime).Seconds())

		switch status {
		case "healthy":
			fmt.Println("-----> Container is healthy!")

			return nil
		case "starting":
			fmt.Printf("       Starting... (%d/%ds)\n", elapsed, timeout)
			time.Sleep(2 * time.Second)
		case "unhealthy":
			logCmd := exec.Command("docker", "logs", name)
			logOutput, _ := logCmd.Output()

			return fmt.Errorf("container is unhealthy, logs: %s", string(logOutput))
		default:
			time.Sleep(2 * time.Second)
		}
	}
}

// StandardDeploy performs standard deployment (kill and restart).
func StandardDeploy(cfg DeploymentConfig) error {
	fmt.Println("=====> Starting Standard Deployment")

	containerName := cfg.AppName

	if ContainerExists(containerName) {
		fmt.Printf("-----> Stopping old container: %s\n", containerName)

		if err := StopContainer(containerName); err != nil {
			fmt.Printf("Warning: Failed to stop container: %v\n", err)
		}

		if err := RemoveContainer(containerName, true); err != nil {
			fmt.Printf("Warning: Failed to remove container: %v\n", err)
		}

		time.Sleep(2 * time.Second)
	}

	containerPort := GetContainerPort(cfg.EnvFile, 0)

	containerConfig := ContainerConfig{
		Name:          containerName,
		Image:         fmt.Sprintf("%s:%s", cfg.AppName, cfg.ImageTag),
		NetworkMode:   cfg.NetworkMode,
		RestartPolicy: "no",
		WorkingDir:    "/app",
		Volumes:       []string{fmt.Sprintf("%s:/app", cfg.ReleaseDir)},
	}

	if len(cfg.Volumes) > 0 {
		containerConfig.Volumes = append(containerConfig.Volumes, cfg.Volumes...)
		fmt.Printf("-----> Adding %d custom volumes\n", len(cfg.Volumes))
	}

	if cfg.NetworkMode != "host" {
		if len(cfg.DockerPorts) > 0 {
			containerConfig.Ports = cfg.DockerPorts
			fmt.Println("-----> Using ports from voodu.yml")
		} else if containerPort > 0 {
			containerConfig.Ports = []string{fmt.Sprintf("%d:%d", containerPort, containerPort)}
		}
	} else {
		fmt.Println("-----> Using host network (all ports exposed)")
	}

	if fileExists(cfg.EnvFile) {
		containerConfig.EnvFile = cfg.EnvFile
	}

	fmt.Printf("-----> Starting new container: %s\n", containerName)

	if err := CreateContainer(containerConfig); err != nil {
		return fmt.Errorf("failed to start container: %v", err)
	}

	fmt.Println("-----> Waiting for container to be ready...")
	time.Sleep(5 * time.Second)

	if !ContainerIsRunning(containerName) {
		logCmd := exec.Command("docker", "logs", containerName)
		logOutput, _ := logCmd.Output()

		return fmt.Errorf("container failed to start, logs: %s", string(logOutput))
	}

	fmt.Println("=====> Standard Deployment Complete!")
	fmt.Printf("-----> Active container: %s\n", containerName)
	fmt.Printf("-----> Running image: %s\n", cfg.ImageTag)

	return nil
}

// BlueGreenDeploy performs blue/green deployment.
func BlueGreenDeploy(cfg DeploymentConfig) error {
	fmt.Println("=====> Starting Blue/Green Deployment")

	containerPort := GetContainerPort(cfg.EnvFile, 0)

	if err := startGreenContainer(cfg, containerPort); err != nil {
		return fmt.Errorf("failed to start green container: %v", err)
	}

	if err := WaitForContainerHealth(cfg.AppName+"-green", cfg.HealthTimeout); err != nil {
		StopContainer(cfg.AppName + "-green")
		RemoveContainer(cfg.AppName+"-green", true)

		return fmt.Errorf("green container failed health check: %v", err)
	}

	activeContainerName := cfg.AppName

	if ContainerExists(activeContainerName) {
		if err := switchTrafficBlueToGreen(cfg.AppName, containerPort); err != nil {
			StopContainer(cfg.AppName + "-green")
			RemoveContainer(cfg.AppName+"-green", true)

			return fmt.Errorf("failed to switch traffic: %v", err)
		}

		cleanupOldBlueContainer(cfg.AppName)
	} else {
		fmt.Println("-----> First deployment, activating green")

		if err := renameContainer(cfg.AppName+"-green", activeContainerName); err != nil {
			return fmt.Errorf("failed to rename green to active: %v", err)
		}

		updateContainerRestartPolicy(activeContainerName, "always")
	}

	fmt.Println("=====> Blue/Green Deployment Complete!")
	fmt.Printf("-----> Active container: %s\n", activeContainerName)
	fmt.Printf("-----> Running image: %s\n", cfg.ImageTag)

	return nil
}

// DeployContainer determines and executes deployment strategy.
func DeployContainer(cfg DeploymentConfig) error {
	if IsZeroDowntimeEnabled(cfg.EnvFile) {
		fmt.Println("=====> ZERO_DOWNTIME deployment enabled")

		return BlueGreenDeploy(cfg)
	}

	fmt.Println("=====> ZERO_DOWNTIME deployment disabled")

	return StandardDeploy(cfg)
}

func fileExists(filename string) bool {
	_, err := os.Stat(filename)

	return !os.IsNotExist(err)
}

func startGreenContainer(cfg DeploymentConfig, containerPort int) error {
	greenName := cfg.AppName + "-green"
	fmt.Printf("-----> Starting green container: %s\n", greenName)

	if ContainerExists(greenName) {
		fmt.Println("       Removing old green container...")
		StopContainer(greenName)
		RemoveContainer(greenName, true)
	}

	containerConfig := ContainerConfig{
		Name:          greenName,
		Image:         fmt.Sprintf("%s:%s", cfg.AppName, cfg.ImageTag),
		NetworkMode:   cfg.NetworkMode,
		RestartPolicy: "unless-stopped",
		WorkingDir:    "/app",
		Volumes:       []string{fmt.Sprintf("%s:/app", cfg.ReleaseDir)},
	}

	if len(cfg.Volumes) > 0 {
		containerConfig.Volumes = append(containerConfig.Volumes, cfg.Volumes...)
		fmt.Printf("-----> Adding %d custom volumes to green container\n", len(cfg.Volumes))
	}

	if cfg.NetworkMode != "host" {
		if len(cfg.DockerPorts) > 0 {
			containerConfig.Ports = cfg.DockerPorts
		} else {
			containerConfig.Ports = []string{fmt.Sprintf("%d:%d", containerPort, containerPort)}
		}
	}

	if fileExists(cfg.EnvFile) {
		containerConfig.EnvFile = cfg.EnvFile
	}

	if err := CreateContainer(containerConfig); err != nil {
		return fmt.Errorf("failed to start green container: %v", err)
	}

	fmt.Printf("-----> Green container started (%s)\n", greenName)

	return nil
}

func switchTrafficBlueToGreen(appName string, containerPort int) error {
	activeName := appName
	greenName := appName + "-green"

	fmt.Println("-----> Switching traffic: active → green")

	if ContainerIsRunning(activeName) {
		fmt.Println("       Pausing active container...")
		cmd := exec.Command("docker", "pause", activeName)
		cmd.Run()
		time.Sleep(2 * time.Second)
	}

	fmt.Println("       Swapping container names...")

	if ContainerExists(activeName) {
		renameContainer(activeName, activeName+"-old")
	}

	if err := renameContainer(greenName, activeName); err != nil {
		return fmt.Errorf("failed to rename green container to active: %v", err)
	}

	updateContainerRestartPolicy(activeName, "always")

	fmt.Println("-----> Traffic switch complete (green → active)")

	return nil
}

func cleanupOldBlueContainer(appName string) {
	oldActiveName := appName + "-old"

	fmt.Println("-----> Cleaning up old active container...")

	if ContainerExists(oldActiveName) {
		fmt.Println("       Waiting 5s before removing old container...")
		time.Sleep(5 * time.Second)

		fmt.Println("       Removing old active container...")
		StopContainer(oldActiveName)
		RemoveContainer(oldActiveName, true)

		fmt.Println("-----> Old container cleaned up")
	}
}

func renameContainer(oldName, newName string) error {
	cmd := exec.Command("docker", "rename", oldName, newName)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("failed to rename container %s to %s: %v, output: %s", oldName, newName, err, string(output))
	}

	return nil
}

func updateContainerRestartPolicy(containerName, policy string) {
	cmd := exec.Command("docker", "update", "--restart", policy, containerName)
	cmd.Run()
}

// RecreateActiveContainer recreates the active container with a new env
// file. Used when secrets rotate — docker reads --env-file at container
// create time only, so a plain `docker restart` would not pick up the
// change; we have to destroy and recreate.
//
// Image, network mode, volumes, and port mappings are preserved from the
// running container via `docker inspect`. That's deliberate: the live
// container is the source of truth for "what's currently serving", and
// re-reading a declarative spec here risks diverging from what actually
// got deployed.
func RecreateActiveContainer(appName, envFile, appDir string) error {
	var activeContainer string

	if ContainerExists(appName) {
		activeContainer = appName
	} else if ContainerExists(appName + "-green") {
		activeContainer = appName + "-green"
	} else {
		return fmt.Errorf("no active container found for %s", appName)
	}

	fmt.Printf("-----> Recreating container: %s\n", activeContainer)

	inspected, err := inspectContainerShape(activeContainer)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", activeContainer, err)
	}

	fmt.Printf("       Using image: %s\n", inspected.Image)
	fmt.Printf("       Network mode: %s\n", inspected.NetworkMode)

	fmt.Println("       Stopping old container...")

	StopContainer(activeContainer)
	RemoveContainer(activeContainer, true)

	containerConfig := ContainerConfig{
		Name:          activeContainer,
		Image:         inspected.Image,
		NetworkMode:   inspected.NetworkMode,
		RestartPolicy: "always",
		WorkingDir:    "/app",
		Volumes:       inspected.Volumes,
		Ports:         inspected.Ports,
	}

	if len(containerConfig.Volumes) == 0 {
		containerConfig.Volumes = []string{fmt.Sprintf("%s:/app", appDir)}
	}

	if fileExists(envFile) {
		containerConfig.EnvFile = envFile
	}

	fmt.Println("       Starting new container with updated configuration...")

	if err := CreateContainer(containerConfig); err != nil {
		return fmt.Errorf("failed to recreate container: %v", err)
	}

	fmt.Println("Container recreated successfully with new environment")

	return nil
}

// containerShape captures the bits of a live container we need to
// recreate it identically minus its env.
type containerShape struct {
	Image       string
	NetworkMode string
	Ports       []string
	Volumes     []string
}

// inspectContainerShape pulls image, network mode, port bindings and
// bind-mount volumes out of a running container via `docker inspect`.
func inspectContainerShape(name string) (*containerShape, error) {
	cmd := exec.Command("docker", "inspect", name)

	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var raw []struct {
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`

		HostConfig struct {
			NetworkMode  string                           `json:"NetworkMode"`
			Binds        []string                         `json:"Binds"`
			PortBindings map[string][]struct{ HostIP, HostPort string } `json:"PortBindings"`
		} `json:"HostConfig"`
	}

	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, fmt.Errorf("parse docker inspect: %w", err)
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("no container data for %s", name)
	}

	entry := raw[0]

	shape := &containerShape{
		Image:       entry.Config.Image,
		NetworkMode: entry.HostConfig.NetworkMode,
		Volumes:     entry.HostConfig.Binds,
	}

	if shape.NetworkMode == "" {
		shape.NetworkMode = "bridge"
	}

	for containerSpec, bindings := range entry.HostConfig.PortBindings {
		containerPort := strings.SplitN(containerSpec, "/", 2)[0]

		for _, b := range bindings {
			if b.HostPort == "" {
				continue
			}

			if b.HostIP != "" {
				shape.Ports = append(shape.Ports, fmt.Sprintf("%s:%s:%s", b.HostIP, b.HostPort, containerPort))
			} else {
				shape.Ports = append(shape.Ports, fmt.Sprintf("%s:%s", b.HostPort, containerPort))
			}
		}
	}

	return shape, nil
}

// BlueGreenRollback performs rollback to previous blue container.
func BlueGreenRollback(appName string) error {
	blueName := appName + "-blue"
	oldBlueName := appName + "-blue-old"

	fmt.Println("=====> Starting Blue/Green Rollback")

	if !ContainerExists(oldBlueName) {
		return fmt.Errorf("no previous blue container found for rollback")
	}

	fmt.Println("-----> Stopping current blue container...")
	StopContainer(blueName)

	fmt.Println("-----> Restoring previous blue container...")

	if err := renameContainer(oldBlueName, blueName); err != nil {
		return fmt.Errorf("failed to restore previous blue container: %v", err)
	}

	fmt.Println("-----> Starting previous blue container...")
	cmd := exec.Command("docker", "start", blueName)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return fmt.Errorf("failed to start previous blue container: %v, output: %s", err, string(output))
	}

	time.Sleep(5 * time.Second)

	fmt.Println("=====> Blue/Green Rollback Complete!")
	fmt.Printf("-----> Active container: %s\n", blueName)

	return nil
}
