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

// eventRenderer is the client-side NDJSON consumer. Sibling of
// progressFilter (cmd/cli/stream_filter.go) — same input contract
// (server-streamed bytes), different parse (typed events vs free-form
// text), shared visual vocabulary (style.go palette + symbols, spinner.go
// loading indicators).
//
// Output anatomy (matches the landing page mockup):
//
//   <spinner> packing context           ← in-progress, redraws in place
//   ✓ packing context (1.4 MB)          ← frozen above when complete
//   <spinner> streaming over ssh
//   ✓ streaming over ssh ubuntu@prod-1  (1s)
//   <spinner> controller: planning ...
//   ✓ controller: planning (0s)
//   + deployment/prod/api  replicas=3 image=ghcr.io/myorg/api:1.7
//   ~ ingress/prod/api     tls.email=ops@example.com
//   + redis/clowk-lp/redis-ha sentinel.monitor=clowk-lp/redis
//   <spinner> build → swap current → reconcile caddy
//   ✓ apply complete in 11.8s            ← aurora-colored terminus
//   ✓ https://api.example.com · 3/3 healthy
//
// In-progress line uses a brand-kit braille frame in mint-400 (see
// spinner.go for the 8-frame, 80ms/frame pattern). We tried inline-
// image (iTerm2/kitty GIF) for a richer indicator but multi-row images
// stacked vertically on every tick because single-line `\r\x1b[2K`
// overprint can't clear a 2-row image — visually broken on every
// terminal that supported the protocol. Braille-only is the safer call.
//
// Wire-format invariants (see internal/progress.Event):
//   - One JSON object per line, newline-terminated.
//   - First line is `{"type":"hello","protocol":"..."}` — handshake.
//   - step_start / step_end pair by .id.
//
// Escape hatches:
//   - verbose=true  → raw NDJSON passthrough (grep / jq friendly).
//   - stdout not a TTY → passthrough (CI / pipe-to-file).
type eventRenderer struct {
	out     io.Writer
	verbose bool
	tty     bool

	mu       sync.Mutex
	leftover []byte

	// Handshake state — the renderer refuses to interpret frames until
	// it sees a hello matching its compiled-in ProtocolVersion. In
	// practice the forwarder peeks the first line before instantiating
	// us, so this is defense-in-depth.
	handshakeOK bool

	// Spinner / step state.
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

	// resourceCount tracks per-manifest result events emitted by
	// handleResultLocked. Read after Close() by the apply orchestrator
	// to render the aurora `✓ apply complete (N resources)` terminus.
	// Sibling of applyResultFilter.resourceCount — the negotiatingWriter
	// picks one renderer per run, so the orchestrator sums both counters
	// safely (one will always be zero).
	resourceCount int
}

func newEventRenderer(out io.Writer, verbose bool) *eventRenderer {
	return &eventRenderer{
		out:     out,
		verbose: verbose,
		tty:     writerIsTerminal(out),
	}
}

// Write implements io.Writer — the SSH forwarder pipes the server's
// stdout here. Bytes are framed on \n and each complete line routed
// through processLine.
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

func (r *eventRenderer) processLineLocked(line []byte) {
	trimmed := bytes.TrimSpace(line)

	// Blank line as visual separator — pass through clean. Skip the
	// spinner-clear because there's nothing to overdraw.
	if len(trimmed) == 0 {
		fmt.Fprintln(r.out)
		return
	}

	var e progress.Event

	if err := json.Unmarshal(trimmed, &e); err != nil {
		// Non-JSON frame — stderr leak (panic trace) or legacy text.
		// Clear the spinner row so the raw line lands clean.
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintln(r.out, string(line))

		return
	}

	switch e.Type {
	case progress.EventHello:
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
		// Unknown event type — forward-compatible silent ignore.
	}
}

