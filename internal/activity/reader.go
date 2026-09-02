package activity

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

// maxLineBytes caps one NDJSON line. A record carrying hundreds of resources is
// still far under this; the cap exists so a corrupt file cannot make the reader
// allocate without bound.
const maxLineBytes = 1 << 20

// DumpOpts is the input to Dump. The HTTP layer turns `?since=<unix_ts>` into
// a Time here.
type DumpOpts struct {
	Dir   string
	Since time.Time // emit only lines with ts > Since
}

// Dump streams raw NDJSON lines newer than opts.Since to w, chronologically
// across daily files. Writes on-disk bytes verbatim — no parse and
// re-serialise — except for the per-line ts filter, which decodes only `ts`.
//
// This is what the WebUI's warehouse sync pulls, exactly like /metrics/dump:
// the wire shape and the on-disk shape are the same thing, so a new field
// reaches the warehouse without a controller-side change.
func Dump(w io.Writer, opts DumpOpts) error {
	files, err := dayFiles(opts.Dir)
	if err != nil {
		return err
	}

	for _, f := range files {
		if err := dumpFile(w, f.path, opts.Since); err != nil {
			return err
		}
	}

	return nil
}

// QueryOpts filters records for human-facing reads.
//
// Every filter is optional; the zero value means "everything in retention".
// Limit defaults to DefaultQueryLimit — an unbounded query against 30 days of
// history is never what a caller wanted.
type QueryOpts struct {
	Dir string

	Start time.Time // inclusive; zero means oldest
	End   time.Time // exclusive; zero means now

	Actions  []Action
	Origins  []Origin
	Statuses []Status
	Scope    string
	Kind     string
	Name     string

	Limit int
}

// DefaultQueryLimit bounds a query that did not ask for one.
const DefaultQueryLimit = 200

// Query returns matching records, NEWEST FIRST — the order a history screen
// reads in, and the order that makes Limit mean "the most recent N" rather than
// "the oldest N of thirty days".
func Query(opts QueryOpts) ([]Record, error) {
	if opts.Limit <= 0 {
		opts.Limit = DefaultQueryLimit
	}

	files, err := dayFiles(opts.Dir)
	if err != nil {
		return nil, err
	}

	// Newest file first, and stop as soon as Limit is met: a screen asking for
	// the last 50 actions should not read thirty days off disk.
	out := make([]Record, 0, opts.Limit)

	for i := len(files) - 1; i >= 0; i-- {
		recs, err := queryFile(files[i].path, opts)
		if err != nil {
			return nil, err
		}

		// queryFile returns chronological; reverse into newest-first.
		for j := len(recs) - 1; j >= 0; j-- {
			out = append(out, recs[j])

			if len(out) >= opts.Limit {
				return out, nil
			}
		}
	}

	return out, nil
}

type dayFile struct {
	date string
	path string
}

// dayFiles lists activity files in chronological order. A missing directory is
// not an error: a controller that has never recorded anything has nothing to
// show, which is a valid empty answer rather than a failure.
func dayFiles(dir string) ([]dayFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("activity read dir: %w", err)
	}

	out := make([]dayFile, 0, len(entries))

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		date, ok := ParseFileDate(e.Name())
		if !ok {
			continue
		}

		out = append(out, dayFile{date: date, path: filepath.Join(dir, e.Name())})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].date < out[j].date })

	return out, nil
}

// ParseFileDate extracts YYYY-MM-DD from an activity filename, raw or gzipped.
// Reports false for anything that is not ours — an operator's own files under a
// different prefix are left alone by every pass.
func ParseFileDate(name string) (string, bool) {
	if !strings.HasPrefix(name, FilePrefix) {
		return "", false
	}

	rest := strings.TrimPrefix(name, FilePrefix)
	rest = strings.TrimSuffix(rest, ".gz")

	if !strings.HasSuffix(rest, ".ndjson") {
		return "", false
	}

	date := strings.TrimSuffix(rest, ".ndjson")

	if _, err := time.Parse(dateLayout, date); err != nil {
		return "", false
	}

	return date, true
}

// openDayFile opens a file, transparently decompressing a .gz. Callers must
// close both returned closers in order.
func openDayFile(path string) (io.ReadCloser, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}

	if !strings.HasSuffix(path, ".gz") {
		return io.NopCloser(f), f, nil
	}

	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()

		return nil, nil, fmt.Errorf("activity gzip %s: %w", path, err)
	}

	return gz, f, nil
}

func scanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	return s
}

// tsOnly decodes just the timestamp, so Dump can filter without paying to
// unmarshal a whole record.
type tsOnly struct {
	Ts time.Time `json:"ts"`
}

func dumpFile(w io.Writer, path string, since time.Time) error {
	rc, f, err := openDayFile(path)
	if err != nil {
		return err
	}

	defer f.Close()
	defer rc.Close()

	sc := scanner(rc)

	for sc.Scan() {
		line := sc.Bytes()

		if len(line) == 0 {
			continue
		}

		var probe tsOnly

		// A malformed line is skipped, never fatal. A truncated tail from a
		// crash must not make the whole history unreadable.
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}

		if !since.IsZero() && !probe.Ts.After(since) {
			continue
		}

		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}

	return sc.Err()
}

func queryFile(path string, opts QueryOpts) ([]Record, error) {
	rc, f, err := openDayFile(path)
	if err != nil {
		return nil, err
	}

	defer f.Close()
	defer rc.Close()

	var out []Record

	sc := scanner(rc)

	for sc.Scan() {
		line := sc.Bytes()

		if len(line) == 0 {
			continue
		}

		var rec Record

		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}

		if matches(rec, opts) {
			out = append(out, rec)
		}
	}

	return out, sc.Err()
}

func matches(rec Record, opts QueryOpts) bool {
	if !opts.Start.IsZero() && rec.Ts.Before(opts.Start) {
		return false
	}

	if !opts.End.IsZero() && !rec.Ts.Before(opts.End) {
		return false
	}

	if opts.Scope != "" && rec.Scope != opts.Scope {
		return false
	}

	if opts.Kind != "" && rec.Kind != opts.Kind {
		return false
	}

	if opts.Name != "" && rec.Name != opts.Name {
		return false
	}

	if len(opts.Actions) > 0 && !containsAction(opts.Actions, rec.Action) {
		return false
	}

	if len(opts.Origins) > 0 && !containsOrigin(opts.Origins, rec.Origin) {
		return false
	}

	// A status filter implies "finished or done": a `started` row has no status
	// and matching it against one would be comparing against a field the row
	// does not have yet.
	if len(opts.Statuses) > 0 && !containsStatus(opts.Statuses, rec.Status) {
		return false
	}

	return true
}

func containsAction(list []Action, v Action) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}

	return false
}

func containsOrigin(list []Origin, v Origin) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}

	return false
}

func containsStatus(list []Status, v Status) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}

	return false
}
