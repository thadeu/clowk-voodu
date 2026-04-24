package main

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"

	"go.voodu.clowk.in/internal/progress"
)

// negotiatingWriter is the client-side protocol dispatcher. It peeks
// at the server's first stdout line and routes every subsequent byte
// to one of two pre-constructed writers:
//
//   - `ndjson`: an eventRenderer that decodes typed NDJSON frames.
//   - `legacy`: a progressFilter / applyResultFilter that parses the
//     old `-----> Banner` text format.
//
// The decision criterion is a single question: does line one parse as
// a `hello` event with the exact wire version the client was compiled
// against? If yes, NDJSON is on. If no — malformed, stray text, an
// unrelated banner, a panic trace, anything — we fall back to legacy
// rendering and dump the bytes we buffered into it.
//
// Why this exists: `voodu` CLI is deployed independently of the
// server binary. A client linked against ndjson/1 may hit a server
// that still only knows the text format (older deploy, feature-
// flagged off, plugin that doesn't speak progress). Without this
// sniffer, we'd need out-of-band probing or a flag the user sets.
// Peeking the first line is free and auto-negotiates across the
// boundary.
//
// Contract with the caller:
//   - Pass both writers pre-constructed. Close() forwards to the
//     chosen one; if no decision was ever reached (zero bytes, or a
//     stream that ended before emitting a newline), we close both to
//     release any background goroutines (spinners, tickers).
//   - The writer is not safe to decode NDJSON from if the server
//     emits a partial first frame without a trailing newline. That's
//     a pathological case we don't bother guarding — our servers
//     always emit Hello first and fully.
type negotiatingWriter struct {
	legacy io.WriteCloser
	ndjson io.WriteCloser

	mu      sync.Mutex
	decided bool
	target  io.WriteCloser
	peek    bytes.Buffer
}

// newNegotiatingWriter wires a peek-and-route writer. Both `legacy`
// and `ndjson` are constructed unconditionally — only one will end up
// receiving bytes, but having both ready means the decision is a
// pointer swap, not a lazy constructor call under lock.
func newNegotiatingWriter(legacy, ndjson io.WriteCloser) *negotiatingWriter {
	return &negotiatingWriter{legacy: legacy, ndjson: ndjson}
}

// Write implements io.Writer. Until the first newline is observed,
// bytes accumulate in the peek buffer. Once the first line lands,
// the decision is made and all buffered bytes + any trailing bytes
// from this same Write are forwarded. Subsequent Writes go straight
// to the chosen target with no further buffering.
func (n *negotiatingWriter) Write(p []byte) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.decided {
		if _, err := n.target.Write(p); err != nil {
			return 0, err
		}

		return len(p), nil
	}

	n.peek.Write(p)

	buf := n.peek.Bytes()

	idx := bytes.IndexByte(buf, '\n')
	if idx < 0 {
		// Still accumulating — caller's bytes are in peek, report
		// full acceptance so the io.Writer contract is satisfied.
		return len(p), nil
	}

	firstLine := buf[:idx]
	rest := buf[idx+1:]

	var e progress.Event
	if err := json.Unmarshal(bytes.TrimSpace(firstLine), &e); err == nil &&
		e.Type == progress.EventHello &&
		e.Protocol == progress.ProtocolVersion {
		n.decided = true
		n.target = n.ndjson

		// Forward the entire peek buffer (hello line + any bytes
		// already received beyond it). The eventRenderer's own
		// line-splitting will classify each frame.
		flush := append([]byte{}, firstLine...)
		flush = append(flush, '\n')
		flush = append(flush, rest...)

		if _, err := n.ndjson.Write(flush); err != nil {
			n.peek.Reset()

			return 0, err
		}
	} else {
		n.decided = true
		n.target = n.legacy

		// Forward buffered bytes verbatim — the legacy filter
		// expects the full banner stream including whatever the
		// server sent as its first line.
		if _, err := n.legacy.Write(n.peek.Bytes()); err != nil {
			n.peek.Reset()

			return 0, err
		}
	}

	n.peek.Reset()

	return len(p), nil
}

// Close flushes the chosen target. If no decision was ever reached
// (e.g. the server exited with zero output, or dropped before its
// first newline landed), flush whatever we buffered to the legacy
// path — text mode is the forgiving default — and close both writers
// to release goroutines they own (the legacy filter's spinner
// ticker; eventRenderer's ticker).
func (n *negotiatingWriter) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.decided {
		if n.peek.Len() > 0 {
			_, _ = n.legacy.Write(n.peek.Bytes())
			n.peek.Reset()
		}

		n.decided = true
		n.target = n.legacy
	}

	// Close both so the unchosen side doesn't leak its spinner
	// goroutine. A double-close on the chosen target is harmless —
	// both filter.Close / renderer.Close are idempotent (they check
	// their active flag).
	errLegacy := n.legacy.Close()
	errNDJSON := n.ndjson.Close()

	if errLegacy != nil {
		return errLegacy
	}

	return errNDJSON
}
