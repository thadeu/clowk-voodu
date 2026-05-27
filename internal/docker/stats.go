// stats.go uses the official Docker Go SDK (github.com/moby/moby/client)
// to collect per-container runtime utilisation. Previously we shelled out
// to `docker stats --no-stream --format json` per call, which carried two
// kinds of overhead:
//
//   1. fork + exec per invocation — process spawn, pipe wiring, text
//      parsing on the way back.
//   2. burst load on dockerd — all N containers sampled in one batch
//      caused periodic CPU spikes (visible as cyclical 100% bumps on
//      the host's CPU chart).
//
// The SDK eliminates (1) entirely (HTTP keep-alive to the docker socket,
// typed JSON decode), and the per-container staggered loop here
// transforms (2) from a single tall spike into a flat plateau spread
// across the polling window.
//
// We deliberately do NOT talk to /sys/fs/cgroup directly because the
// path differs between cgroup v1 and v2 and across distros — the daemon
// already handles that diversity, so we let it.
//
// Single-shot semantics (`Stream: false, IncludePreviousSample: true`):
// daemon takes two samples 1s apart to compute CPU% delta, then returns.
// Same fidelity as the old `--no-stream` CLI flag — just without the
// fork+exec tax.

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// ContainerStats is the typed view of one container's runtime
// utilisation, normalised to numeric fields the caller can render or
// alert on.
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

	// ID is the short container id (12-char prefix).
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
	// declared, docker reports the host's total memory here. Callers
	// comparing usage to a manifest cap should not trust this field
	// at face value.
	MemLimitBytes uint64 `json:"mem_limit_bytes"`

	// MemPercent is MemUsageBytes / MemLimitBytes × 100. Convenience
	// — keeps callers free of unit math.
	MemPercent float64 `json:"mem_percent"`

	// NetRxBytes / NetTxBytes are CUMULATIVE network counters since
	// container start, summed across all interfaces. Same value
	// `docker stats` shows in its NET I/O column.
	NetRxBytes uint64 `json:"net_rx_bytes,omitempty"`
	NetTxBytes uint64 `json:"net_tx_bytes,omitempty"`

	// BlockReadBytes / BlockWriteBytes are CUMULATIVE block-device
	// I/O counters since container start — `docker stats` "BLOCK I/O"
	// column. Sourced from cgroup blkio stats (cgroup v1) or the
	// `io_service_bytes_recursive` slice (cgroup v2).
	BlockReadBytes  uint64 `json:"block_read_bytes,omitempty"`
	BlockWriteBytes uint64 `json:"block_write_bytes,omitempty"`

	// PIDs is the current number of processes in the container's
	// cgroup. Useful for spotting fork-bomb regressions or process-
	// pool misconfiguration. Sourced from cgroup pids.current.
	PIDs uint64 `json:"pids,omitempty"`
}

// StatsClient is the seam controller-side collectors dispatch through.
// Production wires DockerStatsClient (uses the SDK); tests substitute
// a fake to assert join behaviour without docker on the box.
type StatsClient interface {
	ContainerStats(names []string) ([]ContainerStats, error)
}

// DockerStatsClient is the production StatsClient. Stateless from the
// caller's perspective; internally caches the docker SDK client across
// calls (HTTP keep-alive at the socket).
type DockerStatsClient struct{}

// StaggerPerSampleMin is the floor between two consecutive single-
// container stat requests. Spreads CPU/IO load on the docker daemon
// across the polling window instead of bursting all N requests at
// once. 200ms × 10 pods = 2s of staggered work — well under the
// sampler's 60s tick budget.
//
// Override via VOODU_STATS_STAGGER_MS for tuning without rebuild.
var StaggerPerSampleMin = parseEnvDurationMs("VOODU_STATS_STAGGER_MS", 200*time.Millisecond)

