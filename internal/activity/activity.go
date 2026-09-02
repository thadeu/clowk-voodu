// Package activity records what was DONE to this box — apply, restart, delete,
// rollback, config — as append-only NDJSON on disk.
//
// WHY IT EXISTS. The controller can say what is running: pods, metrics, logs,
// probes. It cannot say what was done. A CPU spike has a chart; the action that
// caused it has no record, and the most common cause of "it broke and nobody
// deployed" is a config value somebody changed at 14:02.
//
// It has to cover the apply an operator ran by hand, not only the ones a
// control plane triggered — otherwise the timeline answers "what did the
// console do" instead of "what happened to this box".
//
// NOT `deploy`, NOT `release`. internal/deploy is an app release: build the
// image, swap the `current` symlink, run hooks. ReleaseRecord is the version
// history of one deployment, with rollback. Both stay exactly as they are; this
// sits ALONGSIDE them, joined by ReleaseID — the status blob keeps release
// state (capped, for rollback), this keeps the timeline (30 days, for the
// screen). `journal` was avoided too: it already means the systemd journal in
// this codebase.
//
// The file format, rotation and atomicity rules are deliberately identical to
// internal/metrics — same daily NDJSON files, same one-Write-per-line contract,
// same gzip-then-unlink cleanup. Two NDJSON stores on one box that behaved
// differently would be two things to learn.
package activity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event distinguishes the three shapes an action can have.
//
// THREE values, not two, and that is load-bearing. Most actions are
// instantaneous — delete, rollback, config — and forcing them into a
// started/finished pair would be a lie about them. With only two values,
// "instantaneous action" and "action still running" become indistinguishable,
// and the screen's `active` filter reads one as the other.
//
// So: a lone `started` means in flight (or the controller died mid-action),
// and `done` means it was over the moment it was recorded.
type Event string

const (
	EventStarted  Event = "started"
	EventFinished Event = "finished"
	EventDone     Event = "done"
)

// Action is what the operator asked for.
type Action string

const (
	ActionApply        Action = "apply"
	ActionRestart      Action = "restart"
	ActionDelete       Action = "delete"
	ActionRollback     Action = "rollback"
	ActionConfigSet    Action = "config.set"
	ActionConfigDelete Action = "config.delete"
)

// Origin is who asked. Without it every row reads `api` and the history cannot
// answer "who did this", which is most of the point.
//
// NOT INFERABLE SERVER-SIDE: the controller cannot tell a local CLI from an
// SSH-forwarded one by looking at the request, so the caller declares it via
// OriginHeader. Unknown or absent normalises to OriginAPI — an unrecognised
// label must not invent a category.
type Origin string

const (
	OriginCLI         Origin = "cli"
	OriginSSH         Origin = "ssh"
	OriginReceivePack Origin = "receive_pack"
	OriginAPI         Origin = "api"
	OriginDeployPlane Origin = "deploy_plane"
)

// OriginHeader carries Origin from client to controller.
const OriginHeader = "X-Voodu-Origin"

// NormalizeOrigin maps a wire value to a known Origin, defaulting to OriginAPI.
func NormalizeOrigin(raw string) Origin {
	switch Origin(raw) {
	case OriginCLI, OriginSSH, OriginReceivePack, OriginDeployPlane, OriginAPI:
		return Origin(raw)
	default:
		return OriginAPI
	}
}

// Status of a finished action.
//
// `partial` exists because an apply of ten manifests that lands eight is
// neither success nor failure, and collapsing it into either one throws away
// the fact the operator most needs.
type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusPartial   Status = "partial"
)

// Resource is one manifest an action touched.
type Resource struct {
	Kind   string `json:"kind"`
	Scope  string `json:"scope,omitempty"`
	Name   string `json:"name"`
	Action string `json:"action,omitempty"` // created | updated | unchanged | deleted
}

