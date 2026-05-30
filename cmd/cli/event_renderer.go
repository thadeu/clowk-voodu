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
//	<spinner> packing context           ← in-progress, redraws in place
//	✓ packing context (1.4 MB)          ← frozen above when complete
//	<spinner> streaming over ssh
//	✓ streaming over ssh ubuntu@prod-1  (1s)
//	<spinner> controller: planning ...
//	✓ controller: planning (0s)
//	+ deployment/prod/api  replicas=3 image=ghcr.io/myorg/api:1.7
//	~ ingress/prod/api     tls.email=ops@example.com
//	+ redis/clowk-lp/redis-ha sentinel.monitor=clowk-lp/redis
//	<spinner> build → swap current → reconcile caddy
//	✓ apply complete in 11.8s            ← mint-colored terminus
//	✓ https://api.example.com · 3/3 healthy
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

	// Live build-log tail. While a step is active, raw sub-output lines
	// (the docker build's BuildKit stream, lang banners) that aren't
	// typed events get buffered here and rendered as a transient block of
	// up to buildTailN gray rows beneath the spinner — so the operator
	// sees which build phase is running. The block collapses on step end
	// (success) or is kept (failure). blockRows tracks how many rows the
	// live block currently occupies so the clear math wipes all of them.
	tail      []string
	blockRows int

	stopCh chan struct{}
	doneCh chan struct{}

	// resourceCount tracks per-manifest result events emitted by
	// handleResultLocked. Read after Close() by the apply orchestrator
	// to render the mint `✓ apply complete (N resources)` terminus.
	// Sibling of applyResultFilter.resourceCount — the negotiatingWriter
	// picks one renderer per run, so the orchestrator sums both counters
	// safely (one will always be zero).
	resourceCount int
}

