// cmd_pat.go is the `vd pat *` command surface — the only path
// operators use to mint, list, and revoke PATs. Hits the controller's
// /pats endpoints over the orchestration plane (SSH-forwarded by
// localOnlyCommands, since "pat" is in the localOnly list).
//
// Three verbs, terse on purpose:
//
//	vd pat create [--scope=read,actions] [--name=label]
//	vd pat create --scope=all --name=console
//	vd pat list
//	vd pat revoke <id>
//
// Create's response shows the plain token EXACTLY ONCE. Operators
// who lose it must revoke + remint — same posture as GitHub PATs,
// 1Password tokens, AWS access keys.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
)

// patCreateRequest mirrors controller.patCreateRequest. Kept local
// so the CLI doesn't import controller-side types (those are
// internal/, not part of the public Go API).
type patCreateRequest struct {
	Scopes []string `json:"scopes"`
	Name   string   `json:"name,omitempty"`
}

// patEnvelope is the response envelope shared with the controller.
// Decoded into the right inner type per command.
type patEnvelope struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type patCreateResponse struct {
	Token  string      `json:"token"`
	Record patListItem `json:"record"`
}

type patListItem struct {
	ID         string   `json:"id"`
	Scopes     []string `json:"scopes"`
	Name       string   `json:"name,omitempty"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
}

type patListResponse struct {
	PATs []patListItem `json:"pats"`
}

// newPATCmd builds the `vd pat` subcommand tree. Wired into the
// root command in root.go's AddCommand list.
func newPATCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pat",
		Short: "Manage Personal Access Tokens for the WebUI observability plane",
		Long: `pat manages the credentials the voodu WebUI uses to talk to this
controller's observability API (/api/pat/v1/*).

Three verbs:
  vd pat create   — mint a new PAT (plain token shown ONCE)
  vd pat list     — show all PATs on this host (never the plain token)
  vd pat revoke   — delete a PAT by ID

PATs live on the host. To use a PAT from the WebUI, register the
controller's endpoint + PAT in the WebUI's server settings.`,
	}

	cmd.AddCommand(newPATCreateCmd(), newPATListCmd(), newPATRevokeCmd())

	return cmd
}

func newPATCreateCmd() *cobra.Command {
	var (
		scope string
		name  string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint a new Personal Access Token",
		Long: `create mints a fresh PAT and prints the plain token EXACTLY ONCE.

Lost tokens cannot be recovered — revoke + create a new one.

Scopes:
  read         — GET endpoints (stats, pods, logs, metrics, activity)
  config:read  — read config keys, masked by default
  config       — set and unset config values
  actions      — POST mutations (restart, plugin install) + rate-limited
  deploy       — the deploy/ subtree: triggers, preflight, firing a deploy

Defaults to "read" only. Scopes are additive and independent: a token
scoped to deploy alone can do nothing but deploy.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPATCreate(cmd, scope, name)
		},
	}

	cmd.Flags().StringVar(&scope, "scope", "read", "comma-separated scopes ("+scopeFlagHint()+")")
	cmd.Flags().StringVar(&name, "name", "", "operator-supplied label (optional)")

	return cmd
}

func newPATListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all PATs on this host",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPATList(cmd)
		},
	}
}

func newPATRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Delete a PAT by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPATRevoke(cmd, args[0])
		},
	}
}

func runPATCreate(cmd *cobra.Command, scopeFlag, name string) error {
	root := cmd.Root()

	// Validate scope locally so the operator sees an immediate
	// error message instead of one filtered through the controller's
	// 400 response. Server still validates on its side (defense
	// in depth + protection against direct curl).
	scopes := []string{}
	for _, s := range strings.Split(scopeFlag, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			scopes = append(scopes, s)
		}
	}

	scopes = expandAll(scopes)

	if len(scopes) == 0 {
		return fmt.Errorf("--scope is required (%s)", scopeFlagHint())
	}

	body, _ := json.Marshal(patCreateRequest{Scopes: scopes, Name: strings.TrimSpace(name)})

	resp, err := controllerDo(root, http.MethodPost, "/pats", "", bytes.NewReader(body))
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var env patEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response (status %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if env.Status == "error" || resp.StatusCode >= 400 {
		if env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return formatControllerError(resp.StatusCode, raw)
	}

	var cr patCreateResponse
	if err := json.Unmarshal(env.Data, &cr); err != nil {
		return fmt.Errorf("decode create response: %w", err)
	}

	// Render the plain token loudly. Mint color + the "shown once"
	// banner because operators routinely miss this and have to
	// revoke + remint.
	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "%s %s\n",
		colorize(cMint400, "PAT created:"),
		colorize(cMint400, cr.Token))
	fmt.Fprintf(os.Stdout, "  %s\n", dim("⚠ shown ONCE — copy now, can't be recovered"))
	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "  ID:        %s\n", cr.Record.ID)
	fmt.Fprintf(os.Stdout, "  Scopes:    %s\n", strings.Join(cr.Record.Scopes, ", "))

	if cr.Record.Name != "" {
		fmt.Fprintf(os.Stdout, "  Name:      %s\n", cr.Record.Name)
	}

	fmt.Fprintf(os.Stdout, "  Created:   %s\n", cr.Record.CreatedAt)
	fmt.Fprintln(os.Stdout)

	return nil
}

