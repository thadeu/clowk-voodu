package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/progress"
)

// forceEventRenderer constructs a renderer with the TTY gate forced
// on so tests can exercise the state machine even though the test
// binary writes to a pipe. Mirrors forceFilter in
// stream_filter_test.go.
func forceEventRenderer(out *bytes.Buffer, verbose bool) *eventRenderer {
	r := newEventRenderer(out, verbose)
	r.tty = true

	return r
}

// writeEvents is a tiny helper that serializes each event to JSON +
// newline and pipes through the renderer, so tests read like the
// actual wire usage without boilerplate.
func writeEvents(t *testing.T, r *eventRenderer, events []progress.Event) {
	t.Helper()

	var buf bytes.Buffer

	for _, e := range events {
		if err := json.NewEncoder(&buf).Encode(e); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := r.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
}

// TestEventRendererVerboseIsPassthrough locks in the escape hatch:
// --verbose dumps the raw NDJSON line-by-line. Humans grepping a
// build log get the full typed stream without renderer decisions
// getting in the way.
func TestEventRendererVerboseIsPassthrough(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(&buf, true)

	payload := `{"type":"step_start","id":"build","label":"Building release..."}` + "\n"
	if _, err := r.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}

	_ = r.Close()

	if buf.String() != payload {
		t.Errorf("verbose mode must passthrough:\n got:  %q\n want: %q", buf.String(), payload)
	}
}

// TestEventRendererNonTTYIsPassthrough mirrors the verbose case for
// the other escape hatch. When stdout is not a TTY (CI, piped to
// file), we want the raw NDJSON stream preserved so parsers work.
func TestEventRendererNonTTYIsPassthrough(t *testing.T) {
	var buf bytes.Buffer

	r := newEventRenderer(&buf, false)
	// tty defaults to false because bytes.Buffer isn't an *os.File.

	payload := `{"type":"step_start","id":"build","label":"Building release..."}` + "\n"
	if _, err := r.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}

	_ = r.Close()

	if buf.String() != payload {
		t.Errorf("non-tty mode must passthrough:\n got:  %q\n want: %q", buf.String(), payload)
	}
}

// TestEventRendererRenderStepLifecycle is the happy path: start →
// end produces the `✓ <label> (Ns)` commit line. No buildx noise to
// filter, no Summary — just the structural event pair.
func TestEventRendererRenderStepLifecycle(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(&buf, false)

	rememberShippedTag("web")

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
		{Type: progress.EventStepStart, ID: "build", Label: "Building release..."},
		{Type: progress.EventStepEnd, ID: "build", Status: progress.StatusOK},
	})

	_ = r.Close()

	got := stripANSI(buf.String())

	if !strings.Contains(got, "✓ Building release...") {
		t.Errorf("expected ✓ Building release... in output:\n%s", got)
	}
}

// TestEventRendererBuildSummaryReplacesText confirms the client
// synthesises the overall `✓ Built <tag> in Ns` line from the
// Summary("Build completed") frame. The raw "Build completed" text
// must NOT appear — we replace it with the tagged summary.
func TestEventRendererBuildSummaryReplacesText(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(&buf, false)

	rememberShippedTag("api")

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
		{Type: progress.EventStepStart, ID: "build", Label: "Building release..."},
		{Type: progress.EventStepEnd, ID: "build", Status: progress.StatusOK},
		{Type: progress.EventSummary, Text: "Build completed"},
	})

	_ = r.Close()

	got := stripANSI(buf.String())

	if !strings.Contains(got, "✓ Built api in") {
		t.Errorf("expected ✓ Built api in … summary:\n%s", got)
	}

	if strings.Contains(got, "Build completed") {
		t.Errorf("raw 'Build completed' text must not leak when Built summary is synthesised:\n%s", got)
	}
}

// TestEventRendererDeployCompletedDroppedAfterBuild locks in the
// de-duplication rule: once Build completed has fired, Deploy
// completed is redundant and gets swallowed. Otherwise the user
// would see two "success" lines for one deploy.
func TestEventRendererDeployCompletedDroppedAfterBuild(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(&buf, false)

	rememberShippedTag("web")

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
		{Type: progress.EventStepStart, ID: "build", Label: "Building release..."},
		{Type: progress.EventStepEnd, ID: "build", Status: progress.StatusOK},
		{Type: progress.EventSummary, Text: "Build completed"},
		{Type: progress.EventSummary, Text: "Deploy completed successfully for 'softphone-web'"},
	})

	_ = r.Close()

	got := stripANSI(buf.String())

	if !strings.Contains(got, "✓ Built web in") {
		t.Errorf("expected ✓ Built web line:\n%s", got)
	}

	if strings.Contains(got, "Deploy completed successfully") {
		t.Errorf("Deploy completed must be swallowed after Built summary:\n%s", got)
	}
}

