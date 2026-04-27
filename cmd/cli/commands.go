package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/paths"
	"go.voodu.clowk.in/internal/remote"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the voodu version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("voodu %s (commit: %s, built: %s)\n", version, commit, date)
			return nil
		},
	}
}

func newSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Initialize the Voodu root directory on this host",
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs := []string{
				paths.Root(),
				paths.AppsDir(),
				paths.PluginsDir(),
				paths.ServicesDir(),
				paths.ScriptsDir(),
				paths.StateDir(),
				paths.VolumesDir(),
			}

			for _, d := range dirs {
				if err := os.MkdirAll(d, 0755); err != nil {
					return fmt.Errorf("mkdir %s: %w", d, err)
				}
			}

			if err := remote.WriteRCMode(remote.ModeServer); err != nil {
				return fmt.Errorf("write ~/.voodurc: %w", err)
			}

			fmt.Printf("Voodu root initialized at %s\n", paths.Root())
			fmt.Println("marked this host as mode=server in ~/.voodurc")

			return nil
		},
	}
}

func newAppsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apps",
		Short: "Manage Voodu applications",
	}

	cmd.AddCommand(appsCreateCmd(), appsListCmd())

	return cmd
}

func appsCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create NAME",
		Short: "Create an app: directories and initial env file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]

			dirs := []string{
				paths.AppDir(app),
				paths.AppReleasesDir(app),
				paths.AppSharedDir(app),
				paths.AppVolumeDir(app),
			}

			for _, d := range dirs {
				if err := os.MkdirAll(d, 0755); err != nil {
					return fmt.Errorf("mkdir %s: %w", d, err)
				}
			}

			envFile := paths.AppEnvFile(app)

			if _, err := os.Stat(envFile); os.IsNotExist(err) {
				initial := fmt.Sprintf("# App: %s\nZERO_DOWNTIME=0\n", app)

				if err := os.WriteFile(envFile, []byte(initial), 0600); err != nil {
					return fmt.Errorf("create initial .env: %w", err)
				}
			}

			fmt.Printf("App '%s' created at %s\n", app, paths.AppDir(app))

			return nil
		},
	}
}

func appsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all apps known to this server",
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := os.ReadDir(paths.AppsDir())
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Println("No apps yet.")
					return nil
				}

				return err
			}

			for _, e := range entries {
				if e.IsDir() {
					fmt.Println(e.Name())
				}
			}

			return nil
		},
	}
}

// newConfigCmd is wired in cmd_config.go (M-4) — moved out of
// commands.go so the file shrinks back toward its original
// "miscellaneous CLI plumbing" role.
