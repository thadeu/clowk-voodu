package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/progress"
)

// captureWriter is a test double that records everything written to
// it and captures its Close call. Used to stand in for both "legacy"
// and "ndjson" sides of the negotiator so tests can assert which one
// the sniffer picked.
type captureWriter struct {
	buf    bytes.Buffer
	closed bool
}

func (c *captureWriter) Write(p []byte) (int, error) {
	return c.buf.Write(p)
}

func (c *captureWriter) Close() error {
	c.closed = true

	return nil
}

var _ io.WriteCloser = (*captureWriter)(nil)

// TestNegotiatingWriterPicksNDJSONOnHello exercises the happy path
// for a modern server: first line parses as a hello frame with the
// current protocol version, so every subsequent byte routes to the
// ndjson writer. The legacy writer must receive zero bytes — we're
// not supposed to double-feed either side.
func TestNegotiatingWriterPicksNDJSONOnHello(t *testing.T) {
	legacy := &captureWriter{}
	nd := &captureWriter{}

	n := newNegotiatingWriter(legacy, nd)

	hello := `{"type":"hello","protocol":"` + progress.ProtocolVersion + `"}` + "\n"
	payload := hello + `{"type":"log","text":"building"}` + "\n"

	if _, err := n.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}

	if legacy.buf.Len() != 0 {
		t.Errorf("legacy must receive zero bytes when hello matches:\n  got: %q", legacy.buf.String())
	}

	if nd.buf.String() != payload {
		t.Errorf("ndjson must receive the full payload:\n  got:  %q\n  want: %q", nd.buf.String(), payload)
	}
}

// TestNegotiatingWriterFallsBackOnNonHello covers the legacy-server
// case: the first line is `-----> ...`, not a hello frame, so the
// entire stream (including what we buffered during the peek) must
// reach the legacy filter verbatim.
func TestNegotiatingWriterFallsBackOnNonHello(t *testing.T) {
	legacy := &captureWriter{}
	nd := &captureWriter{}

	n := newNegotiatingWriter(legacy, nd)

	payload := "-----> Receiving build context for 'app'...\n" +
		"-----> Creating release abc123\n"

	if _, err := n.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}

	if nd.buf.Len() != 0 {
		t.Errorf("ndjson must receive zero bytes when hello is absent:\n  got: %q", nd.buf.String())
	}

	if legacy.buf.String() != payload {
		t.Errorf("legacy must receive the full payload:\n  got:  %q\n  want: %q", legacy.buf.String(), payload)
	}
}

// TestNegotiatingWriterBuffersUntilFirstNewline guards against an
// early-decision bug: if the server is slow to flush and only some
// bytes of the first line arrive in the first Write, the negotiator
// must keep buffering, not decide prematurely.
func TestNegotiatingWriterBuffersUntilFirstNewline(t *testing.T) {
	legacy := &captureWriter{}
	nd := &captureWriter{}

	n := newNegotiatingWriter(legacy, nd)

	// Partial hello — no newline yet.
	if _, err := n.Write([]byte(`{"type":"hell`)); err != nil {
		t.Fatal(err)
	}

	if legacy.buf.Len() != 0 || nd.buf.Len() != 0 {
		t.Errorf("no decision should be made before first newline")
	}

	// Complete the hello line.
	if _, err := n.Write([]byte(`o","protocol":"` + progress.ProtocolVersion + `"}` + "\n")); err != nil {
		t.Fatal(err)
	}

	if nd.buf.Len() == 0 {
		t.Errorf("ndjson should have received the full hello line after it was completed")
	}
}

// TestNegotiatingWriterRejectsOtherVersion locks in the version
// precision: a hello with a different protocol (newer/older server
// than we know how to speak) must NOT route to ndjson. We fall back
// to legacy so the user at least sees raw text output rather than a
// silently broken NDJSON stream.
func TestNegotiatingWriterRejectsOtherVersion(t *testing.T) {
	legacy := &captureWriter{}
	nd := &captureWriter{}

	n := newNegotiatingWriter(legacy, nd)

	// Hypothetical future protocol we don't support.
	hello := `{"type":"hello","protocol":"ndjson/99"}` + "\n"

	if _, err := n.Write([]byte(hello)); err != nil {
		t.Fatal(err)
	}

	if nd.buf.Len() != 0 {
		t.Errorf("unknown protocol must not route to ndjson:\n  got: %q", nd.buf.String())
	}

	if !strings.Contains(legacy.buf.String(), "ndjson/99") {
		t.Errorf("unknown protocol must fall back to legacy verbatim:\n  got: %q", legacy.buf.String())
	}
}

// TestNegotiatingWriterCloseFlushesUndecided handles the edge where
// the server produced output that never contained a newline (zero
// bytes, or a weird pathological stream). Close flushes the peek
// buffer to the legacy filter — text mode is the forgiving default.
func TestNegotiatingWriterCloseFlushesUndecided(t *testing.T) {
	legacy := &captureWriter{}
	nd := &captureWriter{}

	n := newNegotiatingWriter(legacy, nd)

	if _, err := n.Write([]byte("no newline ever came")); err != nil {
		t.Fatal(err)
	}

	if err := n.Close(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(legacy.buf.String(), "no newline ever came") {
		t.Errorf("undecided Close must flush peek to legacy:\n  got: %q", legacy.buf.String())
	}

	if !legacy.closed || !nd.closed {
		t.Errorf("Close must close both writers: legacy=%v ndjson=%v", legacy.closed, nd.closed)
	}
}