// Record is one NDJSON line.
//
// No ETA field, deliberately. An ETA is not a fact about this action; it is a
// prediction from the median of recent actions of the same (scope, kind).
// Written into the row it would be a forecast frozen at the moment it aged
// worst — computed on read, it is always current.
type Record struct {
	ID    string    `json:"id"`
	Ts    time.Time `json:"ts"`
	Event Event     `json:"event"`

	Action Action `json:"action"`
	Origin Origin `json:"origin"`

	// Actor is the verified PAT id, when the action arrived over the PAT plane.
	// Empty for the internal plane (a CLI on the box itself has no PAT) — that
	// is a fact of the design, not a gap: "who" is answerable for what crosses
	// the PAT plane.
	Actor string `json:"actor,omitempty"`

	Scope string `json:"scope,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Name  string `json:"name,omitempty"`

	// ReleaseID joins this row to a ReleaseRecord when the action produced one.
	// The LEFT JOIN between the two histories.
	ReleaseID string `json:"release_id,omitempty"`

	Resources []Resource `json:"resources,omitempty"`
	Pods      []string   `json:"pods,omitempty"`

	// Files are the operator's original `-f` arguments.
	//
	// Declared by the CLIENT, not observed here, and it cannot be otherwise:
	// a forwarded apply reads the files on the laptop and streams the parsed
	// manifests, so by the time the request arrives the argv says `-f -`.
	// A local apply on the box has no forwarding step and so carries none.
	Files     []string `json:"files,omitempty"`
	CommitSHA string   `json:"commit_sha,omitempty"`

	// Client is who ran the command, resolved on their own machine.
	//
	// Also declared rather than observed, and the distinction matters enough
	// to state: the controller's own view of the peer is 127.0.0.1 for every
	// CLI action, because the CLI always talks to the loopback port — remote
	// work arrives over SSH and executes on the box. So the only place the
	// operator's address exists is the operator's machine.
	//
	// Declared means spoofable. It answers "who ran this" for a cooperating
	// operator, which is what an ops timeline is for; it is not evidence.
	Client *ClientInfo `json:"client,omitempty"`

	Prune bool `json:"prune,omitempty"`

	// ConfigKeys are the keys ONE config command touched.
	//
	// A list and not a single key, because `vd config set A=1 B=2 C=3` is one
	// thing the operator did. Writing a line per key produced three rows a
	// reader had to recognise as one command by squinting at a shared id, and
	// the screen showed three changes where one happened.
	ConfigKeys []ConfigChange `json:"config_keys,omitempty"`

	// finished / done only
	Status    Status `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
}

// ClientInfo is the operator's own attribution of themselves, resolved by the
// CLI on the machine the command was typed on.
//
// City-level and no finer. The lookup returns coordinates and a postal code as
// well; neither is carried, because this lands in a 30-day file served over the
// PAT plane and mirrored into a WebUI database, and "which city and which ISP"
// answers who ran the apply while a latitude answers something else entirely.
type ClientInfo struct {
	IP      string `json:"ip,omitempty"`
	City    string `json:"city,omitempty"`
	Region  string `json:"region,omitempty"`
	Country string `json:"country,omitempty"`
	Org     string `json:"org,omitempty"`
}

// ConfigChange is one key a config command touched.
//
// The key is recorded; the VALUE never is. The digest stands in for it.
//
// This file has 30-day retention, is served over the PAT plane and is mirrored
// into the WebUI's SQLite warehouse. Recording values would create a second
// plaintext copy of every production secret, with its own retention and its
// own read surface — routing around every protection the config bucket has
// (masked by default, ?values=false, never logged) via a file we created
// ourselves.
//
// The digest answers the only question the value would that the key does not:
// did it actually change, or did somebody re-set the same thing. Empty on a
// delete, where there is no value to stand in for.
type ConfigChange struct {
	Key         string `json:"key"`
	ValueDigest string `json:"value_digest,omitempty"`
}

// DigestValue returns the short digest stored in a ConfigChange. Never store
// or log the input.
func DigestValue(v string) string {
	sum := sha256.Sum256([]byte(v))

	return hex.EncodeToString(sum[:6])
}

// Logger is the minimal subset of *log.Logger used here. nil silences.
type Logger interface {
	Printf(format string, args ...any)
}

