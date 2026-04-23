package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/controller"
)

// stubRunE is used for commands whose real implementation lands in a
// later milestone. They are registered so `voodu --help` shows the
// shape of the CLI, and so forwarding doesn't accidentally try to
// contact the controller for things we know are coming.
func stubRunE(milestone, hint string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("%s: not yet implemented (lands in %s). %s", cmd.CommandPath(), milestone, hint)
	}
}

func newStatusCmd() *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the current state of apps and services",
		RunE:  stubRunE("M3", "Status reads from the controller, which arrives in M3."),
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "filter by app name")

	return cmd
}

func newLogsCmd() *cobra.Command {
	var app string

	var follow bool

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Tail logs for an app",
		RunE:  stubRunE("M3", "Log streaming goes through the controller (M3)."),
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new lines")

	return cmd
}

func newExecCmd() *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "exec -- CMD [ARGS...]",
		Short: "Run a one-off command inside the app's container",
		RunE:  stubRunE("M3", "One-off exec runs via the controller (M3)."),
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")

	return cmd
}

func newScaleCmd() *cobra.Command {
	var app string

	var scope string

	var replicas int

	cmd := &cobra.Command{
		Use:   "scale",
		Short: "Set the number of replicas for an app",
		Long: `Scale adjusts the replica count for a deployment in place, without
rewriting the on-disk manifest. The controller fetches the current
deployment spec, mutates spec.replicas, and re-applies — so subsequent
'voodu apply' runs from the original manifest would reset the count
unless the file is updated to match.

--scope disambiguates when the same deployment name exists under more
than one scope. When only one scope owns the name, --scope can be
omitted and the CLI infers it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScale(cmd, scope, app, replicas)
		},
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")
	cmd.Flags().StringVar(&scope, "scope", "", "scope owning the deployment (optional when unambiguous)")
	cmd.Flags().IntVarP(&replicas, "replicas", "r", 0, "desired replica count (>= 1)")

	return cmd
}

// runScale fetches /apply?kind=deployment&name=<app>, mutates
// spec.replicas, and POSTs the result back. Implemented as a spec
// mutation so we do not need a dedicated /scale endpoint — the apply
// pipeline already owns all the idempotence and validation.
func runScale(cmd *cobra.Command, scope, app string, replicas int) error {
	if app == "" {
		return fmt.Errorf("--app/-a is required")
	}

	if replicas < 1 {
		return fmt.Errorf("--replicas/-r must be >= 1 (removing the app is how you scale to zero)")
	}

	root := cmd.Root()

	m, err := fetchRemote(root, controller.KindDeployment, scope, app)
	if err != nil {
		return err
	}

	if m == nil {
		if scope == "" {
			return fmt.Errorf("deployment/%s not found on controller (pass --scope if the deployment is scoped)", app)
		}

		return fmt.Errorf("deployment/%s/%s not found on controller", scope, app)
	}

	mutated, err := withReplicas(m.Spec, replicas)
	if err != nil {
		return fmt.Errorf("rewrite replicas: %w", err)
	}

	m.Spec = mutated

	body, err := json.Marshal([]controller.Manifest{*m})
	if err != nil {
		return err
	}

	resp, err := controllerDo(root, http.MethodPost, "/apply", "", bytes.NewReader(body))
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("controller returned %d: %s", resp.StatusCode, string(raw))
	}

	fmt.Printf("deployment/%s scaled to %d replicas\n", app, replicas)

	return nil
}

// withReplicas round-trips the raw spec through a generic map so we
// only touch the `replicas` key. Typed re-marshalling would drop any
// spec fields the CLI does not know about yet (plugin-kind extensions,
// forward-compat fields) — going through map[string]any preserves them.
func withReplicas(raw json.RawMessage, replicas int) (json.RawMessage, error) {
	obj := map[string]any{}

	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
	}

	obj["replicas"] = replicas

	return json.Marshal(obj)
}

func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Manage Voodu server nodes",
	}

	cmd.AddCommand(
		serverInitCmd(),
		&cobra.Command{
			Use:   "add HOST",
			Short: "Install the controller on a remote host over SSH",
			Args:  cobra.ExactArgs(1),
			RunE:  stubRunE("M3+", "Remote bootstrap over SSH is a M3 follow-up. For now run `voodu server init` on the target host."),
		},
		&cobra.Command{
			Use:   "list",
			Short: "List registered server nodes",
			RunE:  stubRunE("M3+", "Node registry lives in etcd and is populated by the controller (M3+)."),
		},
	)

	return cmd
}

