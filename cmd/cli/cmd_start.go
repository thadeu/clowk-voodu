package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/containers"
)

// newStartCmd builds `voodu start <ref>` — the counterpart to
// `vd stop`. Brings parked pods back online and clears the
// frozen-ordinals annotation so subsequent reconciles include
// them again.
//
//	vd start clowk-lp/redis           # all pods of a statefulset
//	vd start clowk-lp/redis.0         # one specific pod
//
// Idempotent: pods that are already running and ordinals that
// were never frozen both succeed silently. The error path is
// the missing-container case (use `vd apply` to create from
// scratch).
func newStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start <ref>",
		Short: "Start one or all pods of a statefulset (clears any freeze)",
		Long: `Brings stopped pods back online and clears the persistent
frozen-ordinals annotation so the controller's reconciler includes
them again on every subsequent apply / config_set / failover.

<ref> accepts two shapes:

  <scope>/<name>             every pod of the statefulset
  <scope>/<name>.<ordinal>   one specific pod

Idempotent — already-running and never-frozen pods both succeed.
Errors when the container doesn't exist on the host (use 'vd apply'
to spawn from scratch).

Examples:
  vd start clowk-lp/redis              # start every pod, clear all freezes
  vd start clowk-lp/redis.0            # just pod-0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd, args[0])
		},
	}

	return cmd
}

func runStart(cmd *cobra.Command, ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fmt.Errorf("start ref is empty")
	}

	pods, err := resolveStartTargets(cmd, ref)
	if err != nil {
		return err
	}

	if len(pods) == 0 {
		return fmt.Errorf("no pods match %q", ref)
	}

	for _, name := range pods {
		unfroze, err := startOnePod(cmd, name)
		if err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}

		if unfroze {
			fmt.Printf("+ %s started (unfrozen)\n", name)
		} else {
			fmt.Printf("+ %s started\n", name)
		}
	}

	return nil
}

// resolveStartTargets mirrors resolveStopTargets — same dispatch
// rules so `vd stop X && vd start X` work symmetrically against
// any ref the operator typed.
func resolveStartTargets(cmd *cobra.Command, ref string) ([]string, error) {
	if scope, name, replica, ok := splitReplicaRef(ref); ok {
		return []string{containers.ContainerName(scope, name, replica)}, nil
	}

	if !strings.Contains(ref, "/") {
		return nil, fmt.Errorf("start ref must be <scope>/<name> or <scope>/<name>.<ordinal>; got %q", ref)
	}

	scope, name := splitJobRef(ref)
	if name == "" {
		return nil, fmt.Errorf("ref %q: name is empty", ref)
	}

	q := url.Values{}
	q.Set("scope", scope)
	q.Set("name", name)

	pods, err := fetchPodsList(cmd, q)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(pods))
	for _, p := range pods {
		out = append(out, p.Name)
	}

	return out, nil
}

// startOnePod POSTs to the per-pod start endpoint. Returns
// whether the server cleared a freeze annotation — used by the
// caller to render a slightly different status line for the
// frozen-pod-revived case.
func startOnePod(cmd *cobra.Command, name string) (bool, error) {
	resp, err := controllerDo(cmd.Root(), http.MethodPost, "/pods/"+url.PathEscape(name)+"/start", "", nil)
	if err != nil {
		return false, err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var env struct {
			Error string `json:"error"`
		}

		if json.Unmarshal(raw, &env) == nil && env.Error != "" {
			return false, fmt.Errorf("%s", env.Error)
		}

		return false, formatControllerError(resp.StatusCode, raw)
	}

	var env struct {
		Status string `json:"status"`
		Data   struct {
			Unfroze bool `json:"unfroze"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		// Server returned 200 but the body's not what we expect —
		// don't fail the command, just lose the unfroze signal.
		return false, nil
	}

	return env.Data.Unfroze, nil
}
