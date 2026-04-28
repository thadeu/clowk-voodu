package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// phasePrefix identifies any server-emitted progress line. Every
// phase banner ships with "-----> " (five dashes + angle + space)
// — the receive-pack, controller release-phase, and shell plugins
// all share this convention. The filter treats every such line as
// a fresh phase: spinner tag updates, the line passes through,
// and the animation keeps running.
const phasePrefix = "-----> "

// endMarkers stop the spinner and print the final ✓ summary. The
// primary marker is emitted by internal/deploy/deploy.go after a
// successful build; the secondary covers no-build pipelines (pure
// image-pull deploys) where "Build completed" never fires. Either
// one landing first wins; duplicates are harmless because f.active
// is already false on the second.
//
// Both entries carry the leading `-----> ` because that's how a text-
// mode server actually emits them: progress.TextReporter.Summary
// prefixes every summary with `-----> `. Matching without the prefix
// would never fire for a server that went through the Reporter API
// (post-Camada-3), which is every modern server build. Trimming in
// the classifier doesn't help — `strings.HasPrefix` on a `----->`-
// prefixed line against a bare "Deploy completed …" needle is a miss.
var endMarkers = []string{
	"-----> Build completed",
	"-----> Deploy completed successfully",
}

// stepBanners are the curated `-----> ` phases that open a new
// "step" in the UI — a live spinner that transitions to a green ✓
// when the next step (or an end marker) arrives. Adding a phase here
// is a conscious DX decision. All other `-----> ` banners that aren't
// in passthroughBanners are treated as sub-details and swallowed; the
// spinner keeps showing the active step while the build churns.
//
// Since the NDJSON protocol shipped, most of these banners are only
// seen on the legacy fallback path (old server that hasn't been
// upgraded yet) — the modern NDJSON path routes typed step_start
// events through eventRenderer instead. The exception is "Checking " —
// that banner is emitted client-side in runApplyForwarded to drive
// the phase-1 diff spinner and has no server equivalent, so it
// belongs here regardless of negotiation outcome.
//
// "Shipping " used to be client-emitted as a `----->` banner too, but
// since the Camada 3 integration it's committed directly as a `✓`
// line to stdout (outside any filter) to avoid racing with the
// negotiator's first-line peek. Kept in the table anyway as a safety
// net — a future caller that writes `-----> Shipping …` through
// progressFilter should still see the spinner.
var stepBanners = []string{
	"Checking ",
	"Shipping ",
	"Receiving ",
	"Creating release",
	"Building release",
}

// passthroughBanners are `-----> ` phases we want in the scrollback
// but not as a wrapped step. Pruned is the canonical case: it arrives
// after the deploy is already done, describes a post-hoc cleanup, and
// has no meaningful duration to animate. We promote it to a ✓ line
// directly so the output stays stylistically consistent with the
// step-closing ✓ lines above it.
var passthroughBanners = []string{
	"Pruned ",
}

// spinnerFrames are the classic braille dot rotation. Unicode-safe in
// every modern terminal we care about. 10 frames × 100ms = full
// rotation per second, matching the cadence the user's eye expects.
const spinnerFrames = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"

