package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.voodu.clowk.in/internal/progress"
)

// eventRenderer is the client-side NDJSON consumer. It is a sibling of
// progressFilter (cmd/cli/stream_filter.go) — same visual vocabulary
// (spinner, green ✓, red ✗, `Built X in Ns` summary), different input
// contract. progressFilter classifies raw text with a regex-ish state
// machine; eventRenderer decodes typed frames and dispatches. When the
// server sends NDJSON, this is what drives the user's terminal.
//
// Wire-format invariants it relies on (see internal/progress.Event):
//
//   - One JSON object per line, newline-terminated.
//   - First line is always `{"type":"hello","protocol":"..."}` — the
//     renderer refuses to progress until it matches the version it
//     was compiled against. A mismatch falls through to passthrough
//     (unknown future protocol).
//   - step_start and step_end pair by .id (opaque string).
//
// Escape hatches (mirror progressFilter):
//   - verbose=true → every frame is dumped as its raw JSON line so
//     the user can grep / jq. No spinner, no styling.
//   - stdout not a TTY → same passthrough, so piping to a file yields
//     a clean JSON stream CI can parse.
type eventRenderer struct {
	out     io.Writer
	verbose bool
	tty     bool

	mu       sync.Mutex
	leftover []byte

	// Handshake state. The client injected VOODU_PROTOCOL=ndjson/1
	// into the SSH env; the server's first line MUST be the matching
	// hello. Until then, frames are buffered as "potentially legacy
	// text" — but in practice the forwarder only instantiates this
	// renderer after peeking at the first line, so this is a
	// defense-in-depth check rather than a common code path.
	handshakeOK bool

	// Spinner / step state (mirrors progressFilter fields).
	active           bool
	currentStepID    string
	currentStepLabel string
	stepStarted      time.Time
	started          time.Time
	tag              string
	buildClosed      bool
	frame            int

	stopCh chan struct{}
	doneCh chan struct{}
}

// newEventRenderer wires a renderer around out. `verbose` flips the
// render-raw escape hatch (same semantics as --verbose on apply).
func newEventRenderer(out io.Writer, verbose bool) *eventRenderer {
	return &eventRenderer{
		out:     out,
		verbose: verbose,
		tty:     writerIsTerminal(out),
	}
}

// Write implements io.Writer — the SSH forwarder pipes the server's
// stdout here. Bytes are framed on \n (same buffering discipline as
// progressFilter.Write) and each complete line is routed through
// processLine.
func (r *eventRenderer) Write(p []byte) (int, error) {
	if r.verbose || !r.tty {
		return r.out.Write(p)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	data := append(r.leftover, p...)

	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}

		line := data[:idx]
		data = data[idx+1:]

		r.processLineLocked(line)
	}

	r.leftover = append(r.leftover[:0], data...)

	return len(p), nil
}

// processLineLocked parses one frame and dispatches on Type. Lines
// that fail to parse as JSON are emitted verbatim — the server may
// have slipped a stderr line into the frame stream (unlikely with
// Channel A, but a panic trace that escaped os.Stderr during a crash
// would land here), and showing it to the user is more helpful than
// silently swallowing. Caller must hold r.mu.
func (r *eventRenderer) processLineLocked(line []byte) {
	trimmed := bytes.TrimSpace(line)

	// Blank line — used by the apply→release transition (and other
	// downstream callers) as an intentional visual separator. Pass
	// it through clean so the section break survives the SSH-
	// forwarded path. Skip the spinner clear because there's
	// nothing to overdraw, just emit a newline.
	if len(trimmed) == 0 {
		fmt.Fprintln(r.out)
		return
	}

	var e progress.Event

	if err := json.Unmarshal(trimmed, &e); err != nil {
		// Non-JSON frame. Clear the spinner row (if any) so the raw
		// line lands clean, then emit it. Next spinner tick will
		// redraw below.
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintln(r.out, string(line))

		return
	}

	switch e.Type {
	case progress.EventHello:
		// The renderer was built to speak a specific wire version;
		// a mismatch means we're talking to a newer/older server
		// that may use fields we don't understand. Rather than
		// silently proceed and drop events, surface the mismatch —
		// the forwarder can then fall back to text rendering.
		if e.Protocol != progress.ProtocolVersion {
			fmt.Fprintf(r.out, "voodu: protocol mismatch (server=%q, client=%q), continuing with best-effort rendering\n",
				e.Protocol, progress.ProtocolVersion)
		}

		r.handshakeOK = true

	case progress.EventStepStart:
		r.handleStepStartLocked(e)

	case progress.EventStepEnd:
		r.handleStepEndLocked(e)

	case progress.EventLog:
		r.handleLogLocked(e)

	case progress.EventResult:
		r.handleResultLocked(e)

	case progress.EventSummary:
		r.handleSummaryLocked(e)

	default:
		// Unknown event type. Forward-compatible: silently ignore so
		// a server that shipped a new frame type doesn't break an
		// older client.
	}
}

