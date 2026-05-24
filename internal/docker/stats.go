// stats.go wraps `docker stats --no-stream --format json` so callers
// get a Go-typed view of container CPU/memory/network/block-io
// utilisation without parsing the human-formatted columns themselves.
//
// `docker stats` is the only reliable source for live cgroup-level
// usage on a single host. The daemon samples cgroup files internally;
// our wrapper just shells out, normalises units, and returns structs.
// We deliberately do NOT talk to /sys/fs/cgroup directly because the
// path differs between cgroup v1 and v2 and across distros — the
// docker daemon already handles that diversity, so we let it.
//
// Single-shot only (`--no-stream`): each call takes ~1-2s because
// the daemon needs to sample twice to compute CPU%. Callers that
// need refresh are expected to poll.

package docker

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ContainerStats is the typed view of one container's runtime
// utilisation, normalised to numeric fields the caller can render or
// alert on. Strings like "112.1MiB / 1.921GiB" from docker are split
// into the underlying byte counts here so downstream code stays free
// of unit-parsing concerns.
//
// "Limit" fields reflect the docker daemon's view (cgroup memory
// limit, --cpus quota). When no limit was set at `docker run`, docker
// reports the host's total memory as the limit — semantically "no
// effective cap." Callers comparing usage to a *configured* manifest
// limit should pull that from the resource spec separately; this
// struct stays purely runtime-shaped.
type ContainerStats struct {
	// Name is the docker container name (without leading slash).
	// Same identifier used by ListContainers and InspectLabels — the
	// caller joins on this to recover voodu's structured identity.
	Name string `json:"name"`

	// ID is the short container id docker emits in `--format json`
	// (12-char prefix). Useful when the caller wants to escape into
	// `docker logs <id>` for debugging.
	ID string `json:"id,omitempty"`

	// CPUPercent is the percentage of available CPU resources the
	// container is using, relative to the host's CPU count (matches
	// `docker stats` default semantics). 100% means one full core; on
	// a 4-core host the theoretical max is 400%.
	CPUPercent float64 `json:"cpu_percent"`

	// MemUsageBytes is the resident set size docker reports for the
	// container's cgroup. Excludes page cache; this is the "memory in
	// use" number that triggers OOM kills.
	MemUsageBytes uint64 `json:"mem_usage_bytes"`

	// MemLimitBytes is the cgroup memory limit. When no limit was
	// configured, docker reports the host total memory — the caller
	// can detect "no effective cap" by comparing this to the host's
	// physical memory.
	MemLimitBytes uint64 `json:"mem_limit_bytes"`

	// MemPercent is MemUsageBytes / MemLimitBytes * 100, as docker
	// computes it. Kept as a separate field rather than derived in Go
	// so we match `docker stats` output verbatim — operators reading
	// both should see the same number.
	MemPercent float64 `json:"mem_percent"`

	// PIDs is the process count inside the container. Useful for
	// detecting fork bombs / runaway worker pools.
	PIDs int `json:"pids,omitempty"`

	// NetRxBytes / NetTxBytes are CUMULATIVE network counters since
	// container start — same numbers `docker stats` shows in its
	// "NET I/O" column ("338kB / 41.7kB"). Wraps to zero on container
	// restart, NOT on cgroup-level interface flap (the daemon
	// resets the counter only when the container itself restarts).
	//
	// Cumulative chosen over rate so the controller is stateless
	// per-call. Rate (bytes/sec) would need per-container baseline
	// tracking; a future enhancement layered on top — not v1.
	NetRxBytes uint64 `json:"net_rx_bytes,omitempty"`
	NetTxBytes uint64 `json:"net_tx_bytes,omitempty"`

	// BlockReadBytes / BlockWriteBytes are CUMULATIVE block-device
	// I/O counters since container start — `docker stats` "BLOCK I/O"
	// column. Useful for spotting "this container is hammering the
	// disk" without per-cgroup syscall tracing.
	//
	// Read/Write include only block-device I/O (page cache hits are
	// invisible here, by design — same semantics as `iostat`).
	BlockReadBytes  uint64 `json:"block_read_bytes,omitempty"`
	BlockWriteBytes uint64 `json:"block_write_bytes,omitempty"`
}

