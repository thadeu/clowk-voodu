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

func runLogs(cmd *cobra.Command, ref string, follow bool, tail int) error {
	ref = strings.TrimSpace(ref)

	if ref == "" {
		return fmt.Errorf("logs ref is empty")
	}

	containers, err := resolveLogsTargets(cmd, ref)
	if err != nil {
		return err
	}

	if len(containers) == 0 {
		return fmt.Errorf("no containers match %q", ref)
	}

	// Single container: copy verbatim, no prefix. The header banner on
	// stderr surfaces which container produced the stream so an operator
	// who passed a scope/name and got a single hit can still confirm.
	if len(containers) == 1 {
		return streamOneLog(cmd.Context(), cmd, containers[0], follow, tail, os.Stdout, "" /* no prefix */)
	}

	// Multi-container splits into two paths based on follow mode:
	//
	//   - Non-follow (historical dump): stream pods sequentially
	//     in the chronologically-sorted order from
	//     resolveLogsTargets. Oldest pod's logs print first,
	//     newest last. Operator reads top-to-bottom, latest output
	//     lands at the bottom near the cursor — no scroll-up
	//     needed. Concurrent fan-out would race and produce
	//     non-deterministic ordering between pods (lines from
	//     each pod still atomic, but pod-A's chunk could land
	//     before or after pod-B's depending on goroutine timing).
	//
	//   - Follow mode (-f): concurrent fan-out IS the right
	//     behaviour. Operator wants live updates from every pod
	//     interleaved as events happen; they don't care about
	//     "show pod-A's history fully before pod-B starts."
	//     Per-line prefix + locked writer guarantees lines stay
	//     attributable.
	fmt.Fprintf(os.Stderr, "==> tailing %d container(s) for %q\n", len(containers), ref)

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	if !follow {
		return streamSequentialLogs(ctx, cmd, containers, tail)
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	errs := make(chan error, len(containers))

	for _, name := range containers {
		wg.Add(1)

		go func(name string) {
			defer wg.Done()

			prefix := "[" + name + "] "
			writer := &lockedPrefixWriter{w: os.Stdout, mu: &mu, prefix: prefix}

			if err := streamOneLog(ctx, cmd, name, follow, tail, writer, prefix); err != nil {
				errs <- fmt.Errorf("%s: %w", name, err)
			}
		}(name)
	}

	wg.Wait()
	close(errs)

	// Collect non-cancellation errors. Cancellation (the parent ctx
	// firing) is the normal Ctrl-C exit and should not be surfaced as
	// a failure of the command.
	var firstErr error
	for e := range errs {
		if firstErr == nil && !errors.Is(e, context.Canceled) {
			firstErr = e
		}
	}

	return firstErr
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
func streamSequentialLogs(ctx context.Context, cmd *cobra.Command, containers []string, tail int) error {
	for _, name := range containers {
		fmt.Fprintf(os.Stderr, "==> %s <==\n", name)

		prefix := "[" + name + "] "
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

// fetchPodsList GETs /pods with the given filter and returns the
// matching Pod entries. Shared between logs and describe pod's
// scope/name resolution paths so any wire-shape change ripples to
// both at the same time.
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

	var env podsListResponse
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
