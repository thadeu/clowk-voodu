package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Writer appends NDJSON samples to per-UTC-day files. Thread-safe
// for a SINGLE concurrent writer (the sampler goroutine) — multiple
// concurrent writers would interleave bytes within a line, so the
// sampler must be the only producer.
//
// Atomicity contract: each WriteSystem / WritePod call builds the
// whole line as `[]byte` and issues exactly one `(*os.File).Write`
// against an `O_APPEND` fd. On Linux ext4/xfs, the kernel holds
// the inode lock for the duration of the write call, so the line
// lands as one atomic blob — a concurrent reader will see either
// nothing or the complete line, never a half line.
//
// Why NOT bufio.Writer: a buffered writer may split a single
// formatted line at the buffer boundary into TWO write() syscalls,
// breaking the atomicity guarantee above. Same reason we don't use
// json.Encoder.Encode against the file directly (that issues two
// writes: the object then a "\n").
//
// fsync policy: we do NOT fsync per sample. Per-sample fsync would
// add ~30 fsyncs/min on a cheap VPS SSD, serialising against
// kernel writeback for negligible safety gain (metrics are
// inherently lossy on crash — losing the last 30s of samples is
// acceptable). We DO fsync on file rollover (new UTC day) and on
// graceful shutdown via Close().
type Writer struct {
	dir string

	mu       sync.Mutex
	openDate string   // YYYY-MM-DD of the currently-open file
	openFile *os.File // nil when not yet opened
	logger   Logger
}

// Logger is the minimal subset of *log.Logger we use. Caller may
// pass nil to silence logs (tests).
type Logger interface {
	Printf(format string, args ...any)
}

// NewWriter returns a Writer rooted at dir. Creates the directory
// (and parents) if missing. Doesn't open the day file yet — the
// first WriteSystem / WritePod call does that based on its `ts`.
func NewWriter(dir string, logger Logger) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("metrics dir: %w", err)
	}

	return &Writer{dir: dir, logger: logger}, nil
}

// WriteSystem appends one system row. The file is selected by
// `s.Ts.UTC()` (not by "current open handle"), so a tick that
// crosses midnight UTC writes to tomorrow's file — no race with
// the cleanup pass that may rename/gzip yesterday's.
func (w *Writer) WriteSystem(s SystemSample) error {
	s.Source = SourceSystem

	line, err := marshalLine(s)
	if err != nil {
		return fmt.Errorf("marshal system: %w", err)
	}

	return w.appendLine(s.Ts.UTC(), line)
}

// WritePod appends one pod row. Same day-by-ts selection as
// WriteSystem; see that comment.
func (w *Writer) WritePod(s PodSample) error {
	s.Source = SourcePod

	line, err := marshalLine(s)
	if err != nil {
		return fmt.Errorf("marshal pod: %w", err)
	}

	return w.appendLine(s.Ts.UTC(), line)
}

// WriteIngress appends one ingress (HTTP metrics) row. Same day-by-ts
// selection as WriteSystem / WritePod; all three sources share the
// daily NDJSON file so the reader can stream once for queries that
// touch multiple sources.
func (w *Writer) WriteIngress(s IngressSample) error {
	s.Source = SourceIngress

	line, err := marshalLine(s)
	if err != nil {
		return fmt.Errorf("marshal ingress: %w", err)
	}

	return w.appendLine(s.Ts.UTC(), line)
}

// marshalLine builds a complete NDJSON line as one []byte —
// trailing newline included — so the caller can issue a single
// Write() against the file. See Writer doc for why this matters.
func marshalLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, len(b)+1)
	out = append(out, b...)
	out = append(out, '\n')

	return out, nil
}

// appendLine ensures the right day file is open, then writes the
// line in one syscall. On ENOSPC we drop the sample (logged once)
// — a full disk should not crash the controller.
func (w *Writer) appendLine(ts time.Time, line []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.ensureOpenForLocked(ts); err != nil {
		return err
	}

	if _, err := w.openFile.Write(line); err != nil {
		// ENOSPC is the one case we tolerate without surfacing —
		// the controller has other responsibilities and crashing
		// because metrics ran out of disk would be worse. Log
		// once-per-tick at most (the caller already throttles).
		if errors.Is(err, fs.ErrExist) || isENOSPC(err) {
			if w.logger != nil {
				w.logger.Printf("metrics: drop sample, write failed: %v", err)
			}

			return nil
		}

		return fmt.Errorf("append: %w", err)
	}

	return nil
}

// ensureOpenForLocked rotates the open file handle if the sample's
// UTC date differs from the currently-open file's date. Caller
// must hold w.mu.
//
// On rotation we fsync the previous file before closing — this is
// the one place where durability matters because the file is about
// to be subject to the cleanup pass (gzip + eventual unlink), and
// we don't want a kernel-cached-but-not-on-disk tail to be lost.
func (w *Writer) ensureOpenForLocked(ts time.Time) error {
	date := ts.Format("2006-01-02")

	if w.openFile != nil && w.openDate == date {
		return nil
	}

	if w.openFile != nil {
		_ = w.openFile.Sync()
		_ = w.openFile.Close()

		w.openFile = nil
	}

	path := filepath.Join(w.dir, "metrics-"+date+".ndjson")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}

	w.openFile = f
	w.openDate = date

	return nil
}

// Close fsyncs + closes the open file. Idempotent. Called from the
// controller's teardown so the last few samples in the kernel page
// cache hit disk before shutdown.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.openFile == nil {
		return nil
	}

	_ = w.openFile.Sync()
	err := w.openFile.Close()
	w.openFile = nil
	w.openDate = ""

	return err
}
