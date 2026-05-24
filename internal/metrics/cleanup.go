package metrics

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cleanup is the file-retention + gzip-rotation pass. Safe to call
// every tick — the unlink/gzip is no-op when files are already in
// the desired state, so the cost is one ReadDir per tick (cheap).
//
// Two passes:
//
//  1. Gzip yesterday's `metrics-YYYY-MM-DD.ndjson` if it's
//     untouched (not the currently-open today file) and the .gz
//     doesn't already exist. ~85% size reduction is the win;
//     scanner-side `compress/gzip` reads it transparently.
//
//  2. Unlink anything (raw or .gz) whose date is older than
//     `now - retention`. Operator can drop in custom older files
//     under a different prefix and they're preserved (we only
//     touch `metrics-*` matches).
//
// Operates on `dir` directly — does NOT acquire the Writer's lock
// because the operations target FILES OTHER than the currently-
// open one (today's file). Day-rollover safety: the writer picks
// the file by sample.Ts so it never has yesterday's file open by
// the time we get here.
func Cleanup(dir string, now time.Time, retention time.Duration, logger Logger) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("cleanup read dir: %w", err)
	}

	today := now.UTC().Truncate(24 * time.Hour)
	cutoff := now.Add(-retention).UTC().Truncate(24 * time.Hour)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		date, ok := parseFilenameDate(e.Name())
		if !ok {
			continue
		}

		path := filepath.Join(dir, e.Name())

		// Pass 2 first — if it's already too old, we don't care
		// about gzipping it, just delete.
		if date.Before(cutoff) {
			if err := os.Remove(path); err != nil {
				if logger != nil {
					logger.Printf("metrics: cleanup remove %s: %v", path, err)
				}
			}

			continue
		}

		// Pass 1 — gzip yesterday's raw .ndjson if not already.
		// "Yesterday" = strictly before today. Today's file stays
		// raw because the writer has it open.
		if date.Before(today) && strings.HasSuffix(e.Name(), ".ndjson") {
			if err := gzipFile(path, logger); err != nil {
				if logger != nil {
					logger.Printf("metrics: cleanup gzip %s: %v", path, err)
				}
			}
		}
	}

	return nil
}

// gzipFile compresses `path` → `path + ".gz"` and removes the
// original on success. Atomic enough: we write to a `.gz.tmp` and
// rename, so a crash mid-compress leaves the original untouched
// + a stray .tmp the next cleanup pass cleans up itself (it
// won't match the `metrics-*` filename predicate, so it's safe to
// leave; or operator can rm manually).
//
// No-op when `path + ".gz"` already exists — we've already
// compressed this file, the original may have been hand-restored
// by the operator; do nothing.
func gzipFile(path string, logger Logger) error {
	gzPath := path + ".gz"

	if _, err := os.Stat(gzPath); err == nil {
		// Already compressed. Remove the original (idempotent).
		return os.Remove(path)
	}

	tmp := gzPath + ".tmp"

	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	defer out.Close()

	in, err := os.Open(path)
	if err != nil {
		return err
	}

	defer in.Close()

	gz := gzip.NewWriter(out)

	if _, err := io.Copy(gz, in); err != nil {
		return err
	}

	if err := gz.Close(); err != nil {
		return err
	}

	if err := out.Sync(); err != nil {
		return err
	}

	if err := os.Rename(tmp, gzPath); err != nil {
		return err
	}

	if err := os.Remove(path); err != nil {
		if logger != nil {
			logger.Printf("metrics: gzip rename ok but original remove failed for %s: %v", path, err)
		}
	}

	return nil
}