// StatsTimeout caps the total time a single ContainerStats batch can
// take. Each per-container call also costs ~1s (daemon's two-sample
// pause to compute CPU% delta), so 30s comfortably covers ~25 pods
// even with the stagger above.
const StatsTimeout = 30 * time.Second

// dockerClient is the package-level singleton SDK client. Lazy-init on
// first use; subsequent calls reuse the connection. Safe to share
// across goroutines (the underlying HTTP client is concurrency-safe).
var (
	dockerClient     *client.Client
	dockerClientErr  error
	dockerClientOnce sync.Once
)

func getDockerClient() (*client.Client, error) {
	dockerClientOnce.Do(func() {
		// `FromEnv` honours DOCKER_HOST + DOCKER_API_VERSION etc.
		// `WithAPIVersionNegotiation` lets the SDK auto-pick the
		// highest version both sides support — avoids the
		// "client version newer than daemon" error on older hosts.
		// API-version negotiation is now the default per moby v28 SDK,
		// so we don't need WithAPIVersionNegotiation explicitly. FromEnv
		// still honours DOCKER_HOST + DOCKER_TLS_VERIFY etc.
		dockerClient, dockerClientErr = client.New(client.FromEnv)
	})

	return dockerClient, dockerClientErr
}

// ContainerStats samples each named container in turn via the docker
// SDK and returns the typed results. Containers are processed serially
// with a short stagger between requests — daemon-side that means one
// CPU/Mem read at a time instead of an N-wide burst, smoothing the
// dockerd CPU profile.
//
// Behaviours preserved from the previous CLI shim:
//   - missing names are silently omitted (container vanished between
//     list + stat; matches `docker stats foo` returning what it can)
//   - stopped containers are absent from the output (no live cgroup
//     to sample)
//   - returns entries in input order (callers needing canonical order
//     should sort themselves)
//
// Empty names slice returns empty slice — the SDK path requires
// explicit container IDs (no "all running" shortcut here; if needed,
// caller passes the names from ListContainers).
func (DockerStatsClient) ContainerStats(names []string) ([]ContainerStats, error) {
	if len(names) == 0 {
		return nil, nil
	}

	cli, err := getDockerClient()
	if err != nil {
		return nil, fmt.Errorf("docker client init: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), StatsTimeout)
	defer cancel()

	out := make([]ContainerStats, 0, len(names))

	for i, name := range names {
		// Stagger between samples (skip before the first). Daemon
		// gets to breathe between reads — concurrent calls were the
		// source of the periodic CPU spikes.
		if i > 0 && StaggerPerSampleMin > 0 {
			select {
			case <-time.After(StaggerPerSampleMin):
			case <-ctx.Done():
				return out, ctx.Err()
			}
		}

		stats, err := sampleOne(ctx, cli, name)
		if err != nil {
			// Skip + continue — one missing/stopped container shouldn't
			// fail the whole batch. The CLI version had the same
			// behaviour (docker stats silently omits missing names).
			continue
		}

		out = append(out, stats)
	}

	return out, nil
}

// sampleOne fetches a single container's stats via the SDK + converts
// to our ContainerStats shape. The daemon takes two samples 1s apart
// (because IncludePreviousSample=true) to compute the CPU% delta; this
// is the same cost the old `--no-stream` CLI flag carried.
func sampleOne(ctx context.Context, cli *client.Client, name string) (ContainerStats, error) {
	result, err := cli.ContainerStats(ctx, name, client.ContainerStatsOptions{
		Stream:                false,
		IncludePreviousSample: true, // required for CPU% delta calculation
	})
	if err != nil {
		return ContainerStats{}, fmt.Errorf("container stats %s: %w", name, err)
	}

	defer result.Body.Close()

	var raw container.StatsResponse
	if err := json.NewDecoder(result.Body).Decode(&raw); err != nil {
		return ContainerStats{}, fmt.Errorf("decode stats %s: %w", name, err)
	}

	return convertStats(raw, name), nil
}

// convertStats turns the SDK's StatsResponse into our internal
// ContainerStats shape. CPU% derivation matches what `docker stats`
// itself computes (see daemon/stats/collector.go in moby/moby for the
// canonical formula).
func convertStats(raw container.StatsResponse, fallbackName string) ContainerStats {
	name := raw.Name
	if name == "" {
		name = fallbackName
	}

	// Docker prefixes names with "/" (e.g. "/fsw-controller.8879");
	// strip it so the value lines up with what `docker ps` shows in
	// the NAMES column (and what voodu uses everywhere else as the
	// natural key).
	name = strings.TrimPrefix(name, "/")

	id := raw.ID
	if len(id) > 12 {
		id = id[:12]
	}

	cpuPercent := calcCPUPercent(raw.CPUStats, raw.PreCPUStats)

	memUsage := raw.MemoryStats.Usage
	// Subtract page cache so the number matches `docker stats` (which
	// surfaces RSS, not Usage). Cache lives in MemoryStats.Stats — key
	// is "cache" on cgroup v1, "file" on cgroup v2.
	if cache, ok := raw.MemoryStats.Stats["cache"]; ok {
		memUsage = subSat(memUsage, cache)
	} else if file, ok := raw.MemoryStats.Stats["file"]; ok {
		memUsage = subSat(memUsage, file)
	}

	memLimit := raw.MemoryStats.Limit

	var memPercent float64
	if memLimit > 0 {
		memPercent = float64(memUsage) / float64(memLimit) * 100
	}

	var rx, tx uint64
	for _, n := range raw.Networks {
		rx += n.RxBytes
		tx += n.TxBytes
	}

	var blkRead, blkWrite uint64
	for _, e := range raw.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			blkRead += e.Value
		case "write":
			blkWrite += e.Value
		}
	}

	return ContainerStats{
		Name:            name,
		ID:              id,
		CPUPercent:      cpuPercent,
		MemUsageBytes:   memUsage,
		MemLimitBytes:   memLimit,
		MemPercent:      memPercent,
		NetRxBytes:      rx,
		NetTxBytes:      tx,
		BlockReadBytes:  blkRead,
		BlockWriteBytes: blkWrite,
		PIDs:            raw.PidsStats.Current,
	}
}

