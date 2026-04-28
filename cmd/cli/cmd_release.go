package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
	"go.voodu.clowk.in/internal/progress"
)

// newReleaseCmd is the deployment-release verb. Shape mirrors the
// rest of the kind-based CLI surface — `vd KIND <ref> [verb]` —
// so the operator only has to remember "name first, then what to
// do with it":
//
//	vd release <ref>            list recent releases (default verb)
//	vd release <ref> history    list recent releases (explicit)
//	vd release <ref> run        re-trigger the release-phase commands
//
// Rollback is a separate top-level verb, mirroring `heroku rollback`:
//
//	vd rollback <ref> [release_id]   revert to a past release
//
// Releases are declared in the deployment manifest:
//
//	deployment "clowk-lp" "web" {
//	  image = "clowk-lp:latest"
//
//	  release {
//	    command      = ["rails", "db:migrate"]
//	    timeout      = "5m"
//	    pre_command  = ["bin/preflight"]
//	    post_command = ["bin/notify"]
//	  }
//	}
//
// Notable omissions vs. the previous draft:
//
//   - No `vd release status`. The first row of `history` IS the
//     status. Two commands for one piece of information was noise.
//   - No `vd release logs`. `vd logs <ref>` already streams
//     container logs, including release containers (they share
//     the kind=job + name=<deploy>-release identity); a release-
//     specific wrapper would just shadow the existing path.
func newReleaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release <ref> [verb]",
		Short: "Inspect and re-trigger the deployment release phase",
		Long: `Voodu's release phase runs a manifest-declared command
(typically rails db:migrate, php artisan migrate, etc.) BEFORE the
rolling restart of replicas. Each release is identified by a short
sortable hash and a snapshot of the spec is preserved so you can
roll back to any past release with 'vd rollback'.

Verbs:

  history (default)   list recent release records
  run                 force-run the release for the current spec

Examples:
  vd release clowk-lp/web              # default → history
  vd release clowk-lp/web history      # same, explicit
  vd release clowk-lp/web run          # re-trigger release phase

Rollback is its own top-level verb:

  vd rollback clowk-lp/web              # rollback to previous release
  vd rollback clowk-lp/web 1ksdtcj7e    # rollback to a specific id`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]

			verb := "history"
			if len(args) == 2 {
				verb = strings.ToLower(strings.TrimSpace(args[1]))
			}

			switch verb {
			case "history", "":
				return releaseHistory(cmd, ref)
			case "run":
				return releaseRunStreaming(cmd, ref)
			default:
				return fmt.Errorf("unknown release verb %q (want history or run)", verb)
			}
		},
	}

	return cmd
}

