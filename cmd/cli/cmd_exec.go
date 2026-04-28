package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"go.voodu.clowk.in/internal/controller"
)

// newExecCmd builds `voodu exec <ref> -- CMD [ARGS...]` — kubectl
// exec semantics for voodu containers. Routes through
// /pods/{name}/exec, which hijacks the HTTP connection and bridges
// stdin/stdout/stderr to a `docker exec -it` running inside the
// chosen pod.
//
// Ref shape mirrors `vd get pd` / `vd logs`:
//
//	<scope>             requires --container to disambiguate
//	<scope>/<name>      auto-resolves when single replica; otherwise
//	                    requires --container
//	<container_name>    direct hit (the `.replicaID`-suffixed shape)
//
// Distinct from `vd run` (which spawns a fresh container from a
// declared resource) and from `vd run cronjob` (which forces a
// scheduled tick) — exec only enters something already alive.
func newExecCmd() *cobra.Command {
	var (
		container string
		tty       bool
		stdin     bool
		workdir   string
		user      string
		envs      []string
	)

	cmd := &cobra.Command{
		Use:   "exec <ref> [-- CMD ARGS...]",
		Short: "Run a command inside a running voodu container",
		Long: `Run a command inside a running voodu-managed container —
kubectl-exec semantics. The most common use case is opening an
interactive shell to debug a live deployment:

  voodu exec clowk-lp/web -- bash
  voodu exec clowk-lp-web.a3f9 -- bash
  voodu exec clowk-lp/web -it -- /bin/sh

<ref> accepts the same shapes as 'voodu get pd' / 'voodu logs':

  <scope>             ambiguous; requires --container
  <scope>/<name>      auto-picks the best replica (running > stopped,
                      then most recent). Pass --container to target a
                      specific one when needed.
  <container_name>    direct hit (e.g. clowk-lp-web.a3f9)

The command after '--' runs in the container's current working
directory and as the image's default user unless --workdir / --user
override it. Pass -i/--stdin to attach the local stdin and -t/--tty
to allocate a pty (defaults derived from the local terminal: -it
when stdin is a TTY, neither otherwise).

Distinct from 'voodu run' which spawns a brand-new container from
a declared manifest. Exec ALWAYS reuses an existing pod.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			command := args[1:]

			return runExec(cmd, ref, command, container, tty, stdin, workdir, user, envs)
		},
	}

	// `--` is consumed by cobra by default; we want everything after
	// it as positional. cobra's TraverseChildren=true on the root and
	// the cobra.MinimumNArgs(2) above already handle this.
	cmd.Flags().StringVar(&container, "container", "",
		"specific container name when ref matches multiple replicas")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false,
		"allocate a pty (default: true when stdin is a terminal)")
	cmd.Flags().BoolVarP(&stdin, "stdin", "i", false,
		"attach the local stdin (default: true when stdin is a terminal)")
	cmd.Flags().StringVarP(&workdir, "workdir", "w", "",
		"override the container's working directory")
	cmd.Flags().StringVarP(&user, "user", "u", "",
		"override the user the command runs as")
	cmd.Flags().StringSliceVarP(&envs, "env", "e", nil,
		"add KEY=VALUE env vars to the exec'd process (repeatable)")

	return cmd
}

func runExec(cmd *cobra.Command, ref string, command []string, containerOverride string, tty, stdinFlag bool, workdir, user string, envs []string) error {
	ref = strings.TrimSpace(ref)

	if ref == "" {
		return fmt.Errorf("exec ref is empty")
	}

	target, err := pickExecTarget(cmd, ref, containerOverride)
	if err != nil {
		return err
	}

	// Default --stdin and --tty from the local terminal: interactive
	// invocations (real shell) get -it automatically, piped/CI ones
	// (stdin is a pipe or file) get neither so the script's stdin
	// content reaches the remote process unmodified.
	//
	// Both auto-defaults can be overridden explicitly: `vd exec X
	// --tty=false -- bash` runs in non-TTY mode even from a real
	// shell, useful for scripted captures of an interactive command.
	if !cmd.Flags().Changed("stdin") {
		stdinFlag = term.IsTerminal(int(os.Stdin.Fd()))
	}

	if !cmd.Flags().Changed("tty") {
		tty = term.IsTerminal(int(os.Stdin.Fd()))
	}

	// In TTY mode, forward the operator's TERM and LANG so the
	// remote shell knows how to render. Without TERM, vim/htop/less
	// see the kernel's empty default, fall back to "dumb", and
	// keystrokes like arrow keys ship as literal escape bytes
	// (^[[A, ^[[B...) — visible in the buffer instead of moving the
	// cursor. systemd doesn't set TERM on the voodu-controller
	// daemon's environment, so docker exec inherits nothing useful.
	//
	// User-supplied -e flags win on conflict — operator choice
	// always trumps automatic enrichment.
	if tty {
		envs = autoPrependEnv(envs, "TERM", "xterm-256color")
		envs = autoPrependEnv(envs, "LANG", "")
		envs = autoPrependEnv(envs, "LC_ALL", "")
	}

	body := struct {
		Command []string `json:"command"`
		Env     []string `json:"env,omitempty"`
	}{
		Command: command,
		Env:     envs,
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode exec body: %w", err)
	}

	q := url.Values{}

	if tty {
		q.Set("tty", "true")

		// Snapshot the local terminal dimensions so the remote shell
		// starts at the right size. Sticky for the session — mid-run
		// resize (SIGWINCH) needs a wire protocol that doesn't exist
		// yet. Best-effort: if we can't read the size (stdin not a
		// real fd, redirect, etc.) we just skip and let the kernel
		// pick the pty's default.
		if cols, rows, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
			q.Set("cols", strconv.Itoa(cols))
			q.Set("rows", strconv.Itoa(rows))
		}
	}

	if stdinFlag {
		q.Set("interactive", "true")
	}

	if workdir != "" {
		q.Set("workdir", workdir)
	}

	if user != "" {
		q.Set("user", user)
	}

	root := cmd.Root()
	base := strings.TrimRight(controllerURL(root), "/")
	full := base + "/pods/" + url.PathEscape(target) + "/exec"

	if encoded := q.Encode(); encoded != "" {
		full += "?" + encoded
	}

	// Open a raw connection to the controller and write a manual HTTP
	// request — net/http abstracts away the hijacked-connection
	// pattern we need to bidirectionally stream stdin and stdout.
	parsed, err := url.Parse(full)
	if err != nil {
		return err
	}

	conn, err := dialControllerConn(parsed)
	if err != nil {
		return fmt.Errorf("connect controller: %w", err)
	}

	defer conn.Close()

	// Issue the POST manually so we can take ownership of the conn
	// after the response headers without going through Go's http
	// client (which insists on reading the body to completion before
	// freeing the conn).
	req := fmt.Sprintf(
		"POST %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: voodu-cli/%s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		parsed.RequestURI(), parsed.Host, version, len(bodyJSON),
	)

	if _, err := conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	if _, err := conn.Write(bodyJSON); err != nil {
		return fmt.Errorf("write body: %w", err)
	}

	br := bufio.NewReader(conn)

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)

		var env struct {
			Error string `json:"error"`
		}

		if json.Unmarshal(raw, &env) == nil && env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return formatControllerError(resp.StatusCode, raw)
	}

	// At this point, conn is the raw bidirectional channel. The
	// server-side handler hijacked its end and connected docker exec
	// to it (with a PTY when --tty). We bridge local stdin → conn
	// (when interactive) and conn → stdout (always).
	//
	// Raw mode: when TTY is on, the local kernel must NOT cook the
	// input — Ctrl-C, arrow keys, Tab need to reach the remote
	// shell as their literal byte sequences. MakeRaw flips the
	// terminal into character-at-a-time, no-echo mode. The remote
	// pty handles echo and tty discipline. We restore the previous
	// terminal state on exit so the operator's shell isn't left
	// stuck in raw mode if vd exec exits unexpectedly.
	if tty && term.IsTerminal(int(os.Stdin.Fd())) {
		oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err == nil {
			defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
		}
	}

	done := make(chan struct{}, 2)

	if stdinFlag {
		go func() {
			// Drain anything bufio.Reader prefetched before forwarding
			// — without this, characters typed before the response
			// arrived can be lost.
			_, _ = io.Copy(conn, os.Stdin)

			// Half-close the write side so the remote knows stdin is
			// done. Conn.CloseWrite is only on TCP/TLS — type-assert
			// best-effort.
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}

			done <- struct{}{}
		}()
	}

	go func() {
		// Pull from the buffered reader first (it may already hold
		// post-header bytes) then continue from the raw conn.
		_, _ = io.Copy(os.Stdout, br)
		done <- struct{}{}
	}()

	<-done

	return nil
}

