package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"wardnet/bulwark/internal/config"
	"wardnet/bulwark/internal/detect"
	"wardnet/bulwark/internal/executil"
	"wardnet/bulwark/internal/gitstate"
	"wardnet/bulwark/internal/golang"
	"wardnet/bulwark/internal/rust"
	"wardnet/bulwark/internal/semgrep"
	"wardnet/bulwark/internal/typescript"
)

func newScanCmd() *cobra.Command {
	var dir, diffBase string
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Run code-quality and security checks for every detected ecosystem",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}

			ecosystems, err := detect.Ecosystems(dir, cfg.AllExcludes())
			if err != nil {
				return err
			}
			if len(ecosystems) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "no supported ecosystem detected under", dir)
				return err
			}

			var results []executil.Result
			for _, e := range ecosystems {
				switch e {
				case detect.Rust:
					if !cfg.Rust.Enabled {
						continue
					}
					rustResults, err := rust.Check(ctx, dir, cfg.Rust.Exclude)
					if err != nil {
						return err
					}
					results = append(results, rustResults...)
				case detect.TypeScript:
					if !cfg.TypeScript.Enabled {
						continue
					}
					tsResults, err := typescript.Check(ctx, dir, cfg.TypeScript.Exclude)
					if err != nil {
						return err
					}
					results = append(results, tsResults...)
				case detect.Go:
					if !cfg.Go.Enabled {
						continue
					}
					results = append(results, golang.Check(ctx, dir)...)
				}
			}
			if cfg.Semgrep.Enabled {
				baseSHA, err := resolveDiffBase(ctx, dir, diffBase)
				if err != nil {
					return err
				}
				results = append(results, semgrep.Check(ctx, dir, cfg.Semgrep.Config, baseSHA))
			}

			return report(cmd, results)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "root directory to scan")
	cmd.Flags().StringVar(&diffBase, "diff-base", "", `only report findings introduced since this commit ("auto" resolves the merge-base with origin/main); empty scans everything`)
	return cmd
}

// resolveDiffBase turns the --diff-base flag into a commit SHA for Semgrep's
// scan-mode --baseline-commit. "auto" resolves the same merge-base
// `bulwark coverage` already gates against, so a PR's scan and coverage agree
// on what "this change" means; any other non-empty value is passed through as
// a literal ref.
//
// It's skipped entirely when SEMGREP_APP_TOKEN is set, because `semgrep ci`
// scopes itself to the diff already — resolving a merge-base there would cost
// a `git fetch` whose result nothing reads, and would newly require a full
// checkout depth from token-bearing consumers that don't need one today.
func resolveDiffBase(ctx context.Context, dir, diffBase string) (string, error) {
	if diffBase == "" || os.Getenv(semgrep.AppTokenEnv) != "" {
		return "", nil
	}
	if diffBase != "auto" {
		return diffBase, nil
	}
	// Deliberately an error, not a silent full-repo scan: falling back would
	// reintroduce exactly the surprise this flag exists to remove — a scan
	// that quietly changes scope, and starts blocking on findings the PR
	// never touched. A shallow checkout is a fixable CI misconfiguration
	// (fetch-depth: 0), so say so.
	baseSHA, err := gitstate.BaseSHA(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("--diff-base auto: %w (a full-history checkout is required — set fetch-depth: 0)", err)
	}
	return baseSHA, nil
}

// report prints a pass/fail line per check and returns an error if any
// check failed, so the process exit code reflects the aggregate result.
func report(cmd *cobra.Command, results []executil.Result) error {
	failed := 0
	for _, r := range results {
		status := "PASS"
		if !r.Ok() {
			status = "FAIL"
			failed++
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s\n", status, r.Name); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}
