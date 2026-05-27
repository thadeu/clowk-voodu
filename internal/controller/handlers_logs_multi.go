// handlers_logs_multi.go owns GET /logs — the multi-pod fan-out
// counterpart to GET /pods/{name}/logs. Server-side aggregation so
// `vd logs <scope> -f` and the WebUI's `/logs` view share one
// implementation instead of each repeating the fan-out client-side.
//
// Response is chunked text/plain. Each line carries a [pod-name]
// prefix exactly like the CLI's multi-target rendering, so a single
// stream can be parsed line-by-line and attributed back to its pod
// of origin without inventing a structured wire format.

package controller

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// handleLogsMulti fans out the request to every pod that matches
// the (kind, scope, name) filter and multiplexes their lines into
// one chunked response.
//
//	GET /logs?follow=&tail=&kind=&scope=&name=
//
// Filters use the same vocabulary as GET /pods so the same query
// works on both endpoints. `follow=true` keeps every per-pod stream
// open until the client disconnects (cancellation propagates via
// the request context); `tail=N` caps the historical backfill per
// pod.
//
// Concurrency: one goroutine per pod streams in parallel, writes
// serialised by a mutex around the response writer. Ordering BETWEEN
// pods is best-effort (whichever pod's docker-logs read returns
// first writes first); per-line atomicity is guaranteed. Operators
// who want strict ordering can use GET /pods/{name}/logs per pod.
//
// Errors:
//
//   - 503 if the controller wasn't wired with a LogStreamer (test
//     setups, misconfiguration)
//   - 400 on malformed `tail`
//   - 200 on a zero-match filter — the body is empty, but the
//     header X-Voodu-Containers carries the (empty) match list so
//     the caller can distinguish "no pods" from "transport
//     problem". Same headers a happy match returns.
//   - Per-pod open failures emit `[pod-name] [stream error] msg`
//     into the multiplexed body and continue with the rest. Better
//     than 500ing the whole call when one pod is gone — a /logs
//     for a scope of 20 shouldn't black out because one container
//     was deleted between match and stream-open.
func (a *API) handleLogsMulti(w http.ResponseWriter, r *http.Request) {
	if a.Logs == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("log streaming not configured"))
		return
	}

	lister := a.Pods
	if lister == nil {
		lister = DockerPodsLister{}
	}

	q := r.URL.Query()

	follow := q.Get("follow") == "true"

	tail := 0
	if t := strings.TrimSpace(q.Get("tail")); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("tail must be a non-negative integer"))
			return
		}

		tail = n
	}

	wantKind := strings.TrimSpace(q.Get("kind"))
	wantScope := strings.TrimSpace(q.Get("scope"))
	wantName := strings.TrimSpace(q.Get("name"))

	// `since` filters lines emitted at/after the given timestamp.
	// Passed verbatim to docker logs --since (it accepts RFC3339
	// absolute, relative duration like "10m", or unix string). We
	// trust the caller to format it correctly; bad values get
	// rejected by docker itself and surface as an error line.
	//
	// Designed for polling consumers (WebUI's LogTailIslandJob):
	// they advance a watermark each poll and ask only for what's
	// new, killing the redundant re-tail of the last 500 lines
	// every cycle.
	wantSince := strings.TrimSpace(q.Get("since"))

	pods, err := lister.ListPods()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("list pods: %w", err))
		return
	}

	matches := filterPodsForLogs(pods, wantKind, wantScope, wantName)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Voodu-Containers", strings.Join(podNames(matches), ","))
	w.WriteHeader(http.StatusOK)

	if len(matches) == 0 {
		return
	}

	streamMultiplexedLogs(r.Context(), w, a.Logs, matches, LogsOptions{
		Follow: follow,
		Tail:   tail,
		Since:  wantSince,
	})
}