func (r *eventRenderer) handleStepStartLocked(e progress.Event) {
	// Implicit close: a step_start without a preceding step_end means
	// the prior step succeeded (server's "transition implies completion"
	// convention).
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

	// Synchronous first frame so sub-second steps still flash at least
	// one spinner glyph before the ticker fires.
	r.renderSpinnerLocked()
}

func (r *eventRenderer) handleStepEndLocked(e progress.Event) {
	if r.currentStepID != e.ID || r.currentStepLabel == "" {
		return
	}

	elapsed := time.Since(r.stepStarted).Round(time.Second)

	switch e.Status {
	case progress.StatusOK:
		fmt.Fprintf(r.out, "\r\x1b[2K%s %s %s\n",
			check(), r.currentStepLabel, dim(fmt.Sprintf("(%s)", elapsed)))

	case progress.StatusFail:
		fmt.Fprintf(r.out, "\r\x1b[2K%s %s %s\n",
			cross(), r.currentStepLabel, dim(fmt.Sprintf("(%s)", elapsed)))

		if e.Error != "" {
			fmt.Fprintf(r.out, "  %s\n", colorize(cRose, e.Error))
		}

	case progress.StatusCancel:
		// Neutral clear — no claim of success or failure. Caller emits
		// the user-facing "Apply canceled." line next.
		fmt.Fprint(r.out, "\r\x1b[2K")

	default:
		fmt.Fprintf(r.out, "\r\x1b[2K%s %s %s\n",
			check(), r.currentStepLabel, dim(fmt.Sprintf("(%s)", elapsed)))
	}

	r.currentStepID = ""
	r.currentStepLabel = ""
}

func (r *eventRenderer) handleLogLocked(e progress.Event) {
	switch e.Level {
	case progress.LevelWarn:
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintf(r.out, "%s %s\n", warn(), e.Text)

	case progress.LevelError:
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintf(r.out, "%s %s\n", cross(), e.Text)

	default:
		// info inside an active step gets swallowed (spinner is the
		// story) but we still advance the frame so chatter-heavy phases
		// animate even when the ticker is lock-starved.
		if r.active {
			r.advanceAndRenderLocked()
			return
		}

		fmt.Fprintln(r.out, e.Text)
	}
}

func (r *eventRenderer) handleResultLocked(e progress.Event) {
	if r.active {
		fmt.Fprint(r.out, "\r\x1b[2K")
	}

	ref := e.Kind + "/" + e.Name
	if e.Scope != "" {
		ref = e.Kind + "/" + e.Scope + "/" + e.Name
	}

	fmt.Fprintf(r.out, "%s %s %s\n", check(), ref, e.Action)

	r.resourceCount++
}

// ResourceCount returns the number of per-manifest ✓ lines this renderer
// emitted from EventResult frames. Safe to call after Close. Sibling of
// applyResultFilter.ResourceCount — apply_forwarded.go reads from
// whichever filter the negotiating writer picked.
func (r *eventRenderer) ResourceCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.resourceCount
}

// handleSummaryLocked emits the terminus lines — the ones that close
// out an apply. These use the aurora variant of ✓ per brand kit:
// aurora is reserved for "active states" and the final "everything's
// good" terminal.
func (r *eventRenderer) handleSummaryLocked(e progress.Event) {
	r.closeCurrentStepLocked()

	switch {
	case strings.HasPrefix(e.Text, "Build completed"):
		// Built X in Ns — synthesizes the overall build banner from
		// the first step_start of this run. Mint ✓ (not aurora) — this
		// is the build phase ending, not the whole apply.
		if r.active {
			r.stopSpinnerLocked()
			r.active = false
		}

		total := time.Since(r.started).Round(time.Second)

		fmt.Fprintf(r.out, "%s Built %s in %s\n", check(), r.tag, total)

		r.buildClosed = true

	case strings.HasPrefix(e.Text, "Deploy completed successfully"):
		// In the build-then-deploy path, Build completed already fired
		// the terminus banner. Drop the redundant message.
		if r.buildClosed {
			return
		}

		// Pure-image-pull deploy path: this IS the terminus. Aurora ✓.
		if r.active {
			r.stopSpinnerLocked()
			r.active = false
		}

		fmt.Fprintf(r.out, "%s %s\n", checkFinal(), e.Text)

	case strings.HasPrefix(e.Text, "Pruned "):
		// Pruned N old release(s) — passthrough ✓ line, no spinner
		// active.
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintf(r.out, "%s %s\n", check(), e.Text)

	default:
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintf(r.out, "%s %s\n", check(), e.Text)
	}
}

