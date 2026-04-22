package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"go.voodu.clowk.in/internal/paths"
)

//go:embed assets/voodu-controller.service
var systemdUnit string

func serverInitCmd() *cobra.Command {
	var (
		printOnly  bool
		systemdDir string
		autoStart  bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize the Voodu server on this host (creates /opt/voodu, installs systemd unit)",
		Long: `init is the localhost bootstrap for the controller.

It (1) creates the Voodu root layout, (2) writes a systemd unit file for
voodu-controller, and (3) optionally reloads systemd and starts the
service. Run this on a fresh host before pointing the CLI at it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if printOnly {
				fmt.Print(systemdUnit)
				return nil
			}

			if err := ensureServerLayout(); err != nil {
				return err
			}

			unitPath := filepath.Join(systemdDir, "voodu-controller.service")

			if err := os.MkdirAll(systemdDir, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", systemdDir, err)
			}

			if err := os.WriteFile(unitPath, []byte(systemdUnit), 0644); err != nil {
				return fmt.Errorf("write %s: %w", unitPath, err)
			}

			fmt.Printf("Wrote systemd unit to %s\n", unitPath)

			if !autoStart {
				fmt.Println("Next steps:")
				fmt.Println("  systemctl daemon-reload")
				fmt.Println("  systemctl enable --now voodu-controller")

				return nil
			}

			for _, cmd := range [][]string{
				{"systemctl", "daemon-reload"},
				{"systemctl", "enable", "--now", "voodu-controller"},
			} {
				c := exec.Command(cmd[0], cmd[1:]...)
				c.Stdout = os.Stdout
				c.Stderr = os.Stderr

				if err := c.Run(); err != nil {
					return fmt.Errorf("%s: %w", cmd[0], err)
				}
			}

			fmt.Println("voodu-controller is running. Check with: systemctl status voodu-controller")

			return nil
		},
	}

	cmd.Flags().BoolVar(&printOnly, "print-systemd", false, "print the systemd unit to stdout and exit")
	cmd.Flags().StringVar(&systemdDir, "systemd-dir", "/etc/systemd/system", "directory to write the unit file")
	cmd.Flags().BoolVar(&autoStart, "start", false, "reload systemd and start the service after writing the unit")

	return cmd
}

func ensureServerLayout() error {
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

	return nil
}
