package progress

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// Reporter is the emitter side of the progress protocol. Server code
// (deploy pipeline, apply handler, future plugin adapters) calls these
// methods instead of `fmt.Fprintln(out, "-----> ...")`. Two concrete
// implementations exist:
//
//   - TextReporter preserves the legacy `-----> Banner` wire format so
//     older clients and humans reading raw logs see exactly what they
//     used to.
//
//   - JSONReporter emits one NDJSON Event per call. Enabled only when
//     the client advertised VOODU_PROTOCOL=ndjson/1 on the SSH env.
//
// The interface is intentionally narrow — six verbs. Anything fancier
// (progress bars, nested steps, download ETAs) composes from these.
// The method set matches the Event types one-to-one except that
// StepStart + StepEnd share a single id key for pairing, and Hello is
// hidden from text callers (StdOut-style protocols don't have a
// handshake).
type Reporter interface {
	// Hello announces the protocol version. No-op for text; first
	// line for JSON. Callers invoke it exactly once, as the first
	// thing the server writes, before any Step/Log/Result.
	Hello()

	// StepStart opens a live step. id is a short stable key the
	// renderer uses to pair with StepEnd; label is the human
	// headline. In text mode this is "-----> label\n". In JSON mode
	// this is a step_start event.
	StepStart(id, label string)

	// StepEnd closes the step opened with the same id. status ok/
	// fail/cancel drives client-side ✓/✗/cleared rendering. err
	// (optional) surfaces as Event.Error for fails. In text mode
	// StepEnd is a no-op — the legacy client infers close from the
	// next banner or an end marker, and emitting extra lines here
	// would break that.
	StepEnd(id string, status StepStatus, err error)

	// Log is a non-step message: buildx chatter, plugin sub-banners,
	// post-deploy hook output, generic notices. Level is info/warn/
	// error. In text mode it's `fmt.Fprintln(w, text)`; the level
	// doesn't change the output (legacy clients never distinguished).
	// In JSON mode it's a log event that the client renders per its
	// policy (info swallowed during active step, warn/error always
	// visible).
	Log(level, text string)

	// Result announces a per-manifest verdict from voodu apply. The
	// text equivalent is `"<kind>/<scope>/<name> <action>"` — the
	// shape cmd/cli/apply.go has always printed. JSON mode emits a
	// structured result event so the client doesn't need regex to
	// extract the parts.
	Result(kind, scope, name, action string)

	// Summary is a terminal line for a logical operation: "Built web
	// in 3s", "Deploy completed successfully for 'softphone-web'".
	// One per pipeline run. Text mode prefixes with `-----> ` to
	// match the legacy banner format.
	Summary(text string)

	// Close flushes any buffered state. Safe to call multiple times.
	// TextReporter's Close is a no-op; JSONReporter's flushes the
	// underlying writer if it's a *bufio.Writer.
	Close() error
}

// NewReporter picks TextReporter or JSONReporter based on useJSON.
// Centralizes the decision so callers (receive-pack, apply) write one
// line instead of duplicating the env-var sniff. Pass the writer the
// reporter should write to — typically os.Stdout, but callers running
// in-process (tests, local apply) can pass a bytes.Buffer.
func NewReporter(w io.Writer, useJSON bool) Reporter {
	if useJSON {
		return NewJSONReporter(w)
	}

	return NewTextReporter(w)
}

// NewReporterFromEnv is the convenience server-boot entry point: it
// inspects VOODU_PROTOCOL and returns the matching reporter around w.
// Wraps NewReporter so the "is the client speaking NDJSON?" check
// lives exactly here. Anything other than the exact current protocol
// version falls back to text — we never promise to interoperate with
// a future wire format we haven't shipped yet.
func NewReporterFromEnv(w io.Writer) Reporter {
	return NewReporter(w, os.Getenv(EnvProtocol) == ProtocolVersion)
}

// TextReporter emits the legacy `-----> Banner` / plain-line format.
// It is a thin shim so the existing client progressFilter (and any
// human piping to `tee build.log`) sees exactly what they saw before
// NDJSON existed. Zero behavior change on the wire when the reporter
// is TextReporter — the whole point.
//
// Safe for concurrent use via an internal mutex: the deploy pipeline
// doesn't emit concurrently today, but the apply handler may in a
// future fan-out, and an os.Stdout writer is shared with other
// goroutines (e.g. post-deploy hook subprocess streaming).
type TextReporter struct {
	mu  sync.Mutex
	out io.Writer
}

// NewTextReporter wires a TextReporter around w. out is typically
// os.Stdout inside the server process; tests pass a bytes.Buffer.
func NewTextReporter(w io.Writer) *TextReporter {
	return &TextReporter{out: w}
}

// Hello is a no-op for text — the legacy format never had a handshake.
func (*TextReporter) Hello() {}

// StepStart emits `-----> <label>`. Matches the legacy server banner
// format byte-for-byte so the old client-side progressFilter classifies
// it correctly.
func (t *TextReporter) StepStart(_, label string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	fmt.Fprintf(t.out, "-----> %s\n", label)
}

