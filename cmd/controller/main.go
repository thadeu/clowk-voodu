package main

import (
	"fmt"
	"os"
)

var (
	version = "0.1.0-dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			fmt.Printf("voodu-controller %s (commit: %s, built: %s)\n", version, commit, date)
			return
		}
	}

	fmt.Println("voodu-controller — not yet implemented (M3)")
	os.Exit(1)
}