// calcCPUPercent — same formula `docker stats` uses internally
// (see daemon/stats/collector_unix.go: calculateCPUPercentUnix).
//
//	cpuDelta    = current.TotalUsage - pre.TotalUsage
//	systemDelta = current.SystemUsage - pre.SystemUsage
//	percent     = (cpuDelta / systemDelta) × onlineCPUs × 100
//
// Returns 0 when systemDelta is 0 (cold-start, or pre sample missing).
// OnlineCPUs falls back to len(PercpuUsage) for older daemons that
// don't populate the field directly.
func calcCPUPercent(cur, pre container.CPUStats) float64 {
	cpuDelta := float64(cur.CPUUsage.TotalUsage) - float64(pre.CPUUsage.TotalUsage)
	systemDelta := float64(cur.SystemUsage) - float64(pre.SystemUsage)
	if systemDelta <= 0 || cpuDelta < 0 {
		return 0
	}

	onlineCPUs := float64(cur.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(cur.CPUUsage.PercpuUsage))
	}

	if onlineCPUs == 0 {
		onlineCPUs = 1
	}

	return (cpuDelta / systemDelta) * onlineCPUs * 100
}

// subSat — saturating subtraction (no underflow). Page cache CAN
// exceed Usage in rare daemon races; clamp to 0 instead of wrapping.
func subSat(a, b uint64) uint64 {
	if b >= a {
		return 0
	}

	return a - b
}

// parseEnvDurationMs reads an env var as integer milliseconds. Returns
// fallback on empty/unparseable. Used for tuning the stagger without
// a controller rebuild.
func parseEnvDurationMs(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}

	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}

	return time.Duration(n) * time.Millisecond
}