// progressFilter wraps an io.Writer and renders each top-level
// `-----> ` phase as a live spinner that resolves to a green ✓ line
// when the next phase (or an end marker) arrives. Sub-banners and
// docker buildx chatter are swallowed; the spinner's advancing timer
// is the progress signal during a step. This is pure presentation —
// bytes on the SSH pipe are unchanged.
//
// Role after the NDJSON protocol (Camada 3) shipped:
//
//   - Phase 1 of a forwarded apply still uses progressFilter directly
//     for the client-side `-----> Checking remote state...` spinner.
//     That banner is entirely client-driven and never crosses the wire.
//
//   - For server-driven streams (build push via receive-pack, apply
//     phase 3) progressFilter is now the *legacy fallback* branch of
//     the negotiatingWriter. When the server speaks NDJSON,
//     eventRenderer takes over and progressFilter sees no bytes at
//     all. When the server is still on the text format (older deploy,
//     transition period), progressFilter handles it exactly as it did
//     before.
//
// The long-term sunset plan is: once every deployed server speaks
// NDJSON (tracked in the Voodu upgrade notes), progressFilter
// collapses into a buildx-noise-only classifier — no step state
// machine, no banner tables, just `#<digits> ... → swallow`. Until
// that milestone passes (and it's an opt-in, per-deploy decision),
// the full state machine stays.
//
// Visual model:
//
//	⠋ Building release... (2s)      ← live, spinner ticking
//	✓ Building release... (3s)      ← committed after next step arrives
//
// Lifecycle of one "step" (one live-then-closed headline):
//   - idle → first step banner opens a step; spinner starts
//   - step open → next step banner closes the current with ✓ and
//     opens the new one (spinner never stops ticking across the run)
//   - step open → `-----> Build completed` closes the current step
//     and prints the overall `✓ Built <tag> in <N>s` summary
//   - step open → `Deploy completed successfully` closes the current
//     step and passes the line through verbatim
//   - any time → `-----> Pruned ...` prints a ✓ line inline without
//     disturbing the current step (Pruned is a post-facto note)
//
// Escape hatches:
//   - verbose=true → passthrough, no filtering at all
//   - stdout not a TTY → passthrough (piping to file shouldn't get \r
//     escapes polluting the log)
type progressFilter struct {
	out     io.Writer
	verbose bool
	tty     bool

	mu          sync.Mutex
	leftover    []byte // partial line carried across Write boundaries
	active      bool
	currentStep string    // headline of the open step (shown on spinner and in the ✓ on close)
	stepStarted time.Time // wall-clock when the current step opened
	started     time.Time // wall-clock of the very first step, for the overall summary
	tag         string    // deploy name captured from rememberShippedTag
	buildClosed bool      // "-----> Build completed" already committed its ✓ Built summary
	frame       int

	// Spinner goroutine control. Non-nil only while active.
	stopCh chan struct{}
	doneCh chan struct{}
}

// newProgressFilter wires up the filter with sensible defaults. `out`
// is typically os.Stdout; `verbose` comes from --verbose on the apply
// command.
func newProgressFilter(out io.Writer, verbose bool) *progressFilter {
	return &progressFilter{
		out:     out,
		verbose: verbose,
		tty:     writerIsTerminal(out),
	}
}

// writerIsTerminal reports whether w is a TTY. Non-*os.File writers
// (buffers, tests) are never TTYs — which is what we want for unit
// tests that capture output into a strings.Builder.
func writerIsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}

	return term.IsTerminal(int(f.Fd()))
}

// Write implements io.Writer. It accumulates bytes until it sees a
// complete line, then dispatches line-by-line through the state
// machine. Partial lines at the end of a write are stashed in
// `leftover` so we never render a half-line.
func (f *progressFilter) Write(p []byte) (int, error) {
	if f.verbose || !f.tty {
		return f.out.Write(p)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	data := append(f.leftover, p...)

	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}

		line := string(data[:idx])
		data = data[idx+1:]

		f.processLineLocked(line)
	}

	// Stash whatever is left as a partial line for the next write.
	f.leftover = append(f.leftover[:0], data...)

	return len(p), nil
}

