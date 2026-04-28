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

// newConfigCmd is wired in cmd_config.go (M-4) — moved out of
// commands.go so the file shrinks back toward its original
// "miscellaneous CLI plumbing" role.