// StatsClient is the seam controller-side collectors dispatch through.
// Production wires DockerStatsClient (shells out); tests substitute
// a fake to assert join behaviour without docker on the box.
//
// The slice argument is an opt-in filter: pass nil/empty to fetch all
// running containers, or specific names to narrow the call. Docker
// honours the filter daemon-side, which is faster than fetching all
// and filtering client-side on a busy host.
type StatsClient interface {
	ContainerStats(names []string) ([]ContainerStats, error)
}

// DockerStatsClient is the production StatsClient. Stateless; safe
// to share across goroutines.
type DockerStatsClient struct{}

// ContainerStats shells out `docker stats --no-stream --format json
// [names...]` and parses the line-delimited JSON output into typed
// structs. Returns the entries in the order docker emitted them
// (stable enough for tests; callers needing canonical order should
// sort themselves).
//
// When names is non-empty, docker filters daemon-side — a missing
// name is silently omitted rather than erroring (matches `docker
// stats foo bar` behaviour: it lists the ones it knows about).
//
// Stopped containers are absent from the output by design: `docker
// stats` only reports on running containers (their cgroup is the
// data source). Callers that need "config + zero usage" rows for
// stopped pods must inject those themselves.
func (DockerStatsClient) ContainerStats(names []string) ([]ContainerStats, error) {
	args := []string{"stats", "--no-stream", "--format", "{{json .}}"}
	args = append(args, names...)

	cmd := exec.Command("docker", args...)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker stats: %w", err)
	}

	return parseStatsOutput(output)
}

// parseStatsOutput converts the raw line-delimited JSON docker emits
// into typed ContainerStats. Each line is one container — empty lines
// (trailing newline) are skipped. Malformed lines are reported as
// errors rather than silently dropped because the caller's filter
// expectation might depend on counts matching the input slice.
func parseStatsOutput(raw []byte) ([]ContainerStats, error) {
	lines := strings.Split(string(raw), "\n")
	out := make([]ContainerStats, 0, len(lines))

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		stats, err := parseStatsLine(line)
		if err != nil {
			return nil, fmt.Errorf("docker stats line %d: %w", i+1, err)
		}

		out = append(out, stats)
	}

	return out, nil
}

// dockerStatsLine mirrors the raw `--format json` shape docker emits.
// Lives as a private struct (only used inside parseStatsLine) — the
// public API is ContainerStats, which carries the parsed numeric
// fields the rest of the codebase consumes.
type dockerStatsLine struct {
	Name      string `json:"Name"`
	ID        string `json:"ID"`
	Container string `json:"Container"` // long ID, unused
	CPUPerc   string `json:"CPUPerc"`   // "0.14%"
	MemUsage  string `json:"MemUsage"`  // "112.1MiB / 1.921GiB"
	MemPerc   string `json:"MemPerc"`   // "5.70%"
	NetIO     string `json:"NetIO"`     // "338kB / 41.7kB" — captured but unused
	BlockIO   string `json:"BlockIO"`   // "2.04GB / 1.21MB" — captured but unused
	PIDs      string `json:"PIDs"`      // "19"
}

func parseStatsLine(line string) (ContainerStats, error) {
	var raw dockerStatsLine

	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return ContainerStats{}, fmt.Errorf("decode json: %w", err)
	}

	cpu, err := parsePercent(raw.CPUPerc)
	if err != nil {
		return ContainerStats{}, fmt.Errorf("cpu_percent: %w", err)
	}

	memUsage, memLimit, err := parseDualBytes(raw.MemUsage)
	if err != nil {
		return ContainerStats{}, fmt.Errorf("mem_usage: %w", err)
	}

	memPerc, err := parsePercent(raw.MemPerc)
	if err != nil {
		return ContainerStats{}, fmt.Errorf("mem_percent: %w", err)
	}

	pids, _ := strconv.Atoi(strings.TrimSpace(raw.PIDs))

	// NetIO / BlockIO are formatted as "RX / TX" and "READ / WRITE"
	// respectively. Same "A / B" docker convention parseDualBytes
	// understands. Parse errors degrade to 0 — these are advisory
	// metrics, not the primary correctness signal of `docker stats`,
	// so a malformed NET column shouldn't fail the whole stats fetch.
	netRx, netTx, _ := parseDualBytes(raw.NetIO)
	blkRead, blkWrite, _ := parseDualBytes(raw.BlockIO)

	return ContainerStats{
		Name:            strings.TrimSpace(raw.Name),
		ID:              strings.TrimSpace(raw.ID),
		CPUPercent:      cpu,
		MemUsageBytes:   memUsage,
		MemLimitBytes:   memLimit,
		MemPercent:      memPerc,
		PIDs:            pids,
		NetRxBytes:      netRx,
		NetTxBytes:      netTx,
		BlockReadBytes:  blkRead,
		BlockWriteBytes: blkWrite,
	}, nil
}