// StepEnd is deliberately a no-op. The legacy protocol infers step
// close from the next banner (or an end marker like `-----> Build
// completed` which the deploy pipeline emits as a Summary). Writing
// an explicit close line here would surface as an unknown banner to
// the client and clutter the output.
func (*TextReporter) StepEnd(_ string, _ StepStatus, _ error) {}

// Log writes text verbatim. Level is ignored in text mode — older
// clients never rendered warn/error differently, so adding a prefix
// here would be gratuitous drift.
func (t *TextReporter) Log(_, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	fmt.Fprintln(t.out, text)
}

// Result writes `"<kind>/<scope>/<name> <action>"` — the shape
// cmd/cli/apply.go has been printing since M1 of the apply rework.
// Kept here so server code has one way to say "this resource was
// applied" regardless of which reporter is active.
func (t *TextReporter) Result(kind, scope, name, action string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if scope == "" {
		fmt.Fprintf(t.out, "%s/%s %s\n", kind, name, action)

		return
	}

	fmt.Fprintf(t.out, "%s/%s/%s %s\n", kind, scope, name, action)
}

// Summary prints `-----> <text>` — same prefix the legacy pipeline
// used for "Build completed" and "Deploy completed successfully for
// 'app'". The client's stream_filter has these exact phrases in its
// endMarkers table, so the summary line doubles as the step-close
// trigger for the final step. Adding or renaming a summary without
// updating stream_filter would leave a dangling spinner — the kind of
// coupling NDJSON replaces, but we keep it stable for text mode.
func (t *TextReporter) Summary(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	fmt.Fprintf(t.out, "-----> %s\n", text)
}

// Close is a no-op for text. No buffering happens inside the reporter;
// any buffering (bufio.Writer wrapping os.Stdout) is the caller's
// responsibility.
func (*TextReporter) Close() error { return nil }

// JSONReporter emits one NDJSON Event per call. Each write is a
// single json.Encoder.Encode (adds a trailing newline) so readers can
// frame on `\n`. Behind a mutex because Event.Type on the wire must
// not interleave with another event's bytes — a half-written JSON
// breaks the client parser for the rest of the stream.
type JSONReporter struct {
	mu  sync.Mutex
	out io.Writer
	enc *json.Encoder
}

// NewJSONReporter wires a JSONReporter around w. The encoder writes
// compact JSON (no indent) — one event per line is the wire contract,
// and pretty-printing would break it.
func NewJSONReporter(w io.Writer) *JSONReporter {
	r := &JSONReporter{out: w}
	r.enc = json.NewEncoder(w)

	return r
}

// write serializes and emits a single event under the mutex. The
// encoder writes Event JSON + "\n" atomically, giving the line-framed
// wire format.
func (j *JSONReporter) write(e Event) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if e.Ts == "" {
		e.Ts = Now()
	}

	// Silently ignore encode errors: the writer is typically os.Stdout
	// wired to an SSH pipe, and if SSH is gone we'll surface the error
	// on the next real I/O anyway. Panicking inside a reporter would
	// kill the whole deploy mid-build for a cosmetic failure.
	_ = j.enc.Encode(e)
}

// Hello emits the handshake. Called exactly once, before any other
// event, so the client can confirm the wire version before parsing
// further frames.
func (j *JSONReporter) Hello() {
	j.write(Event{Type: EventHello, Protocol: ProtocolVersion})
}

// StepStart emits a step_start event with the given id and label.
func (j *JSONReporter) StepStart(id, label string) {
	j.write(Event{Type: EventStepStart, ID: id, Label: label})
}

// StepEnd emits a step_end event. err is flattened to its Error()
// string; nil stays empty so the JSON `omitempty` drops the field.
func (j *JSONReporter) StepEnd(id string, status StepStatus, err error) {
	e := Event{Type: EventStepEnd, ID: id, Status: status}

	if err != nil {
		e.Error = err.Error()
	}

	j.write(e)
}

// Log emits a log event. Empty level defaults to info — the protocol
// doesn't require a level, but having "info" on the wire is clearer
// for downstream `jq` inspection than an empty string.
func (j *JSONReporter) Log(level, text string) {
	if level == "" {
		level = LevelInfo
	}

	j.write(Event{Type: EventLog, Level: level, Text: text})
}

// Result emits a result event with the four fields the client renders
// as `✓ <kind>/<scope>/<name> <action>`. Empty scope is preserved as
// empty — the client omits that segment when it's empty.
func (j *JSONReporter) Result(kind, scope, name, action string) {
	j.write(Event{
		Type:   EventResult,
		Kind:   kind,
		Scope:  scope,
		Name:   name,
		Action: action,
	})
}

// Summary emits a summary event. One per logical operation.
func (j *JSONReporter) Summary(text string) {
	j.write(Event{Type: EventSummary, Text: text})
}

// Close is a no-op — json.Encoder doesn't buffer, and the underlying
// writer's lifecycle belongs to the caller. Kept on the interface so
// future reporters (buffered, batching, over-HTTP) can cleanly hook
// teardown without the interface shape changing.
func (*JSONReporter) Close() error { return nil }
