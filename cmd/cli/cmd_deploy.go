// cmd_deploy.go is the `vd deploy trigger *` surface — how an operator
// authorises a repository to deploy to this box.
//
//	vd deploy trigger create --repo owner/name --branch main --allow-scope runa
//	vd deploy trigger list
//	vd deploy trigger show <id>
//	vd deploy trigger enable|disable <id>
//	vd deploy trigger delete <id>
//
// NOT in localOnlyCommands, unlike `vd pat`: a trigger belongs to the BOX, so
// running this from a laptop with a remote configured should authorise the
// remote. Having SSH access to the machine is what makes the statement the
// operator's to make.
//
// WHAT THIS DOES NOT CONFIGURE, and the omission is the design: which branches
// and tags fire, which paths are watched, and which manifest gets applied all
// live in `.voodu/**/*.yml` inside the repository. This command is the trust
// statement — the repository, the branch a commit must descend from, and the
// scopes it may touch. It changes almost never; the file changes with the code.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type triggerPayload struct {
	Repo        string   `json:"repo"`
	Branch      string   `json:"branch"`
	AllowScopes []string `json:"allow_scopes"`
	Enabled     *bool    `json:"enabled,omitempty"`
}

type triggerRecord struct {
	ID          string   `json:"id"`
	Repo        string   `json:"repo"`
	Branch      string   `json:"branch"`
	AllowScopes []string `json:"allow_scopes"`
	Enabled     bool     `json:"enabled"`
	CreatedAt   string   `json:"created_at"`
	LastFiredAt string   `json:"last_fired_at,omitempty"`
}

type triggerEnvelope struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func newDeployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Authorise repositories to deploy to this box",
	}

	cmd.AddCommand(newDeployTriggerCmd())

	return cmd
}

func newDeployTriggerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trigger",
		Short: "Manage deploy triggers",
		Long: `A trigger is a statement of trust: this repository, on this branch, may
apply to these scopes on this box.

It is NOT the deploy configuration. Which branches and tags fire, which
paths are watched and which manifest is applied live in .voodu/**/*.yml
inside the repository — so they travel with the code, get reviewed in a
pull request, and cannot be changed by anything that is not a commit on
the branch you pin here.`,
	}

	cmd.AddCommand(
		newTriggerCreateCmd(),
		newTriggerListCmd(),
		newTriggerShowCmd(),
		newTriggerToggleCmd("enable", true),
		newTriggerToggleCmd("disable", false),
		newTriggerDeleteCmd(),
	)

	return cmd
}

func newTriggerCreateCmd() *cobra.Command {
	var (
		repo   string
		branch string
		scopes []string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Authorise a repository",
		Example: `  vd deploy trigger create --repo acme/web --branch main --allow-scope runa
  vd deploy trigger create --repo acme/web --branch main --allow-scope runa --allow-scope staging`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			payload := triggerPayload{Repo: repo, Branch: branch, AllowScopes: scopes}

			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}

			record, err := triggerRequest(cmd, http.MethodPost, "/deploy/triggers", bytes.NewReader(body))
			if err != nil {
				return err
			}

			fmt.Printf("Trigger %s created\n", record.ID)
			fmt.Printf("  %s @ %s → scopes %s\n", record.Repo, record.Branch, strings.Join(record.AllowScopes, ", "))
			fmt.Println()
			fmt.Printf("Next: commit a %s/*.yml in that repository declaring when to deploy.\n", ".voodu")

			return nil
		},
	}

	cmd.Flags().StringVar(&repo, "repo", "", "repository as owner/name (required)")
	cmd.Flags().StringVar(&branch, "branch", "", "the branch a deployed commit must descend from (required)")
	// Repeatable rather than comma-separated: a scope is a single value, and
	// `--allow-scope a,b` silently creating a scope literally named "a,b" is
	// the kind of thing nobody notices until a deploy is refused.
	cmd.Flags().StringArrayVar(&scopes, "allow-scope", nil, "a scope this repository may apply to (repeatable, required)")

	return cmd
}

func newTriggerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List authorised repositories",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := controllerDo(cmd.Root(), http.MethodGet, "/deploy/triggers", "", nil)
			if err != nil {
				return err
			}

			defer resp.Body.Close()

			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}

			if resp.StatusCode >= 400 {
				return triggerError(resp.StatusCode, raw)
			}

			var env triggerEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}

			var data struct {
				Triggers []triggerRecord `json:"triggers"`
			}

			if err := json.Unmarshal(env.Data, &data); err != nil {
				return fmt.Errorf("decode triggers: %w", err)
			}

			if len(data.Triggers) == 0 {
				fmt.Println("No deploy triggers on this box.")
				fmt.Println("Authorise one with: vd deploy trigger create --repo owner/name --branch main --allow-scope <scope>")

				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tREPO\tBRANCH\tSCOPES\tSTATUS\tLAST FIRED")

			for _, t := range data.Triggers {
				status := "enabled"
				if !t.Enabled {
					status = "paused"
				}

				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					t.ID, t.Repo, t.Branch, strings.Join(t.AllowScopes, ","), status, humanTime(t.LastFiredAt))
			}

			return w.Flush()
		},
	}
}

func newTriggerShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one trigger",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			record, err := triggerRequest(cmd, http.MethodGet, "/deploy/triggers/"+args[0], nil)
			if err != nil {
				return err
			}

			fmt.Printf("ID          %s\n", record.ID)
			fmt.Printf("Repository  %s\n", record.Repo)
			fmt.Printf("Branch      %s\n", record.Branch)
			fmt.Printf("Scopes      %s\n", strings.Join(record.AllowScopes, ", "))
			fmt.Printf("Status      %s\n", map[bool]string{true: "enabled", false: "paused"}[record.Enabled])
			fmt.Printf("Created     %s\n", humanTime(record.CreatedAt))
			fmt.Printf("Last fired  %s\n", humanTime(record.LastFiredAt))

			return nil
		},
	}
}

// newTriggerToggleCmd builds enable and disable from one body, because they
// are the same request with one field flipped — and because a paused trigger
// keeps its authorisation, which is the whole reason pausing exists separately
// from deleting.
func newTriggerToggleCmd(verb string, enabled bool) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <id>",
		Short: map[bool]string{true: "Resume a paused trigger", false: "Pause a trigger without withdrawing trust"}[enabled],
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			current, err := triggerRequest(cmd, http.MethodGet, "/deploy/triggers/"+args[0], nil)
			if err != nil {
				return err
			}

			body, err := json.Marshal(triggerPayload{
				Repo:        current.Repo,
				Branch:      current.Branch,
				AllowScopes: current.AllowScopes,
				Enabled:     &enabled,
			})
			if err != nil {
				return err
			}

			updated, err := triggerRequest(cmd, http.MethodPut, "/deploy/triggers/"+args[0], bytes.NewReader(body))
			if err != nil {
				return err
			}

			fmt.Printf("Trigger %s %sd (%s @ %s)\n", updated.ID, verb, updated.Repo, updated.Branch)

			return nil
		},
	}
}

func newTriggerDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Withdraw trust from a repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := controllerDo(cmd.Root(), http.MethodDelete, "/deploy/triggers/"+args[0], "", nil)
			if err != nil {
				return err
			}

			defer resp.Body.Close()

			raw, _ := io.ReadAll(resp.Body)

			if resp.StatusCode >= 400 {
				return triggerError(resp.StatusCode, raw)
			}

			fmt.Printf("Trigger %s deleted\n", args[0])

			return nil
		},
	}
}

// triggerRequest performs a call that returns a single trigger record.
func triggerRequest(cmd *cobra.Command, method, path string, body io.Reader) (triggerRecord, error) {
	resp, err := controllerDo(cmd.Root(), method, path, "", body)
	if err != nil {
		return triggerRecord{}, err
	}

	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return triggerRecord{}, err
	}

	if resp.StatusCode >= 400 {
		return triggerRecord{}, triggerError(resp.StatusCode, raw)
	}

	var env triggerEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return triggerRecord{}, fmt.Errorf("decode response: %w", err)
	}

	var record triggerRecord
	if err := json.Unmarshal(env.Data, &record); err != nil {
		return triggerRecord{}, fmt.Errorf("decode trigger: %w", err)
	}

	return record, nil
}

// triggerError surfaces the controller's own message rather than the status
// code. The controller already explains what was wrong with the input, and
// re-describing it here would be a second, worse explanation.
func triggerError(status int, raw []byte) error {
	var env triggerEnvelope

	if err := json.Unmarshal(raw, &env); err == nil && env.Error != "" {
		return fmt.Errorf("%s", env.Error)
	}

	return fmt.Errorf("controller returned %d", status)
}

// humanTime renders an RFC3339 timestamp, or a dash when there is none. A
// "never fired" trigger is a fact worth showing plainly — it is usually the
// answer to why a deploy is not happening.
func humanTime(iso string) string {
	if iso == "" {
		return "—"
	}

	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}

	return t.Local().Format("2006-01-02 15:04")
}