// filterPodsForLogs is the same filter shape handlePods uses, kept
// local so the logs path doesn't accidentally inherit a behaviour
// change meant for pod listing.
func filterPodsForLogs(pods []Pod, kind, scope, name string) []Pod {
	if kind == "" && scope == "" && name == "" {
		return pods
	}

	out := make([]Pod, 0, len(pods))

	for _, p := range pods {
		if kind != "" && p.Kind != kind {
			continue
		}

		if scope != "" && p.Scope != scope {
			continue
		}

		if name != "" && p.ResourceName != name {
			continue
		}

		out = append(out, p)
	}

	return out
}

func podNames(pods []Pod) []string {
	out := make([]string, len(pods))
	for i, p := range pods {
		out[i] = p.Name
	}

	return out
}

// streamMultiplexedLogs opens one log stream per pod in parallel and
// pipes every line into `dst`, prefixed with `[pod-name] `. Flushes
// after each write so the client (CLI, WebUI) sees lines as they
// arrive without waiting for a chunked-transfer page boundary.
//
// Cancellation: when ctx fires (client disconnect, server shutdown)
// every per-pod stream is closed and goroutines exit. The function
// returns once all goroutines have unwound.
func streamMultiplexedLogs(ctx context.Context, dst io.Writer, source LogStreamer, pods []Pod, opts LogsOptions) {
	flusher, _ := dst.(http.Flusher)

	// writeMu serialises all writes to dst so per-line prefixes stay
	// atomic across goroutines. Without it two pods scribbling at
	// the same time could interleave bytes within a single line.
	// The heartbeat goroutine shares this mutex with the per-pod
	// streams.
	var writeMu sync.Mutex

	write := func(b []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()

		// Best-effort: if the client is gone, dst.Write returns an
		// error; we let the goroutine notice and exit naturally on
		// its next read (which will fail too once the upstream
		// context cancels).
		_, _ = dst.Write(b)

		if flusher != nil {
			flusher.Flush()
		}
	}

	// writeOK variant for the keepalive ticker — needs the bool
	// return to know when to stop (broken pipe == client gone).
	writeOK := func(b []byte) bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := dst.Write(b); err != nil {
			return false
		}

		if flusher != nil {
			flusher.Flush()
		}

		return true
	}

	// Empty-line heartbeat — see handlePodLogs for the rationale.
	// Same cadence (keepaliveInterval), same WebUI invisibility
	// (parser drops empty lines). Without it, a /logs?follow with
	// no log activity for ~30s appears in the Rails log feed as
	// `Net::ReadTimeout with TCPSocket:(closed)` and the feed dies
	// until the operator hits refresh.
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()

	go runLogKeepalive(hbCtx, writeOK)

	var wg sync.WaitGroup

	for _, p := range pods {
		p := p
		wg.Add(1)

		go func() {
			defer wg.Done()

			stream, err := source.Logs(p.Name, opts)
			if err != nil {
				write([]byte(fmt.Sprintf("[%s] [stream error] %s\n", p.Name, err)))
				return
			}

			defer stream.Close()

			// Cancel the upstream read when the request context
			// fires — bufio.Scanner has no ctx awareness, so we
			// close the underlying ReadCloser from a watcher
			// goroutine which interrupts the blocking Read.
			done := make(chan struct{})
			defer close(done)

			go func() {
				select {
				case <-ctx.Done():
					stream.Close()
				case <-done:
				}
			}()

			scanner := bufio.NewScanner(stream)
			// Allow long log lines (Docker can ship multi-KB lines
			// when an app dumps a JSON payload or a stacktrace).
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)

			prefix := []byte("[" + p.Name + "] ")

			for scanner.Scan() {
				if ctx.Err() != nil {
					return
				}

				line := scanner.Bytes()
				buf := make([]byte, 0, len(prefix)+len(line)+1)
				buf = append(buf, prefix...)
				buf = append(buf, line...)
				buf = append(buf, '\n')
				write(buf)
			}

			// Scanner errors (other than EOF) are surfaced inline
			// for the same reason per-pod open failures are.
			if err := scanner.Err(); err != nil && ctx.Err() == nil {
				write([]byte(fmt.Sprintf("[%s] [stream error] %s\n", p.Name, err)))
			}
		}()
	}

	wg.Wait()
}
