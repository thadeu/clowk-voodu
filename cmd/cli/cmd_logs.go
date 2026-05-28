package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/containers"
	"go.voodu.clowk.in/internal/controller"
)

// newLogsCmd builds `voodu logs <ref>` — the read-side counterpart
// to `voodu run job` and the recurring cronjob ticks. The CLI fans
// the request out to GET /pods/{name}/logs (one stream per matching
// container) and copies the streamed bodies straight to stdout, with
// per-line prefixes when more than one container is in the picture.
//
// Single-arg shape mirrors `voodu get pd`: <ref> is one of
//
//	<scope>             every container in this scope
//	<scope>/<name>      all replicas of one resource
//	<container_name>    a specific replica (e.g. clowk-lp-web.a3f9)
//
// Discriminator is the same as describe pod / get pd: slash → split,
// dot → container name, bare → scope filter. The kind argument the
// previous shape required is gone — `vd logs cronjob clowk-lp/foo`
// becomes `vd logs clowk-lp/foo`, with the controller no longer
// caring which kind produced the container.
func newLogsCmd() *cobra.Command {
	var (
		follow bool
		tail   int
	)

	cmd := &cobra.Command{
		Use:   "logs <ref>",
		Short: "Stream container logs by ref (scope, scope/name, or container name)",
		Long: `Stream stdout+stderr from voodu-managed containers.

<ref> accepts four shapes:

  <scope>                  every container in this scope, across kinds
  <scope>/<name>           all replicas of one resource (deployment /
                           statefulset / job / cronjob)
  <scope>/<name>.<replica> one specific replica — ordinal "0" for
                           statefulsets, hex id for deployments
  <container_name>         the full docker name (e.g. clowk-lp-web.a3f9)

When the ref matches more than one container, every line is prefixed
with the container name in brackets so it stays visually distinguishable
in the merged stream:

  [clowk-lp-web.a3f9] starting on :3000
  [clowk-lp-web.bb01] starting on :3000

Use --tail to cap each stream's history and --follow / -f to keep
them open. Job and cronjob run containers are kept around per their
successful_history_limit / failed_history_limit so 'voodu logs
clowk-lp/migrate' replays the most recent execution without re-running.

Examples:
  voodu logs clowk-lp                              every pod in scope
  voodu logs clowk-lp/web                          all replicas of web
  voodu logs clowk-lp/redis.0                      master pod of redis
  voodu logs clowk-lp/redis.1 -f                   follow replica 1
  voodu logs clowk-lp/web --tail 100               last 100 lines each
  voodu logs clowk-lp-web.a3f9                     one specific replica`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd, args[0], follow, tail)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new lines as they arrive")
	cmd.Flags().IntVarP(&tail, "tail", "t", 0, "limit output to the last N lines per container (0 = all)")

	return cmd
}

// runLogs — dispatch based on ref shape:
//
//   - per-replica  (scope/name.replica)  → /pods/{name}/logs  (single, no prefix)
//   - container    (name.replica)        → /pods/{name}/logs  (single, no prefix)
//   - scope/name   (multi-replica)       → /logs?scope=&name=&follow=&tail=
//   - bare scope                         → /logs?scope=&follow=&tail=
//
// The multi-shape paths use the server-side multiplexed endpoint
// (handleLogsMulti): one HTTP round-trip, server fans out across
// every matching pod, prefixes each line with `[pod-name] `, the
// CLI colourises per pod and prints.
//
// Historical note: the CLI used to fan-out N HTTP streams client-
// side (one per matching pod). That logic moved to the server so
// the WebUI, the CLI, and any future Go consumer share one
// implementation of "tail N pods, attributed per line."
func runLogs(cmd *cobra.Command, ref string, follow bool, tail int) error {
	ref = strings.TrimSpace(ref)

	if ref == "" {
		return fmt.Errorf("logs ref is empty")
	}

	// Per-replica → exactly one container, name is deterministic.
	if scope, name, replica, ok := splitReplicaRef(ref); ok {
		return streamOneLog(cmd.Context(), cmd, containers.ContainerName(scope, name, replica), follow, tail, os.Stdout, "" /* no prefix */)
	}

	// Bare container name with a dot — already a docker name.
	if strings.Contains(ref, ".") && !strings.Contains(ref, "/") {
		return streamOneLog(cmd.Context(), cmd, ref, follow, tail, os.Stdout, "" /* no prefix */)
	}

	// Multi-shape (scope/name OR bare scope). Server multiplexes.
	q := url.Values{}

	if strings.Contains(ref, "/") {
		scope, name := splitJobRef(ref)
		if name == "" {
			return fmt.Errorf("ref %q: name is empty", ref)
		}

		if scope != "" {
			q.Set("scope", scope)
		}

		q.Set("name", name)
	} else {
		q.Set("scope", ref)
	}

	if follow {
		q.Set("follow", "true")
	}

	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}

	return streamMultiLogs(cmd, ref, q)
}