func (f *progressFilter) processLineLocked(line string) {
	trimmed := strings.TrimSpace(line)

	// End markers close the current step (with its ✓) and then emit
	// their own finalization. "-----> Build completed" is the primary
	// signal for build-mode deploys and gets replaced by the overall
	// `✓ Built <tag> in <N>s` summary. "Deploy completed successfully"
	// is the sibling signal for no-build deploys — once Build completed
	// has already fired it's redundant (the ✓ Built line carries the
	// same success semantic), so we swallow it to keep the output
	// uncluttered. Only in the (theoretical) case where the filter sees
	// Deploy completed without a preceding Build completed does the line
	// pass through, as a safety net.
	for _, em := range endMarkers {
		if !strings.HasPrefix(trimmed, em) {
			continue
		}

		if em == "-----> Build completed" {
			f.closeCurrentStepLocked()

			if f.active {
				f.stopSpinnerLocked()
				f.active = false
			}

			total := time.Since(f.started).Round(time.Second)

			fmt.Fprintf(f.out, "\x1b[32m✓\x1b[0m Built %s in %s\n", f.tag, total)

			f.buildClosed = true

			return
		}

		// "Deploy completed successfully …" branch.
		if f.buildClosed {
			// Redundant with ✓ Built above — drop.
			return
		}

		f.closeCurrentStepLocked()

		if f.active {
			f.stopSpinnerLocked()
			f.active = false
		}

		fmt.Fprintln(f.out, line)

		return
	}

	// `-----> ...` banner classification:
	//   - stepBanner   → closes any current step, opens a new one (spinner)
	//   - passthrough  → emits a standalone ✓ line, does not touch the step
	//   - anything else → sub-detail, swallowed when active so the spinner
	//                     line stays pinned on the current step's headline
	if strings.HasPrefix(trimmed, phasePrefix) {
		msg := strings.TrimPrefix(trimmed, phasePrefix)

		switch {
		case matchesAny(msg, stepBanners):
			f.closeCurrentStepLocked()
			f.openStepLocked(msg)

		case matchesAny(msg, passthroughBanners):
			if f.active {
				// Clear the spinner line so the passthrough ✓ lands on
				// its own row; next tick redraws the spinner below.
				fmt.Fprint(f.out, "\r\x1b[2K")
			}

			fmt.Fprintf(f.out, "\x1b[32m✓\x1b[0m %s\n", msg)

		default:
			// Unknown `-----> ` banner — neither in stepBanners nor
			// passthroughBanners. When a step is active we swallow
			// (plugin sub-details like "Detected Dockerfile at /…",
			// "Release root has N entries" — the spinner headline is
			// the story) but we also nudge the spinner one frame forward
			// so chatter-heavy phases still animate even if the ticker
			// goroutine is starved by lock contention. When idle we
			// print the line verbatim instead of auto-opening an implicit
			// step: spinning on an unknown phrase is worse DX than
			// showing it plain, and callers already outside the build
			// flow (e.g. "-----> No spec changes. Re-pushing source …")
			// should land in scrollback unchanged. New phases opt into
			// the spinner by joining one of the tables above.
			if f.active {
				f.advanceAndRenderLocked()
			} else {
				fmt.Fprintln(f.out, line)
			}
		}

		return
	}

	// Non-banner content. Inside a step we swallow (tarball noise,
	// plugin continuation lines with leading whitespace, buildx `#N`
	// chatter, blank separators) but treat each line as a spinner
	// heartbeat — the goroutine ticker fires every 100ms, but docker
	// buildx can hold the Write lock in a near-continuous burst during
	// a build; advancing the frame here guarantees the animation moves
	// regardless. Outside a step we passthrough because this is how
	// trailing "deployment/... applied" lines and idle banners reach
	// the user.
	if f.active {
		f.advanceAndRenderLocked()
		return
	}

	fmt.Fprintln(f.out, line)
}

// openStepLocked starts a new step with the given headline message.
// First call also flips active=true and spins up the ticker goroutine;
// subsequent calls reuse the goroutine, only the step state rotates.
// Caller must hold f.mu.
func (f *progressFilter) openStepLocked(msg string) {
	if !f.active {
		f.active = true
		f.started = time.Now()
		f.tag = currentDeployTag()

		f.startSpinnerLocked()
	}

	f.currentStep = msg
	f.stepStarted = time.Now()

	// Render the first frame synchronously. The ticker goroutine only
	// fires 100ms after start, which is longer than many sub-steps take
	// to complete — without this explicit render, Shipping/Receiving/
	// Creating would close so fast the user never actually sees a
	// spinner frame between "start" and "✓". Drawing here guarantees at
	// least one visible `⠋ <step>` frame per step, even if the next line
	// arrives the very next microsecond.
	f.renderSpinnerLocked()
}

// renderSpinnerLocked draws the current spinner frame without advancing
// it. Shared between the ticker goroutine (which advances the frame
// first) and openStepLocked (which just wants to show the current
// frame on a brand-new step). Caller must hold f.mu.
//
// After the spinner line we emit `\n\x1b[2K\x1b[1A` — move down one
// row, clear it, move back up. This reserves a blank row immediately
// below the live spinner, so when the build lands at the bottom edge
// of the user's terminal the animated line isn't hugging the frame
// (which reads as "stuck"). At most one row of scroll happens on the
// first render per step; subsequent ticks reuse the already-blank row
// without further scrolling.
func (f *progressFilter) renderSpinnerLocked() {
	if !f.active || f.currentStep == "" {
		return
	}

	frames := []rune(spinnerFrames)
	elapsed := time.Since(f.stepStarted).Round(time.Second)

	fmt.Fprintf(f.out, "\r\x1b[2K\x1b[36m%c\x1b[0m %s \x1b[2m(%s)\x1b[0m\n\x1b[2K\x1b[1A",
		frames[f.frame], f.currentStep, elapsed)
}

