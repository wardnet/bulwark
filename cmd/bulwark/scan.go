package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"wardnet/bulwark/internal/config"
	"wardnet/bulwark/internal/detect"
	"wardnet/bulwark/internal/executil"
	"wardnet/bulwark/internal/golang"
	"wardnet/bulwark/internal/rust"
	"wardnet/bulwark/internal/semgrep"
	"wardnet/bulwark/internal/typescript"
)

func newScanCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Run code-quality and security checks for every detected ecosystem",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}

			ecosystems, err := detect.Ecosystems(dir, allExcludes(cfg))
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
					results = append(results, rust.Check(ctx, dir)...)
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
				results = append(results, semgrep.Check(ctx, dir, cfg.Semgrep.Config))
			}

			return report(cmd, results)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "root directory to scan")
	return cmd
}

// allExcludes merges every language's exclude list, since Ecosystems' initial
// detection pass doesn't yet know which language a given excluded directory
// belongs to.
func allExcludes(cfg config.Config) []string {
	var out []string
	out = append(out, cfg.Rust.Exclude...)
	out = append(out, cfg.TypeScript.Exclude...)
	out = append(out, cfg.Go.Exclude...)
	return out
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
