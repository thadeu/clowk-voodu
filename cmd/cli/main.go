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
	os.Args = rewriteColonSyntax(os.Args)

	root := newRootCmd()

	if code, forwarded := maybeForwardRemote(root, os.Args[1:]); forwarded {
		os.Exit(code)
	}

	if err := dispatch(root, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
