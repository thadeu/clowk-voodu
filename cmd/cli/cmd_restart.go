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
//	vd restart clowk-lp/web                # deployment auto-detected
//	vd restart clowk-lp/redis              # statefulset auto-detected
//	vd restart web                         # auto-resolves scope too
//
// Kind is inferred from the controller — the operator references
// the resource by ref, the server looks up which kind has a
// manifest at that ref. Pass `-k <kind>` only when both a
// deployment and a statefulset exist under the same scope/name
// (rare; the server returns 400 with the matches listed).
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

Kind is auto-detected from the controller — operator points at
<scope>/<name> and the server figures out whether the manifest
is a deployment or a statefulset. Pass -k/--kind only to
disambiguate when both exist under the same name (rare).

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
  vd restart clowk-lp/web                            # auto-detects deployment
  vd restart clowk-lp/redis                          # auto-detects statefulset
  vd restart web                                     # auto-resolves scope too
  vd restart -k deployment clowk-lp/api              # explicit kind (only on collision)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestart(cmd, args[0], kindFlag)
		},
	}

	cmd.Flags().StringVarP(&kindFlag, "kind", "k", "",
		"resource kind to restart (default: auto-detect from controller). Pass deployment or statefulset to disambiguate.")

	return cmd
}

func runRestart(cmd *cobra.Command, ref, kind string) error {
	scope, name := splitJobRef(ref)

	if name == "" {
		return fmt.Errorf("restart ref %q is empty or invalid", ref)
	}

	q := url.Values{}
	q.Set("name", name)

	if kind != "" {
		q.Set("kind", kind)
	}

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
		var errEnv struct {
			Error string `json:"error"`
		}

		if json.Unmarshal(raw, &errEnv) == nil && errEnv.Error != "" {
			return fmt.Errorf("%s", errEnv.Error)
		}

		return formatControllerError(resp.StatusCode, raw)
	}

	// Pull the resolved kind from the response so the success
	// line names what actually got restarted (the server may
	// have auto-detected statefulset when the operator didn't
	// pass -k). Falls back to the operator-supplied kind on
	// decode failure.
	var env struct {
		Data struct {
			Kind  string `json:"kind"`
			Scope string `json:"scope"`
			Name  string `json:"name"`
		} `json:"data"`
	}

	resolvedKind := kind
	if json.Unmarshal(raw, &env) == nil && env.Data.Kind != "" {
		resolvedKind = env.Data.Kind
	}

	fmt.Printf("%s/%s rolling restart complete\n", resolvedKind, ref)

	return nil
}
