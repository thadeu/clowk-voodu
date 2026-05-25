package metrics

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// DumpOpts is the input to Dump. The HTTP layer translates its
// `?since=<unix_ts>` query param into a Time here.
type DumpOpts struct {
	Dir   string
	Since time.Time // emit only lines with ts > Since
	Now   func() time.Time
}

// Dump streams raw NDJSON lines newer than opts.Since to w, in
// chronological order across daily files. Writes the on-disk bytes
// verbatim — no parse / re-serialise — except for the per-line ts
// filter which decodes only the `ts` field.
//
// This is the public entry point for the WebUI's local metrics
// warehouse sync. The WebUI runs an incremental pull every N
// seconds against `/metrics/dump?since=<last_seen_ts>` and persists
// each emitted line as one row in its local SQLite (payload JSON +
// virtual generated columns indexing the hot identity fields).
//
// Why expose the raw NDJSON instead of a structured "give me a
// curated payload" envelope?
//
//   - Single source of truth for the wire shape: the WebUI commits
//     to the same line format the on-disk sampler emits. Future
//     additions to the per-line schema (new metric field) reach the
//     warehouse without controller-side handler changes.
//   - Cheap on the controller — no aggregation, no bucket math, no
//     extractor allow-list pass. Just "files since X, lines whose ts > X".
//   - The WebUI's JSON storage (payload as TEXT) is already the
//     natural sink for this shape.
//
// Caller is responsible for response headers (Content-Type, etc.).
// On any per-file error (gzip corruption, partial line in the
// middle of a file) the file is skipped and the next is tried —
// a partial dump beats a 500 because the next tick's Since stays
// at the last successfully-emitted ts.
func Dump(w io.Writer, opts DumpOpts) error {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	// When Since is zero (backfill / cold WebUI warehouse), default
	// to the controller's typical retention window. listFiles caps
	// at what's actually on disk, so older windows just produce
	// fewer files (no error).
	start := opts.Since
	if start.IsZero() {
		start = opts.Now().Add(-7 * 24 * time.Hour)
	}

	files, _, err := listFiles(opts.Dir, start, opts.Now())
	if err != nil {
		return err
	}

	// Buffer so per-line Write calls don't syscall. Caller wraps a
	// streaming HTTP response; bufio amortises while still flushing
	// regularly enough that the WebUI sees bytes within a tick.
	bw := bufio.NewWriterSize(w, 64*1024)

	defer bw.Flush()

	for _, path := range files {
		if err := dumpFile(path, opts.Since, bw); err != nil {
			// Per-file degradation: skip and continue. The WebUI's
			// next tick keeps Since at the last ts it successfully
			// stored, so the bad-file lines (if any) will be retried.
			continue
		}
	}

	return nil
}

// dumpFile streams one ndjson file (transparently decompressing
// .gz), forwarding bytes verbatim for any line whose `ts` is > since.
//
// We decode each line's ts (cheap one-field unmarshal) to filter,
// but the rest of the line is passed through. The full per-line
// parse from reader.go (which also extracts the metric value and
// identity fields) is unnecessary here — the WebUI parses the same
// line on the ingest side.
func dumpFile(path string, since time.Time, bw *bufio.Writer) error {
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
	// Match reader.go's bump from the 64 KB default — the rare
	// 100 KB+ line (env_from with a huge value, weird tag) doesn't
	// crash the scan.
	sc.Buffer(make([]byte, 0, 4096), 256*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		// Decode JUST the ts field for the filter. Reuses extractTs
		// from reader.go so the ts-format tolerance behaviour stays
		// identical between Query and Dump.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			// Partial-line corruption (crash mid-write). Skip the
			// line and continue — logging every malformed line would
			// spam after every crash recovery.
			continue
		}

		ts, ok := extractTs(raw)
		if !ok {
			continue
		}

		// Strictly greater-than so a client requesting since=X
		// doesn't re-fetch the row it already canonically holds at
		// ts=X. WebUI computes since = MAX(ts_epoch) and keeps that
		// row in its warehouse.
		if !ts.After(since) {
			continue
		}

		if _, err := bw.Write(line); err != nil {
			return err
		}

		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}

	return sc.Err()
}
