package metrics

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"syscall"
	"time"
)

// IngressSampler tails the Caddy access log file, aggregates requests
// over each Tick window per (host, scope, name) deployment identity,
// and writes one NDJSON line per non-empty bucket to the metrics
// store. Mirrors the existing Sampler pattern (Run loop, immediate
// first eval, ctx-cancel teardown) so lifecycle wiring in
// internal/controller/server.go can spawn both side-by-side without
// special-casing.
//
// Why on-tick file scan instead of fsnotify-driven streaming:
//   - One goroutine, one lock, no event-vs-tick race.
//   - 15s Tick is already our cadence ceiling (aggregation window
//     can't be tighter than this without losing percentile accuracy).
//   - File I/O at tick time is bounded: scanner reads from the saved
//     position to EOF, which on a quiet site is bytes and on a busy
//     site is at most one second's worth of accumulated lines.
//   - Avoids a dep on github.com/fsnotify/fsnotify just to save ~1s
//     of latency on the first sample after a request.
//
// Rotation handling: each Tick we Stat the path. If the inode
// changed (Caddy rotated the file via lumberjack's rename + new
// file) we close the old fd and reopen from the start of the new
// file. First-ever open seeks to EOF — we don't want to backfill
// the entire historical log as "now" samples.
type IngressSampler struct {
	LogPath string
	Tick    time.Duration
	Now     func() time.Time
	Hosts   HostResolver
	Writer  *Writer
	Logger  Logger

	// tail state — owned by the Run goroutine; no external access.
	file        *os.File
	inode       uint64
	firstOpen   bool // true until the very first open; controls EOF seek
	agg         *IngressAggregator
}

// Run blocks until ctx is cancelled. Each tick: refresh host resolver,
// ingest any new lines from the access log into the aggregator, drain
// + emit one NDJSON row per non-empty (host, scope, name) bucket.
func (s *IngressSampler) Run(ctx context.Context) {
	tick := s.Tick
	if tick <= 0 {
		tick = DefaultInterval
	}

	if s.Now == nil {
		s.Now = time.Now
	}

	if s.agg == nil {
		s.agg = NewIngressAggregator()
	}

	s.firstOpen = true

	t := time.NewTicker(tick)
	defer t.Stop()

	// Immediate first eval so the bucket starts filling within a
	// tick of controller boot (matches Sampler's pattern).
	s.evaluate()

	for {
		select {
		case <-ctx.Done():
			s.closeFile()
			return

		case <-t.C:
			s.evaluate()
		}
	}
}

func (s *IngressSampler) evaluate() {
	// Resolver refresh first — if a new ingress was just declared,
	// requests received in this window need its mapping.
	if r, ok := s.Hosts.(interface{ Refresh() error }); ok {
		if err := r.Refresh(); err != nil {
			s.logf("ingress: refresh hosts: %v", err)
		}
	}

	if err := s.ingestNewLines(); err != nil {
		s.logf("ingress: ingest: %v", err)
	}

	s.emit(s.Now().UTC())
}

// ingestNewLines opens the access log if needed, reads from the
// saved position to EOF, parses each line, and pushes matching
// requests into the aggregator. Lines whose host is unmanaged
// (no matching ingress) are skipped silently.
func (s *IngressSampler) ingestNewLines() error {
	if s.LogPath == "" {
		return nil
	}

	if err := s.ensureFileFresh(); err != nil {
		return err
	}

	if s.file == nil {
		// File didn't exist this tick (Caddy not running, no apps
		// applied, path mismatch). Quiet skip — next tick retries.
		return nil
	}

	scanner := bufio.NewScanner(s.file)
	// Bump buffer ceiling — Caddy access lines with full headers can
	// easily exceed the 64 KB default (Cookie + User-Agent + JWT etc).
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		req, ok := parseCaddyLine(line)
		if !ok {
			// Malformed JSON (partial line during rotation, log corruption).
			// Skip silently; logging every bad line would spam.
			continue
		}

		scope, name, ok := s.Hosts.Lookup(req.Host)
		if !ok {
			// Host not declared as a voodu ingress — could be a
			// stray request to localhost, IP-only access, or a host
			// that was deleted while traffic was in-flight. Skip.
			continue
		}

		s.agg.Push(req.Host, scope, name, req)
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

// ensureFileFresh opens the file if not open; if open, checks for
// inode change (rotation) and reopens from the start of the new file.
// First-ever open seeks to EOF so we don't backfill historical
// requests as "now" samples — that'd lie about traffic spikes.
func (s *IngressSampler) ensureFileFresh() error {
	fi, err := os.Stat(s.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.closeFile()
			return nil
		}

		return err
	}

	currentInode := inodeOf(fi)

	if s.file != nil && s.inode == currentInode {
		// Same file, same fd — bufio.Scanner picks up new bytes
		// from the current offset on next .Scan() call.
		return nil
	}

	// First open OR rotation. Close any prior fd, open the new file.
	s.closeFile()

	f, err := os.Open(s.LogPath)
	if err != nil {
		return err
	}

	if s.firstOpen {
		// Don't backfill the historical log; seek to EOF.
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			_ = f.Close()
			return err
		}

		s.firstOpen = false
	}
	// Post-rotation (s.firstOpen already false): the new file just
	// got created by Caddy's rotator, it has zero prior content, so
	// reading from offset 0 is correct.

	s.file = f
	s.inode = currentInode

	return nil
}