// releaseRunStreaming POSTs to /releases/run and prints the
// streamed text/plain response body to stdout as it arrives. The
// final marker line ("-----> Release X failed in ...") determines
// the exit code. Different code path from rollback / history which
// use JSON envelopes — release run streams.
//
// Visual contract: a small in-process renderer (releaseRenderer)
// turns the wire format into the same vocabulary the build/apply
// pipeline uses. Each `----->` banner from the controller becomes a
// `✓ <label> (Xs)` line once the next banner arrives (i.e. the
// phase is done); container stdout in between is prefixed with
// `remote:` so it reads like the remote half of a `git push`. The
// final `✓ Released deployment/X in Xs` is emitted by the renderer
// itself, not the server — a single source of truth for the
// overall outcome.
func releaseRunStreaming(cmd *cobra.Command, ref string) error {
	scope, name := splitJobRef(ref)

	if name == "" {
		return fmt.Errorf("release ref %q is empty", ref)
	}

	q := url.Values{}
	q.Set("name", name)

	if scope != "" {
		q.Set("scope", scope)
	}

	root := cmd.Root()

	// Use a separate HTTP request without controllerDo's 30s
	// timeout — release runs can take minutes (slow migrations).
	base := strings.TrimRight(controllerURL(root), "/")
	full := base + "/releases/run?" + q.Encode()

	req, err := http.NewRequest(http.MethodPost, full, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", fmt.Sprintf("voodu-cli/%s", version))

	displayRef := ref
	if !strings.Contains(displayRef, "/") {
		displayRef = name
	}

	rdr := newReleaseRenderer(os.Stdout, displayRef)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		rdr.stop()
		return fmt.Errorf("controller POST /releases/run: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		rdr.stop()
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("controller returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	// Stream the body line-by-line into the renderer. Buffer is
	// 64KB initial / 1MB max so a long log line (stack trace,
	// migration ddl) doesn't trip Scanner's default 64KB cap.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		rdr.line(scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		rdr.stop()
		return fmt.Errorf("read response stream: %w", err)
	}

	// Failure signaled via the trailing banner's "failed" keyword.
	// commitStep first so the failed banner (which is the
	// currently-open step) gets its red ✗ line — without it, the
	// failure banner streams in but never anchors visually, and the
	// operator only sees the bare error message printed by the
	// caller. stop() then halts the spinner so the next CLI line
	// lands on a clean row.
	if rdr.failed() {
		rdr.commitStep()
		rdr.stop()
		return fmt.Errorf("%s", rdr.lastBannerLabel())
	}

	rdr.finish()

	return nil
}

// releaseRenderer translates the line-oriented release stream into
// the apply pipeline's visual vocabulary. Visual model:
//
//	remote:    Running crawler...           ← container stdout (live)
//	remote:    Crawled 0 pages...
//	✓ Release te6jz949: command (8s)         ← phase committed
//	remote:    ...
//	✓ Release te6jz949: rolling restart (5s)
//	⠋ Releasing deployment/clowk-lp/web... (12s)   ← spinner top-line
//
// On finish, the spinner line collapses into:
//
//	✓ Released deployment/clowk-lp/web in 15s
//
// State machine on each input line:
//
//   - `----->` banner → commit previous phase as `✓ <prev> (Xs)`,
//     start a new phase. The outer "Releasing deployment/X..." is
//     special: it powers the spinner top-line and is committed only
//     on finish().
//   - any other line → print as `remote:    <line>` above the
//     spinner; spinner gets redrawn on its own row below.
//
// TTY-aware: when stdout isn't a terminal (CI, file redirect), the
// spinner machinery is skipped and lines stream sequentially. The
// content (banners + remote: lines) stays identical so log greps
// don't change behavior between local and CI runs.
type releaseRenderer struct {
	out        io.Writer
	displayRef string

	started time.Time // overall release start, drives the spinner timer

	// Current sub-phase (Release X: command etc.). Distinct from the
	// outer "Releasing deployment/X..." which is owned by the
	// spinner top-line.
	stepLabel string
	stepStart time.Time

	// last tracks the most recent banner label so the caller can
	// detect the "failed" keyword for exit-code purposes.
	last string

	// Spinner machinery. Active only when out is a TTY. Mutex
	// serialises all writes to out so the goroutine's spinner
	// repaint never interleaves bytes with line() prints.
	mu       sync.Mutex
	tty      bool
	color    bool // emit ANSI color codes — true on local TTY OR over SSH
	frame    int
	stopCh   chan struct{}
	doneCh   chan struct{}
	hasDrawn bool // true once the spinner has painted at least once
}

// newReleaseRenderer builds a renderer wired to out. When out is a
// TTY, the spinner goroutine is started immediately so the operator
// sees "⠋ Releasing deployment/X... (0s)" right away, even before
// the first banner arrives over the wire.
//
// Two TTY-ish notions, deliberately separate:
//
//   - `tty` (writerIsTerminal): the spinner machinery (cursor
//     hide/show, in-place line redraws) only makes sense against
//     a real interactive terminal. Off when piping to a file or
//     running server-side over SSH.
//
//   - `color` (tty || VOODU_PROTOCOL set): controls whether to
//     emit ANSI color codes for the ✓ / ✗ glyphs. The codes are
//     just bytes — when this CLI runs inside the SSH-forwarded
//     apply pipeline (server side, where stdout isn't a TTY) the
//     client at the other end IS a TTY and renders the codes
//     correctly. The forwarder always sets VOODU_PROTOCOL on the
//     remote env, so its presence is a reliable signal that "the
//     ultimate consumer is a terminal-aware client".
//
// Without the second check, server-side release output would lose
// its colors entirely on every `voodu apply` over SSH — the user
// would see plain `✓ Release ...` instead of the green checkmarks
// the build steps already get.
func newReleaseRenderer(out io.Writer, displayRef string) *releaseRenderer {
	tty := writerIsTerminal(out)

	r := &releaseRenderer{
		out:        out,
		displayRef: displayRef,
		started:    time.Now(),
		tty:        tty,
		color:      tty || os.Getenv(progress.EnvProtocol) != "",
	}

	if r.tty {
		r.stopCh = make(chan struct{})
		r.doneCh = make(chan struct{})

		// Hide the terminal cursor while the spinner is animating.
		// Without this, the cursor's blinking block sits on top of
		// the spinner glyph and looks like a white flash behind
		// every frame. Restored by stop().
		fmt.Fprint(r.out, "\x1b[?25l")

		go r.spin()
	}

	return r
}

// spin repaints the spinner top-line every 100ms. Runs until stopCh
// closes; on exit, the line is left intact so finish() can overwrite
// it with the final ✓ summary in one atomic Write.
func (r *releaseRenderer) spin() {
	defer close(r.doneCh)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	frames := []rune(spinnerFrames)

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.mu.Lock()

			r.frame = (r.frame + 1) % len(frames)
			r.paintSpinner(frames[r.frame])

			r.mu.Unlock()
		}
	}
}