// renderSpinnerLocked paints the active step line — spinner glyph (or
// inline image) + label + elapsed-time tail. Cleared/repainted on
// every frame tick; the cursor stays parked at the start of the line
// so the next frame overwrites in place.
//
// Caller must hold r.mu.
func (r *eventRenderer) renderSpinnerLocked() {
	if !r.active || r.currentStepLabel == "" {
		return
	}

	elapsed := time.Since(r.stepStarted).Round(time.Second)

	// Clear line, park cursor at column 0, paint braille frame +
	// label + dim elapsed-time tail.
	fmt.Fprint(r.out, "\r\x1b[2K")
	r.paintBrailleLocked()

	fmt.Fprintf(r.out, " %s %s",
		r.currentStepLabel,
		dim(fmt.Sprintf("(%s)", elapsed)),
	)

	// The trailing CR + line-up dance keeps the cursor positioned for
	// the next overwrite. Without it, log lines emitted while a
	// spinner is active would push the spinner down and leave its
	// last frame as a dangling ghost row.
	fmt.Fprint(r.out, "\n\x1b[2K\x1b[1A")
}

// paintBrailleLocked emits one mint-colored braille frame at the
// cursor's current position. Cursor must already be at the line
// start (caller clears with \r\x1b[2K beforehand).
func (r *eventRenderer) paintBrailleLocked() {
	frames := []rune(brailleFrames)
	frame := frames[r.frame%len(frames)]

	fmt.Fprint(r.out, colorize(cMint400, string(frame)))
}

func (r *eventRenderer) advanceAndRenderLocked() {
	if !r.active || r.currentStepLabel == "" {
		return
	}

	frames := []rune(brailleFrames)
	r.frame = (r.frame + 1) % len(frames)

	r.renderSpinnerLocked()
}

func (r *eventRenderer) closeCurrentStepLocked() {
	if r.currentStepLabel == "" {
		return
	}

	elapsed := time.Since(r.stepStarted).Round(time.Second)

	fmt.Fprintf(r.out, "\r\x1b[2K%s %s %s\n",
		check(), r.currentStepLabel, dim(fmt.Sprintf("(%s)", elapsed)))

	r.currentStepID = ""
	r.currentStepLabel = ""
}

// Spinner goroutine — drives the frame ticker. brailleTickMS is the
// brand kit's specified 80 ms/frame cadence; we use it for both the
// braille spinner AND the GIF path (the GIF doesn't need a tick to
// animate, but the elapsed-time tail does need refresh).

func (r *eventRenderer) startSpinnerLocked() {
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})

	go r.spinLoop(r.stopCh, r.doneCh)
}

func (r *eventRenderer) spinLoop(stop, done chan struct{}) {
	defer close(done)

	ticker := time.NewTicker(brailleTickMS * time.Millisecond)
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
	doneCh := r.doneCh

	r.mu.Unlock()
	<-doneCh
	r.mu.Lock()

	r.stopCh = nil
	r.doneCh = nil
}

// Close flushes leftover partial bytes and shuts down the spinner.
// A dangling active step on SSH drop gets cleared without a false ✓
// — printing success for a step that never finished would lie.
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
		_, _ = r.out.Write(r.leftover)
		r.leftover = nil
	}

	return nil
}