// Writer appends records to per-UTC-day NDJSON files.
//
// The atomicity contract is the one internal/metrics/writer.go documents, and
// it is worth restating because it is easy to break while "improving" this
// code: each write builds the entire line as []byte and issues exactly ONE
// (*os.File).Write against an O_APPEND fd, so a concurrent reader sees the
// whole line or nothing. A bufio.Writer may split a line across two write()
// syscalls at the buffer boundary; json.Encoder.Encode issues two writes (the
// object, then the newline). Neither is safe here.
//
// Unlike the metrics sampler, this has MANY concurrent producers — every action
// in flight — so the mutex is doing real work rather than guarding a single
// goroutine.
type Writer struct {
	dir string

	mu       sync.Mutex
	openDate string
	openFile *os.File
	logger   Logger
}

// NewWriter returns a Writer rooted at dir, creating it if missing. The day
// file opens lazily, on the first record.
func NewWriter(dir string, logger Logger) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("activity dir: %w", err)
	}

	return &Writer{dir: dir, logger: logger}, nil
}

// Write appends one record.
//
// Errors come back, but every caller is expected to log and carry on: recording
// history must never be the reason an action fails. A box that cannot write its
// own trail still has to be able to operate.
func (w *Writer) Write(rec Record) error {
	if rec.Ts.IsZero() {
		rec.Ts = time.Now()
	}

	rec.Ts = rec.Ts.UTC()

	if rec.Origin == "" {
		rec.Origin = OriginAPI
	}

	line, err := marshalLine(rec)
	if err != nil {
		return fmt.Errorf("marshal activity: %w", err)
	}

	return w.appendLine(rec.Ts, line)
}

// Close flushes and closes the open day file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.closeOpenLocked()
}

// marshalLine builds the complete line, newline included, so appendLine can
// issue a single Write. See the Writer doc for why that matters.
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

func (w *Writer) appendLine(ts time.Time, line []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.ensureOpenForLocked(ts); err != nil {
		return err
	}

	if _, err := w.openFile.Write(line); err != nil {
		return fmt.Errorf("activity write: %w", err)
	}

	// fsync per line, unlike metrics — a deliberate divergence.
	//
	// Metrics are lossy by nature: losing the last 30s of samples changes a
	// chart's tail. Losing the record of an action that did happen makes the
	// history lie, and the write rate here is a handful per day rather than
	// thousands per hour, so there is no cost worth saving.
	if err := w.openFile.Sync(); err != nil && w.logger != nil {
		w.logger.Printf("activity: sync: %v", err)
	}

	return nil
}

// ensureOpenForLocked opens the file for ts's UTC day, rotating when the open
// handle belongs to another day.
//
// Selection is by the record's OWN timestamp, never by "whatever is open", so
// an action that finishes after midnight lands in the right file and cannot
// race the cleanup pass gzipping yesterday's.
func (w *Writer) ensureOpenForLocked(ts time.Time) error {
	date := ts.UTC().Format(dateLayout)

	if w.openFile != nil && w.openDate == date {
		return nil
	}

	if err := w.closeOpenLocked(); err != nil && w.logger != nil {
		w.logger.Printf("activity: closing %s: %v", w.openDate, err)
	}

	path := filepath.Join(w.dir, FileName(date))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("activity open %s: %w", path, err)
	}

	w.openFile = f
	w.openDate = date

	return nil
}

func (w *Writer) closeOpenLocked() error {
	if w.openFile == nil {
		return nil
	}

	f := w.openFile
	w.openFile = nil
	w.openDate = ""

	if err := f.Sync(); err != nil {
		f.Close()

		return err
	}

	return f.Close()
}

const (
	// FilePrefix is shared by writer, reader and cleanup so files an operator
	// dropped in under another prefix are never touched.
	FilePrefix = "activity-"

	dateLayout = "2006-01-02"
)

// FileName is the NDJSON file for a YYYY-MM-DD date string.
func FileName(date string) string { return FilePrefix + date + ".ndjson" }
