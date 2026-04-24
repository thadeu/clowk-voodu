package progress

import (
	"bytes"
	"io"
	"sync"
)

// LineWriter adapts an io.Writer consumer (subprocess stdout, docker
// output, anything that writes free-form bytes) to a Reporter.Log call
// per complete line. This is the bridge that keeps raw subprocess
// output from corrupting the NDJSON wire stream: instead of a post-
// deploy `bash -c` writing directly to os.Stdout (and interleaving its
// bytes between our JSON frames), its output passes through LineWriter,
// which frames on \n and emits one Log event per line.
//
// Partial lines are buffered until the next \n or until Close is
// called. Close flushes whatever's left as a final Log.
//
// Not a drop-in replacement for os.Stdout in all cases — LineWriter
// doesn't preserve exact byte counts across Write calls because one
// input Write may map to zero or many Log calls. Callers that need
// byte-accurate piping (rare in a deploy context) should keep using
// the raw writer; this helper targets the "turn chatty subprocess
// output into structured log events" use case.
type LineWriter struct {
	reporter Reporter
	level    string

	mu  sync.Mutex
	buf bytes.Buffer
}

// NewLineWriter wires a LineWriter that routes every complete line
// through reporter.Log with the given level. Empty level defaults to
// info — matching JSONReporter.Log's own default.
func NewLineWriter(r Reporter, level string) *LineWriter {
	if level == "" {
		level = LevelInfo
	}

	return &LineWriter{reporter: r, level: level}
}

// Write implements io.Writer. Appends p to the internal buffer and
// flushes complete lines through the reporter. Always reports len(p)
// bytes accepted — the io.Writer contract is "did the caller's bytes
// make it into our keep-forever structure," and they did (either
// emitted as a Log, or held in buf for the next Write). Errors from
// the reporter are silently ignored for the same reason JSONReporter
// ignores them: a cosmetic log path shouldn't kill a deploy.
func (l *LineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.buf.Write(p)

	for {
		data := l.buf.Bytes()

		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}

		line := string(data[:idx])

		// Strip a trailing \r so CRLF-origin streams (unlikely in a
		// Linux container but possible from Windows-edited scripts)
		// produce clean Log text without the carriage return byte.
		line = trimTrailingCR(line)

		l.reporter.Log(l.level, line)

		// Consume the line and its terminator from the buffer.
		l.buf.Next(idx + 1)
	}

	return len(p), nil
}

// Close flushes any trailing partial line. Call this when the
// subprocess exits — otherwise the last line of output (when it
// doesn't end in \n) would be lost. Safe to call multiple times.
func (l *LineWriter) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.buf.Len() == 0 {
		return nil
	}

	line := trimTrailingCR(l.buf.String())
	l.buf.Reset()

	l.reporter.Log(l.level, line)

	return nil
}

// trimTrailingCR strips a single \r from the end of s. Kept as a
// helper instead of a strings.TrimRight so we don't accidentally eat
// legitimate trailing \r bytes in the middle of a weird payload; we
// only care about the one byte immediately before the newline.
func trimTrailingCR(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\r' {
		return s[:n-1]
	}

	return s
}

// Compile-time assertion: LineWriter satisfies io.WriteCloser so it
// can stand in wherever subprocess wiring expects to Close the stream.
var _ io.WriteCloser = (*LineWriter)(nil)
