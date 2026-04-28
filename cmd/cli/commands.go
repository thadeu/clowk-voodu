package main

import (
	"fmt"

	"github.com/spf13/cobra"
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

// Bootstrap commands (`setup`, `server init`, `server add`, `server
// list`) used to live here. They were retired because the install
// script (`./install` in this repo, served via `curl ... | bash`)
// now owns the whole bootstrap path: dirs, systemd unit, docker,
// and `~/.voodurc mode=server`. Keeping install commands in the CLI
// confuses the mental model — `psql` doesn't install postgres,
// `kubectl` doesn't install kubernetes, and `voodu` likewise stays
// purely operational.