// buildTailN is how many trailing build sub-output lines the live block
// shows beneath the spinner — enough to read the current build phase as
// it scrolls. The block collapses to a single ✓ line on success, so the
// height only matters while the build is live.
const buildTailN = 10

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

	// Blank line. During an active step it MUST be swallowed: a bare
	// newline here would push the cursor below the live block without the
	// block knowing, so the next redraw paints the spinner one row lower —
	// the "stacked spinner" ghosting (build output is full of blank
	// separator lines). Outside a step it's a real visual separator.
	if len(trimmed) == 0 {
		if r.active && r.currentStepLabel != "" {
			return
		}

		fmt.Fprintln(r.out)

		return
	}

	var e progress.Event

	if err := json.Unmarshal(trimmed, &e); err != nil {
		// Non-JSON frame: during a build it's the docker/BuildKit
		// sub-output (and lang banners) interleaved on the stream — feed
		// it to the live tail block instead of printing inline, so it
		// shows the current phase and collapses on step end. Outside an
		// active step it's a stderr leak / legacy line → print clean.
		if r.active && r.currentStepLabel != "" {
			r.pushTailLocked(string(line))

			return
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

	// Each step owns its own tail block — a fresh step starts empty.
	r.tail = nil

	// Synchronous first frame so sub-second steps still flash at least
	// one spinner glyph before the ticker fires.
	r.renderBlockLocked()
}

func (r *eventRenderer) handleStepEndLocked(e progress.Event) {
	if r.currentStepID != e.ID || r.currentStepLabel == "" {
		return
	}

	elapsed := time.Since(r.stepStarted).Round(time.Second)

	// Wipe the live block (spinner + tail) so the committed line lands
	// clean over the top row.
	r.clearBlockLocked()

	switch e.Status {
	case progress.StatusOK:
		// Success collapses the block — just the ✓ line remains, tier-
		// colored by label (gray checking phase / white central flow).
		fmt.Fprintf(r.out, "%s %s %s\n",
			stepGlyph(r.currentStepLabel), stepLabel(r.currentStepLabel), descText(fmt.Sprintf("(%s)", elapsed)))

	case progress.StatusFail:
		// Failure keeps the build tail: the last few lines are usually
		// where it broke, so they stay on screen as committed gray rows
		// beneath the ✗, above the reconciler's error.
		fmt.Fprintf(r.out, "%s %s %s\n",
			cross(), paintLabel(r.currentStepLabel), descText(fmt.Sprintf("(%s)", elapsed)))

		width := termWidth()

		for _, t := range r.tail {
			fmt.Fprintf(r.out, "  %s\n", descText(truncateVisible(t, width-2)))
		}

		if e.Error != "" {
			fmt.Fprintf(r.out, "  %s\n", colorize(cRose, e.Error))
		}

	case progress.StatusCancel:
		// Neutral clear — no claim of success or failure. Caller emits
		// the user-facing "Apply canceled." line next.

	default:
		fmt.Fprintf(r.out, "%s %s %s\n",
			stepGlyph(r.currentStepLabel), stepLabel(r.currentStepLabel), descText(fmt.Sprintf("(%s)", elapsed)))
	}

	r.tail = nil
	r.currentStepID = ""
	r.currentStepLabel = ""
}

func (r *eventRenderer) handleLogLocked(e progress.Event) {
	switch e.Level {
	case progress.LevelWarn:
		if r.active {
			r.clearBlockLocked()
		}

		fmt.Fprintf(r.out, "%s %s\n", warn(), e.Text)

	case progress.LevelError:
		if r.active {
			r.clearBlockLocked()
		}

		fmt.Fprintf(r.out, "%s %s\n", cross(), e.Text)

	default:
		// info inside an active step gets swallowed (spinner is the
		// story) but we still advance the frame so chatter-heavy phases
		// animate even when the ticker is lock-starved. The live build
		// tail is fed by the RAW build sub-output (non-JSON frames in
		// processLineLocked), not by these structured info logs.
		if r.active {
			r.advanceAndRenderLocked()
			return
		}

		fmt.Fprintln(r.out, e.Text)
	}
}

func (r *eventRenderer) handleResultLocked(e progress.Event) {
	if r.active {
		r.clearBlockLocked()
	}

	ref := e.Kind + "/" + e.Name
	if e.Scope != "" {
		ref = e.Kind + "/" + e.Scope + "/" + e.Name
	}

	// Resource-applied lines wear amber (✓ + ref) to set them apart from
	// the mint step-completions — the eye reads "these are the things that
	// changed" distinctly from "this build/stream phase finished".
	fmt.Fprintf(r.out, "%s %s %s\n", checkApplied(), colorize(cAmber, ref), descText(e.Action))

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
// out an apply. The success terminus uses the mint variant of ✓
// (checkFinal) — the brand's vivid "done" green; the Pruned/passthrough
// summaries are central-flow white (checkFlow).
func (r *eventRenderer) handleSummaryLocked(e progress.Event) {
	r.closeCurrentStepLocked()

	switch {
	case strings.HasPrefix(e.Text, "Build completed"):
		// The "✓ building release (Ns)" step line (emitted by the build
		// step_end) already marks the build done — so we DON'T print a
		// second "✓ built <tag> in Ns" terminus here; it was redundant.
		// We still stop the spinner and flag buildClosed so the later
		// "Deploy completed" summary is de-duplicated.
		if r.active {
			r.stopSpinnerLocked()
			r.active = false
		}

		r.buildClosed = true

	case strings.HasPrefix(e.Text, "Deploy completed successfully"):
		// In the build-then-deploy path, Build completed already fired
		// the terminus banner. Drop the redundant message.
		if r.buildClosed {
			return
		}

		// Pure-image-pull deploy path: this IS the terminus. Mint ✓ +
		// mint text — the brand's "done" green.
		if r.active {
			r.stopSpinnerLocked()
			r.active = false
		}

		fmt.Fprintf(r.out, "%s %s\n", checkFinal(), mintText(e.Text))

	case strings.HasPrefix(e.Text, "Pruned "):
		// Pruned N old release(s) — passthrough ✓ line, no spinner
		// active.
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintf(r.out, "%s %s\n", checkFlow(), e.Text)

	default:
		if r.active {
			fmt.Fprint(r.out, "\r\x1b[2K")
		}

		fmt.Fprintf(r.out, "%s %s\n", checkFlow(), e.Text)
	}
}

// renderBlockLocked paints the live block in place: row 0 is the spinner
// (braille glyph + label + elapsed tail); below it, up to buildTailN gray
// rows mirror the most recent build sub-output. The cursor enters and
// leaves at the block's top row, column 0, so each tick overwrites the
// whole block cleanly. Tail rows are truncated to the terminal width — a
// wrapped line would add screen rows the cursor-up count doesn't know
// about, desyncing every subsequent redraw.
//
// Caller must hold r.mu.
func (r *eventRenderer) renderBlockLocked() {
	if !r.active || r.currentStepLabel == "" {
		return
	}

	elapsed := time.Since(r.stepStarted).Round(time.Second)
	frames := []rune(brailleFrames)

	var b strings.Builder

	// Row 0 — spinner glyph + label + elapsed. stepLabel previews the
	// committed tier color (gray for a checking step, default fg for the
	// central flow); the glyph stays mint as the "working" signal.
	b.WriteString("\r\x1b[2K")
	b.WriteString(colorize(cMint400, string(frames[r.frame%len(frames)])))
	b.WriteString(" ")
	b.WriteString(stepLabel(r.currentStepLabel))
	b.WriteString(" ")
	b.WriteString(descText(fmt.Sprintf("(%s)", elapsed)))

	// Tail rows — gray, indented, truncated to the terminal width.
	width := termWidth()

	for _, t := range r.tail {
		b.WriteString("\n\x1b[2K  ")
		b.WriteString(descText(truncateVisible(t, width-2)))
	}

	// Park the cursor back at the top row, column 0.
	if n := len(r.tail); n > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", n)
	}

	b.WriteString("\r")

	fmt.Fprint(r.out, b.String())

	r.blockRows = 1 + len(r.tail)
}

// clearBlockLocked wipes the live block (spinner row + tail rows) and
// returns the cursor to the top row, column 0, so the caller can print a
// committed line over it. Inverse of renderBlockLocked's cursor walk.
func (r *eventRenderer) clearBlockLocked() {
	fmt.Fprint(r.out, "\r\x1b[2K")

	for i := 1; i < r.blockRows; i++ {
		fmt.Fprint(r.out, "\n\x1b[2K")
	}

	if r.blockRows > 1 {
		fmt.Fprintf(r.out, "\x1b[%dA", r.blockRows-1)
	}

	fmt.Fprint(r.out, "\r")

	r.blockRows = 1
}

// pushTailLocked records a raw build sub-output line in the tail ring
// (capped at buildTailN) and redraws the block. Blank/uninformative lines
// don't earn a row but still advance the spinner so it never freezes
// during a quiet stretch of the build.
func (r *eventRenderer) pushTailLocked(raw string) {
	frames := []rune(brailleFrames)
	r.frame = (r.frame + 1) % len(frames)

	if line := sanitizeTailLine(raw); line != "" {
		r.tail = append(r.tail, line)

		if len(r.tail) > buildTailN {
			r.tail = r.tail[len(r.tail)-buildTailN:]
		}
	}

	r.renderBlockLocked()
}

func (r *eventRenderer) advanceAndRenderLocked() {
	if !r.active || r.currentStepLabel == "" {
		return
	}

	frames := []rune(brailleFrames)
	r.frame = (r.frame + 1) % len(frames)

	r.renderBlockLocked()
}

func (r *eventRenderer) closeCurrentStepLocked() {
	if r.currentStepLabel == "" {
		return
	}

	elapsed := time.Since(r.stepStarted).Round(time.Second)

	// Implicit close = the prior step succeeded, so collapse its block.
	r.clearBlockLocked()

	fmt.Fprintf(r.out, "%s %s %s\n",
		stepGlyph(r.currentStepLabel), stepLabel(r.currentStepLabel), descText(fmt.Sprintf("(%s)", elapsed)))

	r.tail = nil
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
		r.clearBlockLocked()
		r.active = false
		r.tail = nil
		r.currentStepID = ""
		r.currentStepLabel = ""
	}

	if len(r.leftover) > 0 {
		_, _ = r.out.Write(r.leftover)
		r.leftover = nil
	}

	return nil
}
