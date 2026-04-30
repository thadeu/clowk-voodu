package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/pkg/plugin"
)

// newPluginsCmd wires `voodu plugins install|list|remove|update` against
// the controller's /plugins endpoints. The heavy lifting (git clone,
// validate, atomic rename) happens controller-side; the CLI is just a
// polite HTTP client.
func newPluginsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Manage Voodu plugins (Caddy, Postgres, Mongo, ...)",
	}

	cmd.AddCommand(
		newPluginsInstallCmd(),
		newPluginsListCmd(),
		newPluginsRemoveCmd(),
		newPluginsUpdateCmd(),
	)

	return cmd
}

func newPluginsInstallCmd() *cobra.Command {
	var version string

	cmd := &cobra.Command{
		Use:   "install SOURCE",
		Short: "Install a plugin from a git repo, URL or local path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return pluginInstall(cmd.Root(), args[0], version)
		},
	}

	cmd.Flags().StringVar(&version, "version", "", "Pin to a specific git tag (e.g. 0.2.0). Without this, the default branch is cloned.")

	return cmd
}

func newPluginsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed plugins",
		RunE: func(cmd *cobra.Command, args []string) error {
			return pluginList(cmd.Root())
		},
	}
}

func newPluginsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove an installed plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return pluginRemove(cmd.Root(), args[0])
		},
	}
}

func newPluginsUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update [NAME]",
		Short: "Update one or all plugins by reinstalling from their original source",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}

			return pluginUpdate(cmd.Root(), name)
		},
	}
}

func pluginInstall(root *cobra.Command, source, version string) error {
	payload := map[string]string{"source": source}
	if version != "" {
		payload["version"] = version
	}

	body, _ := json.Marshal(payload)

	resp, err := controllerDo(root, http.MethodPost, "/plugins/install", "", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("install: %s", controllerErr(raw, resp.StatusCode))
	}

	var env struct {
		Status string          `json:"status"`
		Data   plugin.Manifest `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}

	fmt.Printf("installed %s", env.Data.Name)

	if env.Data.Version != "" {
		fmt.Printf("@%s", env.Data.Version)
	}

	fmt.Println()

	return nil
}

func pluginList(root *cobra.Command) error {
	resp, err := controllerDo(root, http.MethodGet, "/plugins", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("list: %s", controllerErr(raw, resp.StatusCode))
	}

	var env struct {
		Status string `json:"status"`
		Data   struct {
			Plugins []plugin.Manifest `json:"plugins"`
			Errors  []string          `json:"errors"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}

	if len(env.Data.Plugins) == 0 {
		fmt.Println("no plugins installed")
	}

	for _, p := range env.Data.Plugins {
		line := p.Name

		if p.Version != "" {
			line += "@" + p.Version
		}

		if p.Source != "" {
			line += "  " + p.Source
		}

		fmt.Println(line)
	}

	for _, e := range env.Data.Errors {
		fmt.Fprintf(cobraStderr(root), "warning: %s\n", e)
	}

	return nil
}

func pluginRemove(root *cobra.Command, name string) error {
	resp, err := controllerDo(root, http.MethodDelete, "/plugins/"+name, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("plugin %q not installed", name)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("remove: %s", controllerErr(raw, resp.StatusCode))
	}

	fmt.Printf("removed %s\n", name)

	return nil
}

// pluginUpdate reinstalls plugins from the Source recorded at install
// time. "update foo" updates one; bare "update" updates every plugin
// that has a recorded Source (local installs without a source are skipped).
func pluginUpdate(root *cobra.Command, name string) error {
	resp, err := controllerDo(root, http.MethodGet, "/plugins", "", nil)
	if err != nil {
		return err
	}

	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("list: %s", controllerErr(raw, resp.StatusCode))
	}

	var env struct {
		Data struct {
			Plugins []plugin.Manifest `json:"plugins"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}

	targets := env.Data.Plugins

	if name != "" {
		targets = nil

		for _, p := range env.Data.Plugins {
			if p.Name == name {
				targets = []plugin.Manifest{p}
				break
			}
		}

		if len(targets) == 0 {
			return fmt.Errorf("plugin %q not installed", name)
		}
	}

	var failed []string

	for _, p := range targets {
		if p.Source == "" {
			fmt.Printf("skip %s (no recorded source)\n", p.Name)
			continue
		}

		// `vd plugins update` reinstalls from the recorded
		// source at the latest tag (default branch). Pin via
		// `vd plugins install <source> --version X` if needed.
		if err := pluginInstall(root, p.Source, ""); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", p.Name, err))
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("update errors:\n  %s", strings.Join(failed, "\n  "))
	}

	return nil
}

func controllerErr(raw []byte, code int) string {
	var env struct {
		Error string `json:"error"`
	}

	if err := json.Unmarshal(raw, &env); err == nil && env.Error != "" {
		return env.Error
	}

	return fmt.Sprintf("HTTP %d: %s", code, strings.TrimSpace(string(raw)))
}

func cobraStderr(root *cobra.Command) io.Writer {
	if w := root.ErrOrStderr(); w != nil {
		return w
	}

	return io.Discard
}