// dialControllerConn opens the raw conn for the exec stream. Honours
// http vs https in the URL and skips http.Transport entirely (we need
// to keep ownership of the conn after the response headers).
func dialControllerConn(u *url.URL) (net.Conn, error) {
	switch u.Scheme {
	case "https":
		return tls.Dial("tcp", u.Host, &tls.Config{ServerName: hostnameOnly(u.Host)})
	default:
		return net.Dial("tcp", u.Host)
	}
}

// hostnameOnly strips the port from a host:port string, used for
// the TLS SNI value.
func hostnameOnly(hostport string) string {
	if i := strings.LastIndex(hostport, ":"); i > 0 {
		return hostport[:i]
	}

	return hostport
}

// autoPrependEnv inserts `KEY=<value-from-local-env-or-fallback>`
// into the env list IFF the operator hasn't already supplied that
// key via -e. Used to bring TERM / LANG / LC_ALL across when TTY is
// enabled, so the remote shell renders correctly without forcing
// the operator to remember `-e TERM=...` every invocation.
//
// fallback is used when the local env doesn't have KEY set. Empty
// fallback + empty local var → skip the variable entirely (don't
// pass `KEY=` to docker, that's ambiguous for some apps).
func autoPrependEnv(envs []string, key, fallback string) []string {
	prefix := key + "="

	// User-provided wins.
	for _, e := range envs {
		if strings.HasPrefix(e, prefix) {
			return envs
		}
	}

	value := os.Getenv(key)
	if value == "" {
		value = fallback
	}

	if value == "" {
		return envs
	}

	return append([]string{prefix + value}, envs...)
}