// parsePercent strips the trailing "%" from a docker-emitted percent
// string and parses the leading number. Docker uses "--" when stats
// are unavailable for a stopped container (shouldn't happen with
// --no-stream filtering, but defensive); we map that to 0.
func parsePercent(s string) (float64, error) {
	s = strings.TrimSpace(s)

	if s == "" || s == "--" {
		return 0, nil
	}

	s = strings.TrimSuffix(s, "%")

	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}

	return v, nil
}

// parseDualBytes splits a docker-stats two-value column into byte
// counts. Three columns use the same shape:
//
//   - MemUsage  "112.1MiB / 1.921GiB"  → (used, limit)
//   - NetIO     "338kB / 41.7kB"       → (rx, tx)
//   - BlockIO   "2.04GB / 1.21MB"      → (read, write)
//
// Both sides honour docker's unit suffixes (B/KiB/MiB/GiB/TiB and
// their decimal kB/MB/GB/TB cousins). "--" appears for stopped
// containers — mapped to (0, 0), nil — and an empty string is also
// (0, 0). Malformed input (non-matching split, unknown unit) returns
// an error so callers can decide whether to degrade or fail.
func parseDualBytes(s string) (uint64, uint64, error) {
	s = strings.TrimSpace(s)

	if s == "" || s == "--" {
		return 0, 0, nil
	}

	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected A / B, got %q", s)
	}

	a, err := parseBytes(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("left: %w", err)
	}

	b, err := parseBytes(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("right: %w", err)
	}

	return a, b, nil
}

// memUnit maps docker's unit suffix vocabulary to byte multipliers.
// Docker prefers binary units (MiB, GiB) for memory and decimal
// (kB, MB, GB) for network/block I/O, but the parser accepts both
// so other callers (NetIO/BlockIO if surfaced later) can reuse it.
var memUnit = map[string]uint64{
	"":    1,
	"B":   1,
	"kB":  1000,
	"KB":  1000,
	"KiB": 1024,
	"MB":  1000 * 1000,
	"MiB": 1024 * 1024,
	"GB":  1000 * 1000 * 1000,
	"GiB": 1024 * 1024 * 1024,
	"TB":  1000 * 1000 * 1000 * 1000,
	"TiB": 1024 * 1024 * 1024 * 1024,
}

// parseBytes converts docker's human-formatted size string ("112.1MiB"
// / "1.921GiB" / "338kB") into an absolute byte count. Round-trips a
// 0.1MiB difference at the float64 level — docker's own precision —
// which is fine for display.
//
// Returns an error when the unit isn't recognised; callers should
// surface that rather than fall back to a misleading 0.
func parseBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	// Walk forward while we're still on a numeric character (digits,
	// dot, optional leading minus is rejected — sizes are non-neg).
	cutoff := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || c == '.' {
			cutoff = i + 1
			continue
		}

		break
	}

	numStr := s[:cutoff]
	unit := strings.TrimSpace(s[cutoff:])

	mult, ok := memUnit[unit]
	if !ok {
		return 0, fmt.Errorf("unknown unit %q in %q", unit, s)
	}

	v, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", numStr, err)
	}

	if v < 0 {
		return 0, fmt.Errorf("negative size %q", s)
	}

	return uint64(v * float64(mult)), nil
}