func (r *eventRenderer) handleStepStartLocked(e progress.Event) {
	// Any already-open step gets closed as ✓ — the server's ordering
	// convention is "start implies the prior is done" for transitions
	// where it didn't emit an explicit step_end. The deploy pipeline
	// does emit end events, but Summary events also act as implicit
	// closers, so this guard keeps the spinner accurate regardless.
	r.closeCurrentStepLocked()

	if !r.active {
		r.active = true
		r.started = time.Now()
		r.tag = currentDeployTag()

		r.startSpinnerLocked()
	}

	r.currentStepID = e.ID
	r.currentStepLabel = e.Label
	r.stepStarted = time.Now()

	// Synchronous first frame so sub-second steps still flash at
	// least one spinner glyph — same reason openStepLocked in
	// progressFilter renders inline before the ticker fires.
	r.renderSpinnerLocked()
}

func (r *eventRenderer) handleStepEndLocked(e progress.Event) {
	// Only commit if the id matches the currently-open step. A
	// duplicate/late step_end for a step that's already been closed
	// by a follow-up step_start is a no-op. (Happens if a server
	// emits both explicit StepEnd and an implicit close-via-next-start,
	// which our own emitters avoid but downstream plugins might not.)
	if r.currentStepID != e.ID || r.currentStepLabel == "" {
		return
	}

	elapsed := time.Since(r.stepStarted).Round(time.Second)

	switch e.Status {
	case progress.StatusOK:
		fmt.Fprintf(r.out, "\r\x1b[2K\x1b[32m✓\x1b[0m %s \x1b[2m(%s)\x1b[0m\n", r.currentStepLabel, elapsed)

	case progress.StatusFail:
		// Red ✗, bold label, dim elapsed, the error on a second line.
		// Mirrors how the spinner reads: first-line status, second-line
		// detail. Keeps the eye anchored on the ✗/✓ column.
		fmt.Fprintf(r.out, "\r\x1b[2K\x1b[31m✗\x1b[0m %s \x1b[2m(%s)\x1b[0m\n", r.currentStepLabel, elapsed)

		if e.Error != "" {
			fmt.Fprintf(r.out, "  \x1b[31m%s\x1b[0m\n", e.Error)
		}

	case progress.StatusCancel:
		// Neutral clear — no claim about success or failure. The next
		// line the caller writes (e.g. "Apply canceled.") carries the
		// user-facing message.
		fmt.Fprint(r.out, "\r\x1b[2K")

	default:
		// Unknown status → treat as OK so we don't drop the commit.
		fmt.Fprintf(r.out, "\r\x1b[2K\x1b[32m✓\x1b[0m %s \x1b[2m(%s)\x1b[0m\n", r.currentStepLabel, elapsed)
	}

	r.currentStepID = ""
	r.currentStepLabel = ""
}

func (r *eventRenderer) handleLogLocked(e progress.Event) {
	// warn / error always surface — they're the whole reason log has
	// levels. info inside an active step gets swallowed (the spinner
	// is the story) but we advance the frame so chatter-heavy phases
	// still animate even when the ticker is lock-starved.
	switch e.Level {
	case progress.LevelWarn:
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintf(r.out, "\x1b[33m⚠\x1b[0m %s\n", e.Text)

	case progress.LevelError:
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintf(r.out, "\x1b[31m✗\x1b[0m %s\n", e.Text)

	default:
		if r.active {
			r.advanceAndRenderLocked()

			return
		}

		// Idle + info → print verbatim. Covers plugin banners that
		// arrive between steps and anything the server wanted the
		// user to see plain.
		fmt.Fprintln(r.out, e.Text)
	}
}

func (r *eventRenderer) handleResultLocked(e progress.Event) {
	// Clear spinner before printing so the ✓ line isn't trampled by
	// the next frame tick.
	if r.active {
		fmt.Fprint(r.out, "\r\x1b[2K")
	}

	ref := e.Kind + "/" + e.Name
	if e.Scope != "" {
		ref = e.Kind + "/" + e.Scope + "/" + e.Name
	}

	fmt.Fprintf(r.out, "\x1b[32m✓\x1b[0m %s %s\n", ref, e.Action)
}

