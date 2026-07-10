package main

import (
	"context"
	"fmt"
	"os"
	"sort"

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

			current, err := coverage.Compute(ctx, dir, cfg)
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
				baseline, err = computeBaselineAt(ctx, dir, sha, cfg)
				if err != nil {
					return err
				}
				// Caching the baseline is best-effort: a write failure (worktree
				// race, disk pressure, transient git error) must never fail this
				// command outright — `current` and `baseline` are already
				// computed, and diffReport below is what actually matters.
				if err := gitstate.WriteBaseline(ctx, dir, sha, baseline); err != nil {
					if _, printErr := fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to cache coverage baseline for %s: %v\n", sha, err); printErr != nil {
						return printErr
					}
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
func computeBaselineAt(ctx context.Context, dir, sha string, cfg config.Config) (map[string]float64, error) {
	tmp, err := os.MkdirTemp("", "bulwark-baseline-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	defer func() { _ = executil.Run(ctx, dir, "git", "worktree", "remove", "--force", tmp) }()

	if r := executil.Run(ctx, dir, "git", "worktree", "add", "--detach", tmp, sha); !r.Ok() {
		return nil, fmt.Errorf("worktree add %s at %s: %w", tmp, sha, r.Err)
	}
	return coverage.Compute(ctx, tmp, cfg)
}

// diffReport prints current vs. baseline per language, covering every
// language mentioned by either side (not just current's), and fails if any
// language's coverage regressed. A language with no baseline entry (newly
// added) or a language dropped from current (no longer measurable) is
// reported but doesn't fail the check on its own — only a measured decrease
// does.
func diffReport(cmd *cobra.Command, current, baseline map[string]float64) error {
	langs := make(map[string]struct{}, len(current)+len(baseline))
	for lang := range current {
		langs[lang] = struct{}{}
	}
	for lang := range baseline {
		langs[lang] = struct{}{}
	}
	sorted := make([]string, 0, len(langs))
	for lang := range langs {
		sorted = append(sorted, lang)
	}
	sort.Strings(sorted)

	regressed := 0
	for _, lang := range sorted {
		cur, curOK := current[lang]
		base, baseOK := baseline[lang]
		var line string
		switch {
		case !baseOK:
			line = fmt.Sprintf("[NEW]     %s: %.1f%% (no baseline yet)", lang, cur)
		case !curOK:
			line = fmt.Sprintf("[DROPPED] %s: no longer measured (baseline was %.1f%%)", lang, base)
		case cur < base:
			regressed++
			line = fmt.Sprintf("[FAIL]    %s: %.1f%% (baseline %.1f%%, regressed %.1f%%)", lang, cur, base, base-cur)
		default:
			line = fmt.Sprintf("[PASS]    %s: %.1f%% (baseline %.1f%%)", lang, cur, base)
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
			return err
		}
	}
	if regressed > 0 {
		return fmt.Errorf("coverage regressed for %d language(s)", regressed)
	}
	return nil
}
