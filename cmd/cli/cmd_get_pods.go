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

// renderPodsTable groups pods by voodu.role label and prints each
// group as its own section with a divider header. The intent is
// to keep the long-running services (deployment, statefulset)
// from being drowned out by the dozens of historical job/cronjob
// runs that accumulate over time — operators usually scan the
// listing for "what's running RIGHT NOW for app X" and the
// section split makes that 3x faster.
//
// Within a section, pods are tab-aligned and sorted as the
// controller delivered them (sortPods on the server side already
// orders by scope/name/replica).
//
// Pods missing a role label fall into the "(legacy)" section so
// pre-M0 containers stay visible without needing a re-apply.
func renderPodsTable(w io.Writer, pods []controller.Pod) error {
	if len(pods) == 0 {
		fmt.Fprintln(w, "No voodu-managed containers found.")
		return nil
	}

	groups := groupPodsByRole(pods)

	first := true

	for _, role := range groups.order {
		bucket := groups.byRole[role]

		if !first {
			fmt.Fprintln(w)
		}

		first = false

		// Section header — operator sees "=== role (count) ===".
		// Count is useful when one section blows up (e.g. 50
		// backup jobs accumulated) so the operator knows to prune
		// without reading every row.
		fmt.Fprintf(w, "=== %s (%d) ===\n", role, len(bucket))

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

		fmt.Fprintln(tw, "NAME\tKIND\tSCOPE\tRESOURCE\tIMAGE\tSTATUS")

		for _, p := range bucket {
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

		if err := tw.Flush(); err != nil {
			return err
		}
	}

	return nil
}

// roleGroups holds the bucketed-by-role pod listing plus a stable
// section order so renderPodsTable produces the same output across
// runs (rather than map-iteration random).
type roleGroups struct {
	byRole map[string][]controller.Pod
	order  []string
}

// orphansBucket is the section name used for pods that lack a
// voodu.role label entirely. Pre-M0 containers (from before
// BuildLabels emitted role) and any hand-rolled docker container
// without our labels end up here, regardless of their Kind.
//
// Rendered last so the structured-role sections stay scannable —
// orphans are an "archaeology / things to migrate" view, not the
// primary lens.
const orphansBucket = "orphans"

// groupPodsByRole buckets pods by voodu.role with two rules:
//
//  1. Pods with a non-empty Role land in `byRole[role]`.
//  2. Pods missing the role label fall into a single
//     `orphans` bucket regardless of Kind — visible at the
//     bottom so the operator can spot "what still needs an
//     apply to inherit the new labels".
//
// Within the structured sections, ordering is "natural priority":
// long-running services first (deployment, statefulset), then
// scheduled work (cronjob), then completed runs (job, release,
// backup), then anything else alphabetically. Orphans always last.
//
// The priority is opinionated — operators primarily care about
// the live services. Putting them on top keeps `vd get pd`
// scannable even when 50 backup jobs accumulate.
func groupPodsByRole(pods []controller.Pod) roleGroups {
	by := map[string][]controller.Pod{}

	for _, p := range pods {
		role := p.Role
		if role == "" {
			role = orphansBucket
		}

		by[role] = append(by[role], p)
	}

	// Priority for known roles, lower index = printed first.
	// orphans always last (well past anything else).
	priority := map[string]int{
		"deployment":   10,
		"statefulset":  20,
		"cronjob":      30,
		"job":          40,
		"release":      50,
		"backup":       60,
		// Everything else falls to 100 (alphabetical within).
		orphansBucket: 999,
	}

	roles := make([]string, 0, len(by))
	for role := range by {
		roles = append(roles, role)
	}

	// Sort by priority, then alphabetically for ties / unknowns.
	sortStrings(roles, func(a, b string) bool {
		pa, ok := priority[a]
		if !ok {
			pa = 100
		}

		pb, ok := priority[b]
		if !ok {
			pb = 100
		}

		if pa != pb {
			return pa < pb
		}

		return a < b
	})

	return roleGroups{byRole: by, order: roles}
}

// sortStrings is a tiny shim to keep the priority sort readable
// without dragging in the standard sort import name juggling here.
func sortStrings(s []string, less func(a, b string) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