// paintSpinner writes the in-place spinner line. Caller holds r.mu.
// `\r` returns to column 0 and `\033[K` clears to end-of-line so the
// previous frame is wiped cleanly even if the new label is shorter.
func (r *releaseRenderer) paintSpinner(frame rune) {
	elapsed := time.Since(r.started).Round(time.Second)

	fmt.Fprintf(r.out, "\r\033[K%c Releasing deployment/%s... (%s)",
		frame, r.displayRef, elapsed)

	r.hasDrawn = true
}

// clearSpinner erases the spinner row so a fresh line can be
// written without colliding with the spinner text. No-op when no
// spinner has painted yet (avoids a stray escape on first content).
// Caller holds r.mu.
func (r *releaseRenderer) clearSpinner() {
	if !r.hasDrawn {
		return
	}

	fmt.Fprint(r.out, "\r\033[K")
}

// line consumes one line from the release stream. Banners commit
// the previous phase + open a new one; everything else is container
// output and gets the `remote:` prefix. Both paths funnel through
// the spinner-aware print path so the bottom-row spinner never
// gets corrupted by interleaved writes.
func (r *releaseRenderer) line(s string) {
	if strings.HasPrefix(s, "-----> ") {
		label := strings.TrimPrefix(s, "-----> ")

		r.commitStep()

		r.mu.Lock()
		r.stepLabel = label
		r.stepStart = time.Now()
		r.last = label
		r.mu.Unlock()

		return
	}

	r.printAboveSpinner(fmt.Sprintf("  remote:    %s\n", s))
}

// printAboveSpinner writes text in the row currently occupied by
// the spinner (clearing it first), then redraws the spinner on the
// next row. The mutex keeps this atomic vs. the spinner goroutine.
func (r *releaseRenderer) printAboveSpinner(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.clearSpinner()

	fmt.Fprint(r.out, text)

	if r.tty {
		frames := []rune(spinnerFrames)
		r.paintSpinner(frames[r.frame])
	}
}

// commitStep prints `✓ <label> (Xs)` for the in-progress phase, if
// any. Idempotent — calling without an open phase does nothing.
// Failure phases (label contains "failed") get a red ✗ instead so
// the transcript visually distinguishes the broken step.
//
// Colors mirror progressFilter (stream_filter.go) byte-for-byte so
// release output blends visually with the build steps:
//
//	\x1b[32m  green FG     for the ✓ glyph
//	\x1b[31m  red   FG     for the ✗ glyph
//	\x1b[2m   dim          for the elapsed time
//	\x1b[0m   reset
//
// Tests against a non-TTY writer skip the color codes via the same
// tty gate the spinner uses, so captured output stays plain.
func (r *releaseRenderer) commitStep() {
	r.mu.Lock()
	label, start := r.stepLabel, r.stepStart
	r.stepLabel = ""
	r.mu.Unlock()

	if label == "" {
		return
	}

	elapsed := time.Since(start).Round(time.Second)

	r.printAboveSpinner(formatStepLine(label, elapsed, r.color))
}

// formatStepLine renders the closed-step line. Pulled out of
// commitStep so finish() can reuse the same shape for the overall
// `Released deployment/X in Xs` summary without duplicating the
// color logic.
//
// `color` controls whether ANSI escapes are emitted. Decoupled
// from the spinner's TTY check on purpose: when this CLI runs
// server-side via the SSH-forwarded apply, stdout isn't a TTY but
// the bytes still flow back to a terminal-aware client that
// renders the colors correctly. See newReleaseRenderer for the
// full rationale.
func formatStepLine(label string, elapsed time.Duration, color bool) string {
	glyph := "✓"
	colorGlyph := "\x1b[32m✓\x1b[0m"

	if strings.Contains(label, "failed") {
		glyph = "✗"
		colorGlyph = "\x1b[31m✗\x1b[0m"
	}

	if !color {
		return fmt.Sprintf("%s %s (%s)\n", glyph, label, elapsed)
	}

	return fmt.Sprintf("%s %s \x1b[2m(%s)\x1b[0m\n", colorGlyph, label, elapsed)
}