func (r *eventRenderer) handleSummaryLocked(e progress.Event) {
	// Close any open step first — a summary always terminates the
	// most recent step (same semantic as the legacy end markers in
	// stream_filter.go).
	r.closeCurrentStepLocked()

	switch {
	case strings.HasPrefix(e.Text, "Build completed"):
		// Synthesize the overall `✓ Built <tag> in Ns` line — the
		// same banner progressFilter produces from the legacy
		// `-----> Build completed` end marker. Overall time is
		// measured from the first step_start of this run, so the
		// user sees "we built web in 3s" covering ship + receive +
		// create + build as a single operation.
		if r.active {
			r.stopSpinnerLocked()
			r.active = false
		}

		total := time.Since(r.started).Round(time.Second)

		fmt.Fprintf(r.out, "\x1b[32m✓\x1b[0m Built %s in %s\n", r.tag, total)

		r.buildClosed = true

	case strings.HasPrefix(e.Text, "Deploy completed successfully"):
		// Redundant with the Build summary above — drop once Build
		// already fired. In the no-build deploy path (pure image
		// pull, no build step), Build never fires, and this line
		// becomes the primary success signal — emit it plain in
		// that case.
		if r.buildClosed {
			return
		}

		if r.active {
			r.stopSpinnerLocked()
			r.active = false
		}

		fmt.Fprintf(r.out, "\x1b[32m✓\x1b[0m %s\n", e.Text)

	case strings.HasPrefix(e.Text, "Pruned "):
		// Passthrough-style ✓ line — matches how the legacy filter
		// handled `-----> Pruned N old release(s)`. Pruned arrives
		// after the deploy is done; no active step to disturb.
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintf(r.out, "\x1b[32m✓\x1b[0m %s\n", e.Text)

	default:
		// Unknown summary — print as a plain ✓ line. Future-proof
		// for new summary kinds we haven't wired a specific handler
		// for.
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintf(r.out, "\x1b[32m✓\x1b[0m %s\n", e.Text)
	}
}

// renderSpinnerLocked / advanceAndRenderLocked / closeCurrentStepLocked
// mirror the progressFilter helpers byte-for-byte so both renderers
// produce identical terminal bytes for the same step. Duplication
// rather than extraction because the two renderers have different
// state shapes and deduping would force an awkward shared base struct.
// Caller must hold r.mu.

func (r *eventRenderer) renderSpinnerLocked() {
	if !r.active || r.currentStepLabel == "" {
		return
	}

	frames := []rune(spinnerFrames)
	elapsed := time.Since(r.stepStarted).Round(time.Second)

	fmt.Fprintf(r.out, "\r\x1b[2K\x1b[36m%c\x1b[0m %s \x1b[2m(%s)\x1b[0m\n\x1b[2K\x1b[1A",
		frames[r.frame], r.currentStepLabel, elapsed)
}

func (r *eventRenderer) advanceAndRenderLocked() {
	if !r.active || r.currentStepLabel == "" {
		return
	}

	frames := []rune(spinnerFrames)
	r.frame = (r.frame + 1) % len(frames)

	r.renderSpinnerLocked()
}

func (r *eventRenderer) closeCurrentStepLocked() {
	if r.currentStepLabel == "" {
		return
	}

	elapsed := time.Since(r.stepStarted).Round(time.Second)

	fmt.Fprintf(r.out, "\r\x1b[2K\x1b[32m✓\x1b[0m %s \x1b[2m(%s)\x1b[0m\n", r.currentStepLabel, elapsed)

	r.currentStepID = ""
	r.currentStepLabel = ""
}

// startSpinnerLocked / spinLoop / tick / stopSpinnerLocked mirror the
// progressFilter goroutine scaffolding. Same 100ms tick cadence, same
// stop/done channel dance for clean shutdown without racing the last
// render.

func (r *eventRenderer) startSpinnerLocked() {
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})

	go r.spinLoop(r.stopCh, r.doneCh)
}

func (r *eventRenderer) spinLoop(stop, done chan struct{}) {
	defer close(done)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			r.tick()
		}
	}
}

func (r *eventRenderer) tick() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.advanceAndRenderLocked()
}

func (r *eventRenderer) stopSpinnerLocked() {
	if r.stopCh == nil {
		return
	}

	close(r.stopCh)
	stopCh := r.stopCh
	doneCh := r.doneCh

	r.mu.Unlock()
	<-doneCh
	r.mu.Lock()

	_ = stopCh

	r.stopCh = nil
	r.doneCh = nil
}

// Close flushes the leftover partial line and shuts down the spinner.
// Like progressFilter.Close, a dangling active step on SSH drop gets
// cleared without a false ✓ — printing success for a step that never
// finished would lie to the user.
func (r *eventRenderer) Close() error {
	if r.verbose || !r.tty {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.active {
		r.stopSpinnerLocked()
		fmt.Fprint(r.out, "\r\x1b[2K")
		r.active = false
		r.currentStepID = ""
		r.currentStepLabel = ""
	}

	if len(r.leftover) > 0 {
		// A trailing partial line at EOF is either a truncated JSON
		// frame (SSH dropped mid-event) or stray text. Dump it as-is
		// — if it's valid JSON we'd have to double-buffer an extra
		// newline to run it through processLineLocked, and the edge
		// case isn't worth the complexity.
		_, _ = r.out.Write(r.leftover)
		r.leftover = nil
	}

	return nil
}