// streamMultiLogs opens GET /logs?... and copies the multiplexed
// body into stdout, parsing the `[pod-name] ` prefix the server
// emits to colourise the pod marker per-line. The colour assignment
// is deterministic (hash of name → palette slot), so a given pod's
// lines stay the same colour across runs and modes.
//
// X-Voodu-Containers header is read up-front to print the
// "==> tailing N container(s) for <ref>" banner on stderr, matching
// the UX of the old client-side fan-out.
func streamMultiLogs(cmd *cobra.Command, ref string, q url.Values) error {
	ctx := cmd.Context()

	// `controllerStream` (no overall request timeout) instead of
	// `controllerDo` (30s budget) — otherwise `?follow=true` is killed
	// at exactly 30s with `context deadline exceeded` and the operator
	// sees the tail drop mid-line. Connect + response-header timeouts
	// still apply, so an unreachable controller surfaces fast.
	resp, err := controllerStream(ctx, cmd.Root(), http.MethodGet, "/logs", q.Encode(), nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return formatControllerError(resp.StatusCode, raw)
	}

	matches := strings.Split(resp.Header.Get("X-Voodu-Containers"), ",")
	// Strip the empty-string artifact Split("") returns.
	if len(matches) == 1 && matches[0] == "" {
		matches = nil
	}

	if len(matches) == 0 {
		return fmt.Errorf("no containers match %q", ref)
	}

	fmt.Fprintf(os.Stderr, "==> tailing %d container(s) for %q\n", len(matches), ref)

	palette := newPodPalette(os.Stdout)
	stdout := bufio.NewWriter(os.Stdout)
	defer stdout.Flush()

	// Read line-by-line so we can colourise the per-line `[pod] `
	// prefix the server emits without buffering the whole stream.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil
		}

		line := scanner.Text()
		if pod, rest, ok := stripPodPrefix(line); ok {
			fmt.Fprintf(stdout, "%s %s\n", palette.ColorFor(pod)("["+pod+"]"), rest)
		} else {
			// Server is supposed to always prefix; surface unprefixed
			// lines verbatim rather than swallowing them.
			fmt.Fprintln(stdout, line)
		}

		// Flush after each line so `-f` feels live in the terminal —
		// the bufio.Writer would otherwise hold lines until its
		// internal buffer fills.
		stdout.Flush()
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	return nil
}

// stripPodPrefix peels `[pod-name] ` off the start of a multiplexed
// line. Returns (pod, rest, true) on match, (line, "", false)
// otherwise — keeping the call site's branch small.
func stripPodPrefix(line string) (pod, rest string, ok bool) {
	if !strings.HasPrefix(line, "[") {
		return line, "", false
	}

	end := strings.Index(line, "] ")
	if end < 0 {
		return line, "", false
	}

	return line[1:end], line[end+2:], true
}

// streamSequentialLogs walks the (already chronologically-sorted)
// container list and prints each pod's logs in full before moving
// to the next. Used by the non-follow path so output reflects the
// sort order from resolveLogsTargets — oldest pod's logs appear
// at the top, newest at the bottom, no race with concurrent
// goroutines.
//
// Each pod's section is bracketed by a header on stderr so the
// operator can tell where one pod's output ends and the next
// begins. stdout stays clean (just the log lines, with the
// container's `[name] ` prefix on each).
//
// Header AND prefix share the pod's hash-derived color — operator
// scanning the output associates pod ↔ color visually without
// reading the full container suffix.
func streamSequentialLogs(ctx context.Context, cmd *cobra.Command, containers []string, tail int) error {
	headerPalette := newPodPalette(os.Stderr)
	prefixPalette := newPodPalette(os.Stdout)

	for _, name := range containers {
		fmt.Fprintln(os.Stderr, headerPalette.ColorFor(name)("==> "+name+" <=="))

		prefix := prefixPalette.ColorFor(name)("[" + name + "]") + " "
		writer := &lockedPrefixWriter{w: os.Stdout, mu: &sync.Mutex{}, prefix: prefix}

		// follow=false here because this path only runs in the
		// non-follow branch — stream is bounded by EOF on the
		// HTTP body.
		if err := streamOneLog(ctx, cmd, name, false, tail, writer, prefix); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}

			return fmt.Errorf("%s: %w", name, err)
		}
	}

	return nil
}

