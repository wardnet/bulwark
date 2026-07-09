package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"wardnet/bulwark/internal/config"
	"wardnet/bulwark/internal/coverage"
	"wardnet/bulwark/internal/executil"
	"wardnet/bulwark/internal/gitstate"
)

func newCoverageCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "coverage",
		Short: "Diff current coverage against the bulwark-state baseline for the PR's base commit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}
			exclude := cfg.AllExcludes()

			current, err := coverage.Compute(ctx, dir, exclude)
			if err != nil {
				return err
			}
			if len(current) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "no coverage measured — no coverage tooling detected/available for any ecosystem")
				return err
			}

			sha, err := gitstate.BaseSHA(ctx, dir)
			if err != nil {
				return err
			}

			baseline, hit, err := gitstate.ReadBaseline(ctx, dir, sha)
			if err != nil {
				return err
			}
			if !hit {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "no cached baseline for %s — computing one now (first PR against this main commit pays this cost)\n", sha); err != nil {
					return err
				}
				baseline, err = computeBaselineAt(ctx, dir, sha, exclude)
				if err != nil {
					return err
				}
				if err := gitstate.WriteBaseline(ctx, dir, sha, baseline); err != nil {
					return err
				}
			}

			return diffReport(cmd, current, baseline)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "repository root")
	return cmd
}

// computeBaselineAt checks out origin/main at sha into a throwaway worktree
// and computes coverage there, so a cache miss doesn't disturb the caller's
// own working tree/branch.
func computeBaselineAt(ctx context.Context, dir, sha string, exclude []string) (map[string]float64, error) {
	tmp, err := os.MkdirTemp("", "bulwark-baseline-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	defer func() { _ = executil.Run(ctx, dir, "git", "worktree", "remove", "--force", tmp) }()

	if r := executil.Run(ctx, dir, "git", "worktree", "add", "--detach", tmp, sha); !r.Ok() {
		return nil, fmt.Errorf("worktree add %s at %s: %w", tmp, sha, r.Err)
	}
	return coverage.Compute(ctx, tmp, exclude)
}

// diffReport prints current vs. baseline per language and fails if any
// language's coverage regressed. A language present only in current (no
// baseline entry — newly added) or only in baseline (dropped) is reported
// but doesn't fail the check on its own.
func diffReport(cmd *cobra.Command, current, baseline map[string]float64) error {
	regressed := 0
	for lang, cur := range current {
		base, ok := baseline[lang]
		switch {
		case !ok:
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "[NEW]  %s: %.1f%% (no baseline yet)\n", lang, cur); err != nil {
				return err
			}
		case cur < base:
			regressed++
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "[FAIL] %s: %.1f%% (baseline %.1f%%, regressed %.1f%%)\n", lang, cur, base, base-cur); err != nil {
				return err
			}
		default:
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "[PASS] %s: %.1f%% (baseline %.1f%%)\n", lang, cur, base); err != nil {
				return err
			}
		}
	}
	if regressed > 0 {
		return fmt.Errorf("coverage regressed for %d language(s)", regressed)
	}
	return nil
}
