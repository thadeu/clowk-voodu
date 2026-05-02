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
		Short: "Recreate one or all pods of a resource (clears any freeze)",
		Long: `Brings stopped pods back online with FRESH config from the
latest env file. Implementation is recreate-via-reconcile:

  1. The freeze annotation is cleared (if any).
  2. The stale stopped container is removed.
  3. The manifest is re-fired through the reconciler, which spawns
     a brand-new container that re-reads the env file at run time.

This is NOT plain 'docker start' on the existing container —
that would skip the env-file re-read, leaving the pod with the
stale env vars from its original 'docker run'. Critical when
something changed during the freeze window (e.g.,
REDIS_MASTER_ORDINAL flipped via 'vd redis:failover'), so the
pod boots with current state instead of pre-freeze state.

<ref> accepts two shapes:

  <scope>/<name>             every pod of the resource
  <scope>/<name>.<replica>   one specific pod (ordinal for
                             statefulset, hex id for deployment)

Trade-off: the original container is destroyed on start, so any
pre-stop logs in that container are gone. Pull them with
'vd logs --tail N' BEFORE 'vd start' if you need them. Volumes
(statefulset) survive — data is preserved.

For deployments, the recreated pod gets a NEW hex replica ID
(the old one disappears with the destroyed container). For
statefulsets, the new pod takes the same ordinal.

Errors with 410 Gone when the manifest no longer exists in the
controller — operator must 'vd apply' first to spawn from scratch.

Examples:
  vd start clowk-lp/redis              # all pods, clear all freezes
  vd start clowk-lp/redis.0            # just pod-0
  vd start clowk-lp/web.a3f9           # specific deployment replica`,
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
		recreated, unfroze, err := startOnePod(cmd, name)
		if err != nil {
			return fmt.Errorf("start %s: %w", name, err)
		}

		switch {
		case recreated && unfroze:
			fmt.Printf("+ %s recreated (unfrozen, fresh env from manifest)\n", name)
		case recreated:
			fmt.Printf("+ %s recreated (fresh env from manifest)\n", name)
		case unfroze:
			fmt.Printf("+ %s started (unfrozen)\n", name)
		default:
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
// (recreated, unfroze) so the caller can render the right
// status line: "recreated" when the managed-pod path fired
// (Remove + reconcile), "unfrozen" when a freeze annotation
// was cleared, both / either / neither in any combination.
func startOnePod(cmd *cobra.Command, name string) (recreated, unfroze bool, err error) {
	resp, err := controllerDo(cmd.Root(), http.MethodPost, "/pods/"+url.PathEscape(name)+"/start", "", nil)
	if err != nil {
		return false, false, err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var errEnv struct {
			Error string `json:"error"`
		}

		if json.Unmarshal(raw, &errEnv) == nil && errEnv.Error != "" {
			return false, false, fmt.Errorf("%s", errEnv.Error)
		}

		return false, false, formatControllerError(resp.StatusCode, raw)
	}

	var env struct {
		Status string `json:"status"`
		Data   struct {
			Recreated bool `json:"recreated"`
			Unfroze   bool `json:"unfroze"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		// Server returned 200 but the body's not what we expect —
		// don't fail the command, just lose the signal flags.
		return false, false, nil
	}

	return env.Data.Recreated, env.Data.Unfroze, nil
}
