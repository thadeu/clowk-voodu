package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

// newRestartCmd is the imperative rolling-restart verb. Common
// after running migrations / config rotation / image rebuilds —
// the spec didn't drift, but the operator wants the running
// processes refreshed without a manifest edit.
//
//	vd restart clowk-lp/web
//	vd restart web                # auto-resolves scope when unambiguous
//
// Today only deployments are supported. Jobs and cronjobs are
// transient (re-trigger via vd run); plugin-managed kinds (database,
// ingress) don't fit the rolling-replace shape.
//
// Distinct from `vd apply` (no-op when spec hash unchanged) and
// `vd run X cmd` (one-off exec, no restart). `vd restart` is the
// only way to refresh a running deploy without changing its spec.
func newRestartCmd() *cobra.Command {
	var kindFlag string

	cmd := &cobra.Command{
		Use:   "restart <ref>",
		Short: "Rolling restart a deployment or statefulset without changing its manifest",
		Long: `Triggers a rolling restart of every replica of the named
resource. Each replica is replaced one at a time with a fresh
container, with a short pause between to keep the load balancer
healthy.

By default targets a deployment; pass -k/--kind=statefulset for a
statefulset (per-ordinal rolling replace, preserves pod-N identity).

Use this after:

  - vd run scope/name rails db:migrate     # so app sees the new schema
  - vd config set FOO=bar                  # already auto-restarts, but
                                             use this for a manual sweep
  - docker pull image                      # picking up rebuilt image tags
                                             without a config bump
  - any HCL change that didn't auto-restart # belt-and-braces refresh

The manifest is NOT modified; the next 'vd apply' is still
authoritative for desired state. Restart only affects the running
processes.

Examples:
  vd restart clowk-lp/web                            # deployment (default)
  vd restart web                                     # auto-resolve scope
  vd restart -k statefulset clowk-lp/redis           # statefulset
  vd restart --kind=statefulset data/postgres        # long form`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestart(cmd, args[0], kindFlag)
		},
	}

	cmd.Flags().StringVarP(&kindFlag, "kind", "k", "deployment",
		"resource kind to restart: deployment | statefulset")

	return cmd
}

func runRestart(cmd *cobra.Command, ref, kind string) error {
	scope, name := splitJobRef(ref)

	if name == "" {
		return fmt.Errorf("restart ref %q is empty or invalid", ref)
	}

	if kind == "" {
		kind = "deployment"
	}

	q := url.Values{}
	q.Set("kind", kind)
	q.Set("name", name)

	if scope != "" {
		q.Set("scope", scope)
	}

	resp, err := controllerDo(cmd.Root(), http.MethodPost, "/restart", q.Encode(), nil)
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

	fmt.Printf("%s/%s rolling restart complete\n", kind, ref)

	return nil
}
