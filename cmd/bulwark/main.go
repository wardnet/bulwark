package main

import (
	"fmt"
	"os"
)

// version is overridden at release via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "bulwark:", err)
		os.Exit(1)
	}
	maybeNudgeUpdate()
}