func (s *IngressSampler) closeFile() {
	if s.file == nil {
		return
	}

	_ = s.file.Close()
	s.file = nil
	s.inode = 0
}

// emit drains the aggregator and writes one IngressSample per
// non-empty bucket. Zero-count buckets are not written — saves disk
// AND avoids the "0 ms p99 looks real on the chart" trap. A deployment
// with no recent traffic shows "no data" on its HTTP cards (the WebUI
// distinguishes from "down" via the resource metrics that keep flowing).
func (s *IngressSampler) emit(ts time.Time) {
	if s.Writer == nil {
		// Drain anyway so the buckets don't accumulate forever.
		_ = s.agg.Drain()
		return
	}

	for k, b := range s.agg.Drain() {
		if b.count == 0 {
			continue
		}

		row := IngressSample{
			Ts:       ts,
			Source:   SourceIngress,
			Host:     k.host,
			Scope:    k.scope,
			Name:     k.name,
			ReqCount: b.count,
			Req2xx:   b.s2xx,
			Req3xx:   b.s3xx,
			Req4xx:   b.s4xx,
			Req5xx:   b.s5xx,
			BytesOut: b.bytesOut,
		}

		row.LatencyP50Ms, row.LatencyP90Ms, row.LatencyP95Ms, row.LatencyP99Ms, row.LatencyMaxMs = b.Percentiles()

		if err := s.Writer.WriteIngress(row); err != nil {
			s.logf("ingress: write %s: %v", k.host, err)
		}
	}
}

func (s *IngressSampler) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}

// parseCaddyLine extracts just the four fields we care about from
// one Caddy v2 JSON access log line. Tolerant to extra fields
// (User-Agent headers, resp_headers, etc.) which Caddy includes by
// default — they're just ignored.
//
// Duration is converted seconds → milliseconds here so the aggregator
// and the wire format both speak ms (matches the units the WebUI
// charts label).
func parseCaddyLine(line []byte) (IngressRequest, bool) {
	var raw struct {
		Request struct {
			Host string `json:"host"`
			URI  string `json:"uri"`
		} `json:"request"`
		Duration float64 `json:"duration"`
		Status   int     `json:"status"`
		Size     uint64  `json:"size"`
	}

	if err := json.Unmarshal(line, &raw); err != nil {
		return IngressRequest{}, false
	}

	if raw.Request.Host == "" {
		// Lines without a host can't be mapped to a deployment.
		// Caddy's startup/config events lack `request.host`; this
		// branch also catches those if they ever leak past the
		// include filter on the access logger.
		return IngressRequest{}, false
	}

	return IngressRequest{
		Host:       raw.Request.Host,
		URI:        raw.Request.URI,
		DurationMs: raw.Duration * 1000.0,
		Status:     raw.Status,
		SizeBytes:  raw.Size,
	}, true
}

// inodeOf extracts the file inode from a FileInfo. Linux + macOS
// both expose this via syscall.Stat_t. On other platforms we'd
// return 0, which would force "always rotated" behaviour — a degraded
// but not broken fallback (we'd reopen the file every tick, paying
// the cost of one extra open + close per Tick).
func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}

	return 0
}
