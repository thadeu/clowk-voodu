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

// newStopCmd builds `voodu stop <ref>` — the imperative pod-
// lifecycle verb that complements `vd apply` (declarative state)
// and `vd restart` (recreate-in-place). `vd stop` parks pods
// without removing them; the persistent volume + identity stay
// intact, the container just transitions to 'exited' state.
//
// Two ref shapes:
//
//	vd stop clowk-lp/redis             # all pods of a statefulset
//	vd stop clowk-lp/redis.0           # one specific pod
//
// Default behaviour is `--freeze`: the pod's ordinal is added to
// the persistent frozen-ordinals annotation in the controller's
// store, so subsequent reconciles (env-change, spec-drift, scale,
// failover) skip it. The pod stays parked until `vd start` clears
// the freeze.
//
// `--no-freeze` makes the stop transient: the next reconcile that
// touches the resource recreates the container. Useful for quick
// chaos tests where you want the controller's self-healing to
// kick in.
//
// Today only statefulsets are supported. Deployment replica IDs
// are regenerated on every spawn (hex), so per-replica freeze
// can't survive scale events; the CLI errors with a clear message
// if pointed at a deployment pod.
func newStopCmd() *cobra.Command {
	var (
		freezeFlag   bool
		noFreezeFlag bool
	)

	cmd := &cobra.Command{
		Use:   "stop <ref>",
		Short: "Stop one or all pods of a statefulset",
		Long: `Stops voodu-managed pods without removing them.

<ref> accepts two shapes:

  <scope>/<name>             every pod of the statefulset
  <scope>/<name>.<ordinal>   one specific pod

Default --freeze: the pod's ordinal is recorded in the controller's
persistent annotation, so subsequent reconciles (env-change,
spec-drift, scale, failover) skip it. The pod stays parked until
'vd start' clears the freeze.

--no-freeze: transient stop. The next reconcile that touches the
resource (apply / config_set / failover) recreates the container.

Today supported only for statefulsets. Deployment replica IDs are
regenerated on every spawn, so per-replica freeze can't survive
scale events.

Examples:
  vd stop clowk-lp/redis                  # stop all 3 pods, freeze each ordinal
  vd stop clowk-lp/redis.0                # stop just pod-0, freeze ordinal-0
  vd stop clowk-lp/redis.2 --no-freeze    # transient stop
  vd start clowk-lp/redis.0               # bring it back, clears freeze`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if freezeFlag && noFreezeFlag {
				return fmt.Errorf("pass either --freeze or --no-freeze, not both")
			}

			// Default = freeze. --no-freeze is the explicit opt-out.
			freeze := !noFreezeFlag

			return runStop(cmd, args[0], freeze)
		},
	}

	cmd.Flags().BoolVar(&freezeFlag, "freeze", false,
		"persist the stop across reconciles (default behaviour; flag is for clarity)")
	cmd.Flags().BoolVar(&noFreezeFlag, "no-freeze", false,
		"transient stop; the next reconcile recreates the container")

	return cmd
}

// runStop dispatches per-pod or per-resource based on the ref
// shape. The per-pod path translates directly to the container
// name; the per-resource path lists matching pods first, then
// stops each in turn.
func runStop(cmd *cobra.Command, ref string, freeze bool) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fmt.Errorf("stop ref is empty")
	}

	pods, err := resolveStopTargets(cmd, ref)
	if err != nil {
		return err
	}

	if len(pods) == 0 {
		return fmt.Errorf("no pods match %q", ref)
	}

	for _, name := range pods {
		if err := stopOnePod(cmd, name, freeze); err != nil {
			return fmt.Errorf("stop %s: %w", name, err)
		}

		if freeze {
			fmt.Printf("- %s stopped (frozen)\n", name)
		} else {
			fmt.Printf("- %s stopped\n", name)
		}
	}

	return nil
}

// resolveStopTargets translates the ref into a list of docker
// container names. Mirrors resolveLogsTargets's dispatch shape
// so users have one consistent mental model across commands.
//
//   - has '/' AND name part has '.' → per-pod ref → one container
//   - has '/'                       → resource ref → list all replicas
//
// Bare tokens (no slash) are rejected: stop-by-scope-only would be
// ambiguous (which kinds? which resources?), and the cobra layer's
// usage requires <ref>.
func resolveStopTargets(cmd *cobra.Command, ref string) ([]string, error) {
	if scope, name, replica, ok := splitReplicaRef(ref); ok {
		return []string{containers.ContainerName(scope, name, replica)}, nil
	}

	if !strings.Contains(ref, "/") {
		return nil, fmt.Errorf("stop ref must be <scope>/<name> or <scope>/<name>.<ordinal>; got %q", ref)
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

// stopOnePod POSTs to the per-pod stop endpoint. The freeze flag
// rides as a query param so the server's freeze-vs-transient
// branching stays a single decision point.
func stopOnePod(cmd *cobra.Command, name string, freeze bool) error {
	q := url.Values{}
	q.Set("freeze", boolStr(freeze))

	resp, err := controllerDo(cmd.Root(), http.MethodPost, "/pods/"+url.PathEscape(name)+"/stop", q.Encode(), nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var env struct {
			Error string `json:"error"`
		}

		if json.Unmarshal(raw, &env) == nil && env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return formatControllerError(resp.StatusCode, raw)
	}

	return nil
}

// boolStr renders a bool as the canonical "true"/"false" pair.
// Centralised so a future flag-rename doesn't drift the wire
// shape.
func boolStr(b bool) string {
	if b {
		return "true"
	}

	return "false"
}
