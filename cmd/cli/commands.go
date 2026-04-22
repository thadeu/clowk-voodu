package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/deploy"
	"go.voodu.clowk.in/internal/git"
	"go.voodu.clowk.in/internal/paths"
	"go.voodu.clowk.in/internal/secrets"
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
				paths.ReposDir(),
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

			fmt.Printf("Voodu root initialized at %s\n", paths.Root())

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
		Short: "Create an app: directories, bare repo, post-receive hook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]

			dirs := []string{
				paths.AppDir(app),
				paths.AppReleasesDir(app),
				paths.AppSharedDir(app),
				paths.AppVolumeDir(app),
				paths.ReposDir(),
			}

			for _, d := range dirs {
				if err := os.MkdirAll(d, 0755); err != nil {
					return fmt.Errorf("mkdir %s: %w", d, err)
				}
			}

			if err := git.SetupBareRepo(app); err != nil {
				return err
			}

			if err := git.SetupPostReceiveHook(app); err != nil {
				return err
			}

			envFile := paths.AppEnvFile(app)

			if _, err := os.Stat(envFile); os.IsNotExist(err) {
				initial := fmt.Sprintf("# App: %s\nZERO_DOWNTIME=0\n", app)

				if err := os.WriteFile(envFile, []byte(initial), 0600); err != nil {
					return fmt.Errorf("create initial .env: %w", err)
				}
			}

			fmt.Printf("App '%s' created at %s\n", app, paths.AppDir(app))
			fmt.Printf("Git remote: <user>@<host>:%s\n", app)

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

func newDeployCmd() *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy the latest push for an app",
		RunE: func(cmd *cobra.Command, args []string) error {
			if app == "" {
				return fmt.Errorf("--app/-a is required")
			}

			return deploy.Run(app, deploy.Options{LogWriter: os.Stdout})
		},
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")

	return cmd
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage per-app environment variables",
	}

	cmd.AddCommand(
		configSetCmd(),
		configGetCmd(),
		configListCmd(),
		configUnsetCmd(),
		configReloadCmd(),
	)

	return cmd
}

func configSetCmd() *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "set KEY=VALUE [KEY=VALUE ...]",
		Short: "Set one or more env vars for an app",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if app == "" {
				return fmt.Errorf("--app/-a is required")
			}

			if _, err := secrets.Set(app, args); err != nil {
				return err
			}

			for _, p := range args {
				fmt.Println(p)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")

	return cmd
}

func configGetCmd() *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "get KEY",
		Short: "Read one env var for an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if app == "" {
				return fmt.Errorf("--app/-a is required")
			}

			v, err := secrets.Get(app, args[0])
			if err != nil {
				return err
			}

			fmt.Printf("%s=%s\n", args[0], v)

			return nil
		},
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")

	return cmd
}

func configListCmd() *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all env vars for an app",
		RunE: func(cmd *cobra.Command, args []string) error {
			if app == "" {
				return fmt.Errorf("--app/-a is required")
			}

			keys, vars, err := secrets.List(app)
			if err != nil {
				return err
			}

			if len(keys) == 0 {
				fmt.Println("No environment variables set")
				return nil
			}

			for _, k := range keys {
				fmt.Printf("%s=%s\n", k, vars[k])
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")

	return cmd
}

func configUnsetCmd() *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "unset KEY [KEY ...]",
		Short: "Delete one or more env vars for an app",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if app == "" {
				return fmt.Errorf("--app/-a is required")
			}

			if err := secrets.Unset(app, args); err != nil {
				return err
			}

			for _, k := range args {
				fmt.Printf("Unset %s\n", k)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")

	return cmd
}

func configReloadCmd() *cobra.Command {
	var app string

	cmd := &cobra.Command{
		Use:   "reload",
		Short: "Recreate the active container to pick up env changes",
		RunE: func(cmd *cobra.Command, args []string) error {
			if app == "" {
				return fmt.Errorf("--app/-a is required")
			}

			if err := secrets.Reload(app); err != nil {
				return err
			}

			fmt.Printf("App '%s' reloaded successfully\n", app)

			return nil
		},
	}

	cmd.Flags().StringVarP(&app, "app", "a", "", "app name (required)")

	return cmd
}