// pickExecTarget resolves the ref to a single container name. When
// containerOverride is non-empty it wins. Otherwise the same
// dispatch rule the rest of the pod-aware verbs use applies, with
// one ergonomic twist: scope/name matching multiple replicas
// auto-picks "the best" instead of erroring. Operators reach for
// `vd exec scope/name -- bash` to debug a deploy, not to pick a
// specific replica id from a list — pick automatically and print
// to stderr which one was chosen so they can re-target if needed.
//
// Selection rule when multiple replicas match:
//
//  1. Prefer running over stopped.
//  2. Among same status, prefer most-recently-created (replicas of
//     a healthy deploy are interchangeable; "newest first" matches
//     the operator's mental model of "freshly redeployed").
//
// Bare-scope refs that match multiple still error: a scope can hold
// deployment + jobs + cronjobs simultaneously, and picking across
// kinds without operator intent is too surprising.
func pickExecTarget(cmd *cobra.Command, ref, containerOverride string) (string, error) {
	if containerOverride != "" {
		return containerOverride, nil
	}

	if strings.Contains(ref, ".") && !strings.Contains(ref, "/") {
		return ref, nil
	}

	q := url.Values{}

	scopeNameRef := strings.Contains(ref, "/")

	if scopeNameRef {
		scope, name := splitJobRef(ref)
		if name == "" {
			return "", fmt.Errorf("ref %q: name is empty", ref)
		}

		if scope != "" {
			q.Set("scope", scope)
		}

		q.Set("name", name)
	} else {
		q.Set("scope", ref)
	}

	pods, err := fetchPodsList(cmd, q)
	if err != nil {
		return "", err
	}

	switch len(pods) {
	case 0:
		return "", fmt.Errorf("no containers match %q", ref)

	case 1:
		return pods[0].Name, nil
	}

	// Multi-match. Bare-scope refs span kinds and are too ambiguous
	// to resolve without operator intent — list candidates and ask
	// for --container.
	if !scopeNameRef {
		var b strings.Builder

		fmt.Fprintf(&b, "ref %q matches %d containers; pass --container to pick one:\n", ref, len(pods))

		for _, p := range pods {
			fmt.Fprintf(&b, "  %s\n", p.Name)
		}

		return "", fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
	}

	// scope/name match: same resource, different replicas.
	// Auto-pick by running first, then by CreatedAt desc.
	sorted := make([]controller.Pod, len(pods))
	copy(sorted, pods)

	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Running != sorted[j].Running {
			return sorted[i].Running
		}

		return sorted[i].CreatedAt > sorted[j].CreatedAt
	})

	chosen := sorted[0]

	status := "running"
	if !chosen.Running {
		status = "stopped"
	}

	// Terse banner. The previous wording was thorough but noisy
	// — operators reach for `vd exec scope/name` precisely because
	// they don't care which replica picks up; the one piece of
	// info worth surfacing is "this one, this status". --container
	// is documented in --help for the rare case where it matters.
	fmt.Fprintf(os.Stderr, "==> %s (%s)\n", chosen.Name, status)

	return chosen.Name, nil
}