// TestEventRendererResultIsStyled verifies per-manifest apply lines
// get the green ✓ treatment and render as `kind/scope/name action`.
// This is the NDJSON-native sibling of applyResultFilter — the
// forwarder picks between them based on the server's handshake.
func TestEventRendererResultIsStyled(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(&buf, false)

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
		{Type: progress.EventResult, Kind: "deployment", Scope: "softphone", Name: "web", Action: "applied"},
		{Type: progress.EventResult, Kind: "service", Name: "router", Action: "unchanged"},
	})

	_ = r.Close()

	got := stripANSI(buf.String())

	for _, want := range []string{
		"✓ deployment/softphone/web applied",
		"✓ service/router unchanged",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

// TestEventRendererWarnEscapesSpinner makes sure warnings always
// surface even during an active step — the spinner swallows info
// chatter on purpose, but warnings are specifically what the user
// needs to see regardless. Mirrors how progressFilter handles the
// same categories.
func TestEventRendererWarnEscapesSpinner(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(&buf, false)

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
		{Type: progress.EventStepStart, ID: "build", Label: "Building release..."},
		{Type: progress.EventLog, Level: progress.LevelInfo, Text: "#1 internal stage (should be swallowed)"},
		{Type: progress.EventLog, Level: progress.LevelWarn, Text: "dockerfile is deprecated"},
		{Type: progress.EventStepEnd, ID: "build", Status: progress.StatusOK},
	})

	_ = r.Close()

	got := stripANSI(buf.String())

	if !strings.Contains(got, "⚠ dockerfile is deprecated") {
		t.Errorf("warn must always surface:\n%s", got)
	}

	if strings.Contains(got, "#1 internal stage") {
		t.Errorf("info-level log during active step must be swallowed:\n%s", got)
	}
}

// TestEventRendererStepFailCommitsRed ensures a failed step gets a
// red ✗ with the error on a second line — the visual twin of the
// ✓ commit for a success, but unambiguously a failure marker.
func TestEventRendererStepFailCommitsRed(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(&buf, false)

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
		{Type: progress.EventStepStart, ID: "build", Label: "Building release..."},
		{Type: progress.EventStepEnd, ID: "build", Status: progress.StatusFail, Error: "docker exited 125"},
	})

	_ = r.Close()

	got := stripANSI(buf.String())

	if !strings.Contains(got, "✗ Building release...") {
		t.Errorf("expected ✗ Building release...:\n%s", got)
	}

	if !strings.Contains(got, "docker exited 125") {
		t.Errorf("expected error detail in output:\n%s", got)
	}
}

// TestEventRendererCloseClearsDanglingSpinner is the crash-scenario
// test — SSH dropped mid-build, no step_end arrived. Close must stop
// the ticker goroutine cleanly and leave no false ✓.
func TestEventRendererCloseClearsDanglingSpinner(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(&buf, false)

	rememberShippedTag("api")

	writeEvents(t, r, []progress.Event{
		{Type: progress.EventHello, Protocol: progress.ProtocolVersion},
		{Type: progress.EventStepStart, ID: "build", Label: "Building release..."},
	})

	if err := r.Close(); err != nil {
		t.Fatalf("Close err = %v", err)
	}

	if r.active {
		t.Error("renderer still in active state after Close")
	}
}

// TestEventRendererUnknownTypeIgnored guards forward compatibility:
// a future event type should be silently ignored, not crash the
// renderer or leak the raw JSON into the user's terminal.
func TestEventRendererUnknownTypeIgnored(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(&buf, false)

	if _, err := r.Write([]byte(`{"type":"future_thing","payload":"whatever"}` + "\n")); err != nil {
		t.Fatal(err)
	}

	_ = r.Close()

	if strings.Contains(buf.String(), "future_thing") {
		t.Errorf("unknown event type must be silently ignored:\n%s", buf.String())
	}
}

// TestEventRendererNonJSONPrintsVerbatim handles the safety net
// case where a stray non-JSON line slips into the frame stream
// (e.g. a panic trace that escaped os.Stderr during a crash).
// Showing it raw is more useful than dropping it.
func TestEventRendererNonJSONPrintsVerbatim(t *testing.T) {
	var buf bytes.Buffer

	r := forceEventRenderer(&buf, false)

	if _, err := r.Write([]byte("panic: runtime error: nil map\n")); err != nil {
		t.Fatal(err)
	}

	_ = r.Close()

	if !strings.Contains(buf.String(), "panic: runtime error: nil map") {
		t.Errorf("non-JSON line must print verbatim:\n%s", buf.String())
	}
}