func runPATList(cmd *cobra.Command) error {
	root := cmd.Root()

	resp, err := controllerDo(root, http.MethodGet, "/pats", "", nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var env patEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response (status %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if env.Status == "error" || resp.StatusCode >= 400 {
		if env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return formatControllerError(resp.StatusCode, raw)
	}

	var lr patListResponse
	if err := json.Unmarshal(env.Data, &lr); err != nil {
		return fmt.Errorf("decode list response: %w", err)
	}

	if len(lr.PATs) == 0 {
		fmt.Fprintln(os.Stdout, "No PATs created yet. Run `vd pat create` to mint one.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSCOPES\tCREATED\tLAST USED")

	for _, p := range lr.PATs {
		name := p.Name
		if name == "" {
			name = "-"
		}

		scopes := strings.Join(p.Scopes, ",")

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.ID,
			name,
			scopes,
			formatPATRelativeTime(p.CreatedAt),
			formatPATLastUsed(p.LastUsedAt),
		)
	}

	return tw.Flush()
}

func runPATRevoke(cmd *cobra.Command, id string) error {
	root := cmd.Root()

	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("PAT id is required")
	}

	q := url.Values{}
	_ = q

	resp, err := controllerDo(root, http.MethodDelete, "/pats/"+url.PathEscape(id), "", nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("no PAT with id %q", id)
	}

	if resp.StatusCode >= 400 {
		var env patEnvelope
		_ = json.Unmarshal(raw, &env)

		if env.Error != "" {
			return fmt.Errorf("%s", env.Error)
		}

		return formatControllerError(resp.StatusCode, raw)
	}

	fmt.Fprintf(os.Stdout, "%s PAT %s revoked\n", check(), id)

	return nil
}

// formatPATRelativeTime renders an RFC3339 timestamp as relative
// duration ("2d ago"). Mirrors the formatter in self_update_release
// but for PAT-listing display purposes (slightly different
// thresholds — PATs live for months, so days/weeks matter more
// than seconds).
func formatPATRelativeTime(rfc3339 string) string {
	if rfc3339 == "" {
		return "-"
	}

	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339 // fall back to raw on parse fail
	}

	d := time.Since(t)

	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	}
}

func formatPATLastUsed(rfc3339 string) string {
	if rfc3339 == "" {
		return "(never)"
	}

	return formatPATRelativeTime(rfc3339)
}

// expandAll turns `--scope=all` into the scopes that exist RIGHT NOW.
//
// EXPANDED AT CREATION, never stored as the word "all", and that is the whole
// design rather than an implementation detail. A token recorded as "all" would
// silently gain every scope added in a later release — its power would grow
// without anybody deciding that it should, and the operator who minted it
// would have no way to see that it had.
//
// Expanded, the record lists the five scopes that were granted, `vd pat list`
// prints them, and a sixth scope shipped next month reaches this token only
// when somebody mints a new one. The convenience is real; the blank cheque is
// what is being avoided.
//
// It composes: `--scope=all` and `--scope=read,all` mean the same thing, and
// the result is deduplicated by the controller's own normaliser.
func expandAll(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	seen := make(map[string]bool, len(scopes))

	for _, s := range scopes {
		if s != "all" {
			if !seen[s] {
				seen[s] = true

				out = append(out, s)
			}

			continue
		}

		for _, known := range controller.KnownScopes() {
			if !seen[string(known)] {
				seen[string(known)] = true

				out = append(out, string(known))
			}
		}
	}

	return out
}

// scopeFlagHint renders the scope vocabulary for the flag help and the
// missing-flag error, from the controller's own list. Retyping it here is how
// the previous text ended up naming two of five scopes.
func scopeFlagHint() string {
	names := make([]string, 0)

	for _, s := range controller.KnownScopes() {
		names = append(names, string(s))
	}

	// `all` last, and named as what it is: a shorthand for the list, expanded
	// at creation. See expandAll.
	return strings.Join(names, " | ") + " | all"
}
