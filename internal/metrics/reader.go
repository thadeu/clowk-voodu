package metrics

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// QueryOpts is the input to Query. The handler at the HTTP layer
// translates its query-string params into one of these.
type QueryOpts struct {
	Dir    string
	Source Source        // SourceSystem or SourcePod
	Metric string        // e.g. "cpu_percent" — see metricExtractors
	Scope  string        // optional, pod source only
	Name   string        // optional, pod source only (resource name)
	Pod    string        // optional, pod source only — exact-match on
	// the docker container name (which is also how
	// `vd logs` / `vd describe` address a pod). When
	// set, narrows further than (scope, name) which
	// aggregates across replicas. Lets the WebUI show
	// per-replica charts when the operator clicks a
	// replica chip.
	//
	// The NDJSON row stores this under the `container`
	// key (daemon-truth field name); the wire param
	// `?pod=` reads naturally for operators because
	// "pod" is voodu's surface vocabulary.
	Start    time.Time     // inclusive
	End      time.Time     // exclusive (typically Now)
	Interval time.Duration // bucket size; the handler decides "auto"
	Now      func() time.Time
}

// QueryResult is what the handler serialises back to the WebUI.
type QueryResult struct {
	Metric          string    `json:"metric"`
	IntervalSeconds int       `json:"interval_seconds"`
	AvailableFrom   time.Time `json:"available_from"`
	Truncated       bool      `json:"truncated"`
	Series          []Point   `json:"series"`

	// Latest is the SINGLE most recent unaggregated sample inside
	// the query window. Distinct from `Series[len-1]` because the
	// series points are bucket aggregates (avg across an interval),
	// so on long ranges (6h, 24h, 7d) the last bucket smooths the
	// real current value with N preceding samples.
	//
	// Callers (WebUI headline, CLI `vd metrics --current`) read
	// `Latest` to get a value that's stable across ranges — picking
	// 1h vs 6h vs 7d shouldn't change "what's CPU right now".
	//
	// nil when the query window has zero samples.
	Latest *Point `json:"latest,omitempty"`
}

// Point is one chart point — the bucket midpoint timestamp and the
// average of all samples that fell in that bucket. Value uses
// float64 because some metrics (CPU%) are inherently float.
type Point struct {
	Ts    time.Time `json:"ts"`
	Value float64   `json:"value"`
}

// MaxBuckets caps the points returned by any single query. The
// handler's "auto" interval picker honours this; explicit-interval
// queries are clamped to this length too. 300 is the sweet spot
// for SVG sparkline rendering on the WebUI side.
const MaxBuckets = 300

// metricExtractor reads one field from a parsed line into a float64.
// Returning a separate `ok` bool lets us distinguish "field absent"
// (skip the sample, don't count toward the bucket) from "field
// present and zero" (count it).
type metricExtractor func(raw map[string]json.RawMessage) (val float64, ok bool)

// metricExtractors is the allow-list of metrics the API exposes.
// Anything not in this map errors at the handler — prevents
// arbitrary field access via the query string.
//
// Why a fixed allow-list instead of accepting any field name?
//  1. Type safety — we know each field is numeric.
//  2. Stable API surface — the WebUI / CLI only learn about
//     metrics we explicitly publish.
//  3. The wire shape of NDJSON lines can evolve (add private
//     debug fields) without leaking them through /metrics.
var metricExtractors = map[string]metricExtractor{
	// System metrics.
	"cpu_percent":      uintOrFloatField("cpu_percent"),
	"mem_used_bytes":   uintOrFloatField("mem_used_bytes"),
	"mem_total_bytes":  uintOrFloatField("mem_total_bytes"),
	"disk_used_bytes":  uintOrFloatField("disk_used_bytes"),
	"disk_total_bytes": uintOrFloatField("disk_total_bytes"),

	// Pod metrics.
	"mem_usage_bytes":         uintOrFloatField("mem_usage_bytes"),
	"mem_limit_bytes":         uintOrFloatField("mem_limit_bytes"),
	"net_rx_bytes":            uintOrFloatField("net_rx_bytes"),
	"net_tx_bytes":            uintOrFloatField("net_tx_bytes"),
	"net_rx_delta_bytes":      uintOrFloatField("net_rx_delta_bytes"),
	"net_tx_delta_bytes":      uintOrFloatField("net_tx_delta_bytes"),
	"block_read_bytes":        uintOrFloatField("block_read_bytes"),
	"block_write_bytes":       uintOrFloatField("block_write_bytes"),
	"block_read_delta_bytes":  uintOrFloatField("block_read_delta_bytes"),
	"block_write_delta_bytes": uintOrFloatField("block_write_delta_bytes"),
}

