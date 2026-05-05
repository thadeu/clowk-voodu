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
	// Skip rewrite when the client has already applied it
	// (forwarded SSH invocation). Re-rewriting would split a
	// multi-segment plugin command like `pg:backups:capture`
	// twice — first the client splits to `["pg", "backups:capture"]`,
	// then the server would re-split `backups:capture` into
	// `["backups", "capture"]`, which collapses to the wrong
	// command (`pg:backups` with arg `capture`) at dispatch.
	if os.Getenv(envRewriteAlreadyApplied) != "1" {
		os.Args = rewriteColonSyntax(os.Args)
	}

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