// closeCurrentStepLocked commits the currently-open step as a green
// `✓ <step> (Ns)` line and leaves the cursor on a fresh row so the
// spinner's next tick renders below. No-op when no step is open.
// Caller must hold f.mu.
func (f *progressFilter) closeCurrentStepLocked() {
	if f.currentStep == "" {
		return
	}

	elapsed := time.Since(f.stepStarted).Round(time.Second)

	fmt.Fprintf(f.out, "\r\x1b[2K\x1b[32m✓\x1b[0m %s \x1b[2m(%s)\x1b[0m\n", f.currentStep, elapsed)

	f.currentStep = ""
}

// matchesAny reports whether any entry in patterns is a prefix of s.
// Used by the banner classifier so the tables of step/passthrough
// phrases stay declarative.
func matchesAny(s string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasPrefix(s, p) {
			return true
		}
	}

	return false
}

// isBuildxNoise matches lines of the form `#<digits> ...` that docker
// buildx emits for every internal step (transferring, resolving,
// DONE, CACHED, and so on). These compose 80%+ of a build's stderr
// and are exactly what the spinner is meant to hide. Anything else —
// including real error traces like "ERROR: failed to solve" — passes
// through untouched.
func isBuildxNoise(s string) bool {
	if len(s) < 2 || s[0] != '#' {
		return false
	}

	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == ' ' {
			return i > 1
		}

		if c < '0' || c > '9' {
			return false
		}
	}

	// No space after digits (rare: just `#12`) — still buildx-shaped.
	return true
}

// currentDeployTag reads the most recent "Shipping <name>" line the
// client itself printed. When we are in the middle of pushing a tar
// for "web", the banner was already shown and `tag` becomes "web".
// Fallback "release" keeps the spinner generic when we couldn't
// capture a tag (e.g. future caller that skips the banner).
var lastShippedTag string

func currentDeployTag() string {
	if lastShippedTag != "" {
		return lastShippedTag
	}

	return "release"
}

// rememberShippedTag is called from pushSourceViaTarball right before
// the SSH push kicks off. The filter picks it up on the next
// buildStartMarker it sees. Not threadsafe across parallel pushes —
// we intentionally serialize deploys today, and the filter runs
// single-threaded within one SSH call.
func rememberShippedTag(tag string) {
	lastShippedTag = tag
}

// startSpinnerLocked kicks off the animation goroutine. Called with
// f.mu held. The goroutine calls tick(), which takes its own lock —
// safe because we release mu between channel reads.
//
// `\x1b[?25l` hides the terminal cursor for the lifetime of the
// spinner — without this, the cursor's blinking block sits on top
// of the spinner glyph and reads as a "white flash" behind every
// frame. Restored in stopSpinnerLocked. Only emitted when out is a
// TTY: piping to a file or pager would leave a stray escape that
// some viewers render literally.
func (f *progressFilter) startSpinnerLocked() {
	f.stopCh = make(chan struct{})
	f.doneCh = make(chan struct{})

	if f.tty {
		fmt.Fprint(f.out, "\x1b[?25l")
	}

	go f.spinLoop(f.stopCh, f.doneCh)
}

func (f *progressFilter) spinLoop(stop, done chan struct{}) {
	defer close(done)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			f.tick()
		}
	}
}

func (f *progressFilter) tick() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.advanceAndRenderLocked()
}

// advanceAndRenderLocked bumps the spinner frame by one and repaints
// the current step line. The timer is per-step (not overall) so the
// user sees "Building release... (3s)" as the build churns — resetting
// on each new step keeps the number meaningful. Caller must hold f.mu.
func (f *progressFilter) advanceAndRenderLocked() {
	if !f.active || f.currentStep == "" {
		return
	}

	frames := []rune(spinnerFrames)
	f.frame = (f.frame + 1) % len(frames)

	f.renderSpinnerLocked()
}

// stopSpinnerLocked signals the goroutine to exit and waits. Lock is
// held on entry; we release it across the channel wait so the
// goroutine's last tick() can acquire it, then reacquire.
//
// Restores the terminal cursor (`\x1b[?25h`) hidden by
// startSpinnerLocked. Skipping this leaves the user's cursor
// invisible after the apply finishes — confusing and easy to
// forget. Only emitted when out is a TTY (matches start).
func (f *progressFilter) stopSpinnerLocked() {
	if f.stopCh == nil {
		return
	}

	close(f.stopCh)
	stopCh := f.stopCh
	doneCh := f.doneCh

	f.mu.Unlock()
	<-doneCh
	f.mu.Lock()

	_ = stopCh

	f.stopCh = nil
	f.doneCh = nil

	if f.tty {
		fmt.Fprint(f.out, "\x1b[?25h")
	}
}