// finish commits the trailing phase, stops the spinner, and prints
// the overall summary. Caller invokes this only on success —
// failure paths call stop() directly so we don't emit a misleading
// green ✓ footer.
func (r *releaseRenderer) finish() {
	r.commitStep()

	r.stop()

	footer := formatStepLine(
		fmt.Sprintf("Released deployment/%s", r.displayRef),
		time.Since(r.started).Round(time.Second),
		r.color,
	)

	fmt.Fprint(r.out, footer)
}

// stop halts the spinner goroutine, erases the spinner line, and
// restores the cursor. Idempotent — safe to call from both the
// success and failure paths. Restoring the cursor here matches the
// hide in newReleaseRenderer; without it, the user's terminal stays
// cursorless after the release returns.
func (r *releaseRenderer) stop() {
	if !r.tty || r.stopCh == nil {
		return
	}

	close(r.stopCh)
	<-r.doneCh

	r.stopCh = nil

	r.mu.Lock()
	r.clearSpinner()
	fmt.Fprint(r.out, "\x1b[?25h")
	r.mu.Unlock()
}

// failed reports whether the trailing banner signaled a failure.
// The server uses `-----> Release X failed in command (exit 42)`
// so a substring check on the captured label is enough.
func (r *releaseRenderer) failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return strings.Contains(r.last, "failed")
}

// lastBannerLabel returns the most recent banner's text (no
// `----->` prefix), used by the caller to compose the error
// message when failed() is true.
func (r *releaseRenderer) lastBannerLabel() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.last
}

// releaseHistory pulls the deployment's status via /describe and
// renders the release history table. Reuses the existing /describe
// endpoint so we don't duplicate scope-resolution logic.
//
// `vd release <ref>` defaults here. Newest entry is row zero —
// that's the operator's "current release"; subsequent rows are the
// rollback candidates. No separate "status" command because the
// first row is the status.
func releaseHistory(cmd *cobra.Command, ref string) error {
	scope, name := splitJobRef(ref)

	if name == "" {
		return fmt.Errorf("ref %q is empty", ref)
	}

	q := url.Values{}
	q.Set("kind", "deployment")
	q.Set("name", name)

	if scope != "" {
		q.Set("scope", scope)
	}

	resp, err := controllerDo(cmd.Root(), http.MethodGet, "/describe", q.Encode(), nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("controller returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var env struct {
		Data struct {
			Status json.RawMessage `json:"status,omitempty"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if len(env.Data.Status) == 0 || string(env.Data.Status) == "null" {
		fmt.Println("(no status recorded yet)")
		return nil
	}

	var st controller.DeploymentStatus

	if err := json.Unmarshal(env.Data.Status, &st); err != nil {
		return fmt.Errorf("decode status: %w", err)
	}

	if len(st.Releases) == 0 {
		fmt.Println("(no releases recorded yet)")
		return nil
	}

	renderReleaseHistory(os.Stdout, st.Releases)

	return nil
}

func renderReleaseHistory(w io.Writer, records []controller.ReleaseRecord) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintln(tw, "RELEASE\tSTATUS\tHASH\tIMAGE\tROLLED_BACK_FROM\tSTARTED\tDURATION")

	for _, r := range records {
		duration := "-"
		if !r.EndedAt.IsZero() && !r.StartedAt.IsZero() {
			duration = r.EndedAt.Sub(r.StartedAt).Round(time.Millisecond).String()
		}

		started := "-"
		if !r.StartedAt.IsZero() {
			started = r.StartedAt.UTC().Format(time.RFC3339)
		}

		rolledBackFrom := "-"
		if r.RolledBackFrom != "" {
			rolledBackFrom = r.RolledBackFrom
		}

		image := r.Image
		if image == "" {
			image = "-"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Status, shortHashCLI(r.SpecHash), image, rolledBackFrom, started, duration)
	}

	_ = tw.Flush()
}

// shortHashCLI is a CLI-side mirror of the controller's shortHash —
// trimming a sha-prefix to 8 chars for human-readable display.
func shortHashCLI(h string) string {
	if len(h) > 8 {
		return h[:8]
	}

	return h
}