// uintOrFloatField returns an extractor that accepts both JSON
// integers (uint64) and floats (CPU%). Most metrics are uints; CPU%
// is float. Single helper handles both shapes cleanly.
func uintOrFloatField(key string) metricExtractor {
	return func(raw map[string]json.RawMessage) (float64, bool) {
		v, ok := raw[key]
		if !ok || len(v) == 0 {
			return 0, false
		}

		var f float64
		if err := json.Unmarshal(v, &f); err != nil {
			return 0, false
		}

		return f, true
	}
}

// MetricAllowed returns true if the metric name is in the
// extractor table. Handler-level guard so a bad query string
// fails cleanly with 400 instead of returning an empty series.
func MetricAllowed(metric string) bool {
	_, ok := metricExtractors[metric]
	return ok
}

// Query is the public entry point. Streams the right files for the
// time range, parses only what the metric needs, buckets, returns.
//
// Memory budget: O(buckets) — never O(samples). A 7d query at 1m
// bucket = 10_080 ints; we clamp to MaxBuckets so it's bounded.
// Per-line parsing decodes into a small map[string]json.RawMessage
// (~12 keys, ~200B per line), then the extractor pulls the one
// field we care about; no decoded struct lingers.
func Query(opts QueryOpts) (QueryResult, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.End.IsZero() {
		opts.End = opts.Now()
	}

	if opts.Interval <= 0 {
		return QueryResult{}, fmt.Errorf("interval must be positive")
	}

	extractor, ok := metricExtractors[opts.Metric]
	if !ok {
		return QueryResult{}, fmt.Errorf("unknown metric: %q", opts.Metric)
	}

	if opts.End.Before(opts.Start) {
		return QueryResult{}, fmt.Errorf("end before start")
	}

	bucketCount := int(opts.End.Sub(opts.Start) / opts.Interval)
	if bucketCount <= 0 {
		bucketCount = 1
	}

	if bucketCount > MaxBuckets {
		bucketCount = MaxBuckets
	}

	type bucket struct {
		sum   float64
		count int
	}

	buckets := make([]bucket, bucketCount)

	// latest tracks the single most recent sample in the window so
	// the response can carry a "current value" that's stable across
	// range choices. See QueryResult.Latest doc.
	var (
		latestTs    time.Time
		latestVal   float64
		latestSeen  bool
	)

	files, available, err := listFiles(opts.Dir, opts.Start, opts.End)
	if err != nil {
		return QueryResult{}, err
	}

	// Stream every file in chronological order. Cheap on hot cache
	// (most queries hit yesterday + today only).
	for _, path := range files {
		if err := streamFile(path, opts, extractor, func(ts time.Time, val float64) {
			// Track the latest sample regardless of bucket bounds
			// — but only within the query window (extractor + ts
			// filter in streamFile already guarantees that).
			if !latestSeen || ts.After(latestTs) {
				latestTs = ts
				latestVal = val
				latestSeen = true
			}

			idx := int(ts.Sub(opts.Start) / opts.Interval)
			// Bucket-bounds guard — clock skew (NTP step) could
			// produce ts < start or ts > end. Drop those points
			// instead of crashing or wrapping.
			if idx < 0 || idx >= len(buckets) {
				return
			}

			buckets[idx].sum += val
			buckets[idx].count++
		}); err != nil {
			// Don't fail the whole query on one bad file — log via
			// the bucket count drop instead. The caller's logger
			// is at the handler level.
			continue
		}
	}

	series := make([]Point, 0, len(buckets))

	for i := range buckets {
		if buckets[i].count == 0 {
			continue
		}

		series = append(series, Point{
			Ts:    opts.Start.Add(time.Duration(i) * opts.Interval),
			Value: buckets[i].sum / float64(buckets[i].count),
		})
	}

	truncated := !available.IsZero() && available.After(opts.Start)

	result := QueryResult{
		Metric:          opts.Metric,
		IntervalSeconds: int(opts.Interval / time.Second),
		AvailableFrom:   available,
		Truncated:       truncated,
		Series:          series,
	}

	if latestSeen {
		result.Latest = &Point{Ts: latestTs, Value: latestVal}
	}

	return result, nil
}

