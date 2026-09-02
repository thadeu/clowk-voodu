package activity

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultRetention is the cleanup window when the operator hasn't overridden
// via --activity-retention / VOODU_ACTIVITY_RETENTION.
//
// 30 days, against the 7 that metrics keeps, because the volume and the useful
// window are both different: a human acts on a box dozens of times a day, not
// thousands of times an hour, so a month of activity fits in what a day of
// metrics occupies — and "what changed last month" is a question people ask,
// while "what was the CPU a month ago" is not.
//
// This is the OPERATOR's number, never the licence's. voodu-webui keeps that
// distinction deliberately (see app/services/retention.rb): an expired licence
// must not shrink the window and delete somebody's trail.
const DefaultRetention = 30 * 24 * time.Hour

// Cleanup is the retention + gzip-rotation pass. Safe to call on a timer: the
// gzip and unlink are no-ops once files are in the desired state, so the cost
// is one ReadDir.
//
// Two passes, mirroring internal/metrics/cleanup.go:
//
//  1. Gzip yesterday's file if it isn't already. Today's stays raw because the
//     writer holds it open.
//  2. Unlink anything (raw or .gz) older than now - retention.
//
// Operates on `dir` directly and does NOT take the Writer's lock: everything it
// touches is a file OTHER than the currently-open one. The writer picks its
// file by the record's own timestamp, so by the time we get here it never has
// yesterday's file open.
//
// Only touches `activity-*` names — files an operator dropped in are preserved.
func Cleanup(dir string, now time.Time, retention time.Duration, logger Logger) error {
	if retention <= 0 {
		retention = DefaultRetention
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("activity cleanup read dir: %w", err)
	}

	today := now.UTC().Truncate(24 * time.Hour)
	cutoff := now.Add(-retention).UTC().Truncate(24 * time.Hour)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		raw, ok := ParseFileDate(e.Name())
		if !ok {
			continue
		}

		date, err := time.Parse(dateLayout, raw)
		if err != nil {
			continue
		}

		path := filepath.Join(dir, e.Name())

		// Too old wins over "should be gzipped" — no point compressing
		// something we are about to delete.
		if date.Before(cutoff) {
			if err := os.Remove(path); err != nil && logger != nil {
				logger.Printf("activity: cleanup remove %s: %v", path, err)
			}

			continue
		}

		if date.Before(today) && strings.HasSuffix(e.Name(), ".ndjson") {
			if err := gzipFile(path, logger); err != nil && logger != nil {
				logger.Printf("activity: cleanup gzip %s: %v", path, err)
			}
		}
	}

	return nil
}

// gzipFile compresses path → path+".gz" and removes the original on success.
//
// Writes a .tmp and renames, so a crash mid-compress leaves the original
// untouched plus a stray .tmp — which does not match the activity-* predicate
// and is therefore never mistaken for history.
//
// No-op when the .gz already exists: we compressed this once already, and the
// original may have been restored by hand.
func gzipFile(path string, logger Logger) error {
	gzPath := path + ".gz"

	if _, err := os.Stat(gzPath); err == nil {
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

	if err := os.Remove(path); err != nil && logger != nil {
		logger.Printf("activity: gzip ok but removing %s failed: %v", path, err)
	}

	return nil
}
