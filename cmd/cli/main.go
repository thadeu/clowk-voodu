package main

import (
	"errors"
	"fmt"
	"os"
)

var (
	version = "0.1.0-dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Args = rewriteColonSyntax(os.Args)

	root := newRootCmd()

	if code, forwarded := maybeForwardRemote(root, os.Args[1:]); forwarded {
		os.Exit(code)
	}

	if err := dispatch(root, os.Args[1:]); err != nil {
		// `voodu diff --detailed-exitcode` uses this sentinel to ask for
		// exit code 2 when the plan has pending changes. Treat it as a
		// success signal for the terminal (no stderr noise) but a
		// non-zero code for CI branching — terraform-plan compatible.
		if errors.Is(err, errExitWithChanges) {
			os.Exit(2)
		}

		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
