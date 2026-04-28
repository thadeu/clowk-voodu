package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"go.voodu.clowk.in/internal/controller"
)

// getPodsFlags holds the CLI knobs for `voodu get pods`. Filters map
// 1:1 to /pods query params, so adding a new filter is one line in
// each direction.
type getPodsFlags struct {
	kind  string
	scope string
	name  string
}

func newGetPodsCmd() *cobra.Command {
	var f getPodsFlags

	cmd := &cobra.Command{
		Use:   "pods",
		Short: "List voodu-managed containers running on this host",
		Long: `pods asks the controller for every container labeled createdby=voodu
on the host and prints them as a table grouped by scope.

Filters narrow the listing without re-reading docker on the client:
  --kind/-k    deployment | job | cronjob
  --scope/-s   only this scope
  --name/-n    only resources with this name

Pre-M0 containers (carrying createdby=voodu but no voodu.kind label)
appear at the bottom under '(legacy)' so the operator can see what
still needs a re-apply to migrate to structured labels.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGetPods(cmd, f)
		},
	}

	cmd.Flags().StringVarP(&f.kind, "kind", "k", "", "filter by kind (deployment, job, cronjob)")
	cmd.Flags().StringVarP(&f.scope, "scope", "s", "", "filter by scope")
	cmd.Flags().StringVarP(&f.name, "name", "n", "", "filter by resource name")

	return cmd
}

// podsResponse mirrors the /pods envelope. Kept local so the CLI
// only depends on the Pod struct's wire shape, not on internal
// controller types beyond that.
type podsResponse struct {
	Status string `json:"status"`
	Data   struct {
		Pods []controller.Pod `json:"pods"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

func runGetPods(cmd *cobra.Command, f getPodsFlags) error {
	root := cmd.Root()

	q := url.Values{}

	if f.kind != "" {
		q.Set("kind", f.kind)
	}

	if f.scope != "" {
		q.Set("scope", f.scope)
	}

	if f.name != "" {
		q.Set("name", f.name)
	}

	resp, err := controllerDo(root, http.MethodGet, "/pods", q.Encode(), nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return formatControllerError(resp.StatusCode, raw)
	}

	var env podsResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode pods: %w", err)
	}

	if env.Status == "error" {
		return fmt.Errorf("%s", env.Error)
	}

	switch outputFormat(root) {
	case "json":
		out := json.NewEncoder(os.Stdout)
		out.SetIndent("", "  ")

		return out.Encode(env.Data)

	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(env.Data)
	}

	return renderPodsTable(os.Stdout, env.Data.Pods)
}

// renderPodsTable prints a tab-aligned listing. We deliberately do
// NOT collapse-by-scope into headed groups: the table is more useful
// when every row is greppable on its own, and `awk '$3=="softphone"'`
// is the most common follow-up. Scope appears as its own column.
//
// "Legacy" pods (no voodu.kind label) render their kind column as
// "(legacy)" so they're visually distinct without needing a separate
// section.
func renderPodsTable(w io.Writer, pods []controller.Pod) error {
	if len(pods) == 0 {
		fmt.Fprintln(w, "No voodu-managed containers found.")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintln(tw, "NAME\tKIND\tSCOPE\tRESOURCE\tIMAGE\tSTATUS")

	for _, p := range pods {
		kind := p.Kind
		if kind == "" {
			kind = "(legacy)"
		}

		scope := p.Scope
		if scope == "" {
			scope = "-"
		}

		resource := p.ResourceName
		if resource == "" {
			// Legacy / pre-M0 containers lack voodu.name. Falling
			// back to the docker name keeps the row identifiable
			// without leaving an empty column the operator has to
			// squint at.
			resource = p.Name
		}

		status := p.Status
		if status == "" {
			if p.Running {
				status = "running"
			} else {
				status = "stopped"
			}
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Name, kind, scope, resource, p.Image, status,
		)
	}

	return tw.Flush()
}