// CommitStep finalizes the currently-open step as a green ✓ without
// requiring a text-level end marker. Used by callers that orchestrate
// a spinner around a discrete unit of work (e.g. the phase-1 diff in
// apply_forwarded.go) where there's no "Build completed" signal in the
// stream — the caller just knows when the work is done. No-op in
// verbose / non-TTY mode, matching the rest of the filter's escape
// hatches.
func (f *progressFilter) CommitStep() {
	if f.verbose || !f.tty {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.closeCurrentStepLocked()

	if f.active {
		f.stopSpinnerLocked()
		f.active = false
	}
}

// Close flushes any trailing partial line and stops a running
// spinner. Call this when the underlying SSH completes — otherwise
// a mid-line byte at EOF would be lost and a never-ended build would
// leave a dangling spinner on screen.
func (f *progressFilter) Close() error {
	if f.verbose || !f.tty {
		return nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.active {
		f.stopSpinnerLocked()
		// Dirty close: never saw an end marker (crash, Ctrl-C, SSH
		// dropped). Clear the spinner line without committing the
		// current step to scrollback — printing ✓ here would be lying,
		// since the step didn't actually finish. The caller's error
		// message lands on the cleaned row next.
		fmt.Fprint(f.out, "\r\x1b[2K")
		f.active = false
		f.currentStep = ""
	}

	if len(f.leftover) > 0 {
		_, _ = f.out.Write(f.leftover)
		f.leftover = nil
	}

	return nil
}

// applyResultTokens are the verbs the server's apply output ends with
// (or contains, in the case of "pruned (…)"). Line classification for
// the applyResultFilter below. Keep in sync with what cmd/cli/apply.go
// prints after the controller returns its per-manifest result.
var applyResultTokens = []string{
	" applied",
	" created",
	" unchanged",
	" deleted",
	" pruned",
}

// applyResultFilter styles the phase-3 `voodu apply` output so the
// server's plain status lines ("<kind>/<scope>/<name> applied",
// "<ref> pruned (removed from manifests)") gain the same green ✓
// treatment as the build-phase steps. Non-matching lines pass
// through unchanged — this is pure presentation, not content.
//
// Used only on the forwarded apply orchestrator's phase 3 SSH call,
// and only when the server speaks the legacy text format. Modern
// NDJSON servers emit typed result events that eventRenderer handles
// directly; the negotiatingWriter picks between the two at runtime
// based on the server's hello frame. Phase 1 (diff) uses the
// structured JSON capture, which doesn't flow through here. Escape
// hatches mirror progressFilter: verbose or non-TTY → raw passthrough.
type applyResultFilter struct {
	out     io.Writer
	verbose bool
	tty     bool

	mu       sync.Mutex
	leftover []byte
}

func newApplyResultFilter(out io.Writer, verbose bool) *applyResultFilter {
	return &applyResultFilter{
		out:     out,
		verbose: verbose,
		tty:     writerIsTerminal(out),
	}
}

func (a *applyResultFilter) Write(p []byte) (int, error) {
	if a.verbose || !a.tty {
		return a.out.Write(p)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	data := append(a.leftover, p...)

	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}

		line := string(data[:idx])
		data = data[idx+1:]

		if isApplyResultLine(line) {
			fmt.Fprintf(a.out, "\x1b[32m✓\x1b[0m %s\n", line)
		} else {
			fmt.Fprintln(a.out, line)
		}
	}

	a.leftover = append(a.leftover[:0], data...)

	return len(p), nil
}

func (a *applyResultFilter) Close() error {
	if a.verbose || !a.tty {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.leftover) > 0 {
		_, _ = a.out.Write(a.leftover)
		a.leftover = nil
	}

	return nil
}

// isApplyResultLine reports whether s is one of the server's per-
// resource status lines. Every such line has a "<kind>/…" shape (the
// slash is the reliable signal the controller is talking about a
// manifest, not free-form text) and contains one of the known verbs.
func isApplyResultLine(s string) bool {
	trimmed := strings.TrimSpace(s)
	if !strings.Contains(trimmed, "/") {
		return false
	}

	for _, tok := range applyResultTokens {
		if strings.Contains(trimmed, tok) {
			return true
		}
	}

	return false
}