// listFiles returns the metrics files whose UTC date overlaps
// [start, end], in chronological order. Also returns the
// "available_from" timestamp — the earliest UTC midnight present
// on disk, used by the handler to signal `truncated`.
//
// Matches both `.ndjson` (today) and `.ndjson.gz` (rolled-over
// historical files); streamFile dispatches on the extension.
func listFiles(dir string, start, end time.Time) (files []string, available time.Time, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// First-boot or after manual nuke — empty result is
			// fine; the query returns an empty series.
			return nil, time.Time{}, nil
		}

		return nil, time.Time{}, fmt.Errorf("read dir: %w", err)
	}

	startDay := start.UTC().Truncate(24 * time.Hour)
	endDay := end.UTC().Truncate(24 * time.Hour)

	type stamped struct {
		path string
		day  time.Time
	}

	var picked []stamped

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		date, ok := parseFilenameDate(e.Name())
		if !ok {
			continue
		}

		// available tracks earliest file on disk, regardless of
		// whether it falls in the query window.
		if available.IsZero() || date.Before(available) {
			available = date
		}

		// File's day must overlap the query day range. We compare
		// midnight-truncated days so a 23:59 sample in yesterday's
		// file is still picked up by a query starting at 23:00.
		if date.Before(startDay) || date.After(endDay) {
			continue
		}

		picked = append(picked, stamped{
			path: filepath.Join(dir, e.Name()),
			day:  date,
		})
	}

	sort.Slice(picked, func(i, j int) bool {
		return picked[i].day.Before(picked[j].day)
	})

	out := make([]string, len(picked))
	for i, s := range picked {
		out[i] = s.path
	}

	return out, available, nil
}

// parseFilenameDate recognises `metrics-YYYY-MM-DD.ndjson` and
// `.ndjson.gz`. Returns the file's UTC midnight Time + ok flag.
// Other filenames in the dir are ignored (operator's notes,
// backups, etc.).
func parseFilenameDate(name string) (time.Time, bool) {
	const prefix = "metrics-"

	if !strings.HasPrefix(name, prefix) {
		return time.Time{}, false
	}

	base := strings.TrimPrefix(name, prefix)
	base = strings.TrimSuffix(base, ".gz")
	base = strings.TrimSuffix(base, ".ndjson")

	t, err := time.Parse("2006-01-02", base)
	if err != nil {
		return time.Time{}, false
	}

	return t.UTC(), true
}

// streamFile reads one ndjson file (transparently decompressing
// .gz), applies the source/scope/name filter, extracts the metric,
// and calls emit per kept sample.
//
// Tolerates partial-line corruption (the dominant failure mode is
// a crash mid-write): if json.Unmarshal fails on a line, we skip
// it and continue.
func streamFile(path string, opts QueryOpts, extractor metricExtractor, emit func(time.Time, float64)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}

	defer f.Close()

	var r io.Reader = f

	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("gzip %s: %w", path, err)
		}

		defer gz.Close()

		r = gz
	}

	sc := bufio.NewScanner(r)
	// Default Scanner buffer is 64 KB; bump to 256 KB so the
	// rare 100 KB+ line (stack-trace-in-env, weird tag) doesn't
	// crash the read.
	sc.Buffer(make([]byte, 0, 4096), 256*1024)

	wantSource := string(opts.Source)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			// Partial-line corruption (crash mid-write). Skip
			// silently — logging every malformed line would spam
			// the controller log after every crash recovery.
			continue
		}

		// Cheap pre-filter — source mismatch skips the rest of the
		// per-line work.
		if !matchString(raw, "source", wantSource) {
			continue
		}

		if opts.Source == SourcePod {
			if opts.Scope != "" && !matchString(raw, "scope", opts.Scope) {
				continue
			}

			if opts.Name != "" && !matchString(raw, "name", opts.Name) {
				continue
			}

			// Pod filter — exact match on the row's `container`
			// field (docker daemon's identifier; same key voodu's
			// /pods/{name} uses). Narrows to ONE replica when set;
			// ignored when blank so (scope, name) aggregation still
			// works for "show me this deployment as a whole" queries.
			if opts.Pod != "" && !matchString(raw, "container", opts.Pod) {
				continue
			}
		}

		ts, ok := extractTs(raw)
		if !ok {
			continue
		}

		if ts.Before(opts.Start) || !ts.Before(opts.End) {
			continue
		}

		val, ok := extractor(raw)
		if !ok {
			continue
		}

		emit(ts, val)
	}

	// sc.Err() — bufio.ErrTooLong handled by the Buffer call above.
	// Anything else is propagated so the caller logs.
	return sc.Err()
}

// matchString — true when raw[key] decoded as a string equals want.
// Tolerant of missing / non-string fields (returns false, doesn't
// error) so unexpected line shapes just get filtered out.
func matchString(raw map[string]json.RawMessage, key, want string) bool {
	v, ok := raw[key]
	if !ok {
		return false
	}

	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return false
	}

	return s == want
}

// extractTs parses the canonical RFC3339 ts. Returns zero+false on
// any issue (skip the line).
func extractTs(raw map[string]json.RawMessage) (time.Time, bool) {
	v, ok := raw["ts"]
	if !ok {
		return time.Time{}, false
	}

	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return time.Time{}, false
	}

	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}

	return t.UTC(), true
}
