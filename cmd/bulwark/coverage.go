package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newCoverageCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "coverage",
		Short: "Diff current coverage against the bulwark-state baseline for the PR's base commit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = dir
			return fmt.Errorf("bulwark coverage is not implemented yet")
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "repository root")
	return cmd
}
