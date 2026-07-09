package main

import (
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "bulwark",
		Short:         "Unified code-quality and security scanning for Rust, TypeScript, and Go",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	cmd.AddCommand(
		newScanCmd(),
		newCoverageCmd(),
		newVersionCmd(),
		newUpdateCmd(),
	)
	return cmd
}