// resolveLogsTargets translates the ref into a list of docker container
// names. Four shapes:
//
//   - has '/' AND the name part has '.'  → per-replica ref, e.g.
//                  clowk-lp/redis.0 → container clowk-lp-redis.0 (1 elem)
//   - has '.'  → already a full container name, return as-is (1 elem)
//   - has '/'  → splitJobRef → /pods?scope=&name= (every replica)
//   - bare     → /pods?scope= (everything in scope)
//
// Mirrors the dispatch in runDescribePod so users get one consistent
// mental model across get pd / describe pod / logs.
func resolveLogsTargets(cmd *cobra.Command, ref string) ([]string, error) {
	// Per-replica shape — `<scope>/<name>.<replica>`. Translates
	// directly to the container name without a controller round-trip:
	// the docker name is deterministic, so we don't need /pods to
	// confirm existence here. If the operator typo'd the ordinal,
	// streamOneLog surfaces the controller's "container not found"
	// instead — same error path as a bad full container name.
	if scope, name, replica, ok := splitReplicaRef(ref); ok {
		return []string{containers.ContainerName(scope, name, replica)}, nil
	}

	if strings.Contains(ref, ".") && !strings.Contains(ref, "/") {
		return []string{ref}, nil
	}

	q := url.Values{}

	if strings.Contains(ref, "/") {
		scope, name := splitJobRef(ref)
		if name == "" {
			return nil, fmt.Errorf("ref %q: name is empty", ref)
		}

		if scope != "" {
			q.Set("scope", scope)
		}

		q.Set("name", name)
	} else {
		// Bare token → scope filter
		q.Set("scope", ref)
	}

	pods, err := fetchPodsList(cmd, q)
	if err != nil {
		return nil, err
	}

	// Sort by CreatedAt ascending — oldest first, newest last.
	// Operators read top-to-bottom in a terminal; chronological
	// order means the most recent (and usually most relevant)
	// output lands at the bottom near the cursor when streaming
	// finishes, no scroll-up required.
	sort.SliceStable(pods, func(i, j int) bool {
		return pods[i].CreatedAt < pods[j].CreatedAt
	})

	out := make([]string, 0, len(pods))
	for _, p := range pods {
		out = append(out, p.Name)
	}

	return out, nil
}

// fetchPodsList GETs /pods (compact) with the given filter and
// returns the matching Pod entries. Logs resolution only needs the
// container Name field — no point paying for the rich /pods?detail=true
// shape that describe uses. The local response struct decouples
// this call from podsListResponse in cmd_describe.go (which now
// decodes []PodDetail for its enriched payload).
func fetchPodsList(cmd *cobra.Command, q url.Values) ([]controller.Pod, error) {
	root := cmd.Root()

	resp, err := controllerDo(root, http.MethodGet, "/pods", q.Encode(), nil)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, formatControllerError(resp.StatusCode, raw)
	}

	var env struct {
		Status string `json:"status"`
		Data   struct {
			Pods []controller.Pod `json:"pods"`
		} `json:"data"`
		Error string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode pods list: %w", err)
	}

	if env.Status == "error" {
		return nil, fmt.Errorf("%s", env.Error)
	}

	return env.Data.Pods, nil
}

// streamOneLog opens GET /pods/{name}/logs and copies the body into
// `out`. When prefix is non-empty, each line gets the prefix prepended;
// when empty, the body is copied verbatim. The streaming is bounded
// by the supplied context so multi-pod fan-out can cancel siblings on
// the first failure / Ctrl-C.
func streamOneLog(ctx context.Context, cmd *cobra.Command, name string, follow bool, tail int, out io.Writer, prefix string) error {
	root := cmd.Root()

	q := url.Values{}

	if follow {
		q.Set("follow", "true")
	}

	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}

	base := strings.TrimRight(controllerURL(root), "/")
	full := base + "/pods/" + url.PathEscape(name) + "/logs"

	if encoded := q.Encode(); encoded != "" {
		full += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", fmt.Sprintf("voodu-cli/%s", version))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("controller GET /pods/%s/logs: %w", name, err)
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)

		var env struct {
			Error string `json:"error"`
		}

		if json.Unmarshal(raw, &env) == nil && env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return formatControllerError(resp.StatusCode, raw)
	}

	if prefix == "" {
		_, err := io.Copy(out, resp.Body)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}

		return nil
	}

	// Line-buffered copy so the prefix lands once per line, not once
	// per read chunk. bufio.Scanner with default buf would truncate
	// at 64 KiB lines — bump to 1 MiB which is generous enough for any
	// reasonable container log line.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		if _, err := fmt.Fprintf(out, "%s\n", scanner.Text()); err != nil {
			return err
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	return nil
}

// lockedPrefixWriter serializes Write calls so multi-goroutine
// streaming can write to the same os.Stdout without interleaving
// mid-line. The prefix is added to every Write — assumes the caller
// already buffered to line granularity (streamOneLog uses Scanner's
// Text which strips the trailing newline; we re-add it on Fprintf).
type lockedPrefixWriter struct {
	w      io.Writer
	mu     *sync.Mutex
	prefix string
}

func (l *lockedPrefixWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Compose prefix + payload in one Write so no other writer can
	// sneak between them under the mutex (the mutex protects against
	// other goroutines, but a partial Write underneath could still
	// fragment if the underlying writer isn't atomic).
	buf := make([]byte, 0, len(l.prefix)+len(p))
	buf = append(buf, l.prefix...)
	buf = append(buf, p...)

	n, err := l.w.Write(buf)

	// Report bytes consumed from the caller's payload, not including
	// the prefix bytes we added.
	if n > len(l.prefix) {
		return n - len(l.prefix), err
	}

	return 0, err
}
