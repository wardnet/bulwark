package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"wardnet/bulwark/internal/config"
	"wardnet/bulwark/internal/coverage"
	"wardnet/bulwark/internal/detect"
	"wardnet/bulwark/internal/executil"
	"wardnet/bulwark/internal/gitstate"
)

func newCoverageCmd() *cobra.Command {
	var dir string
	var testsMode string
	var goReport string
	var rustReport []string
	var rustLCOVReport []string
	cmd := &cobra.Command{
		Use:   "coverage",
		Short: "Diff current coverage against the bulwark-state baseline for the PR's base commit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			mode := coverage.Mode(testsMode)
			if mode != coverage.ModeRun && mode != coverage.ModeSkip {
				return fmt.Errorf("--tests must be %q or %q, got %q", coverage.ModeRun, coverage.ModeSkip, testsMode)
			}
			reports := coverage.ReportPaths{
				Go:       goReport,
				Rust:     parseRustReportOverrides(rustReport),
				RustLCOV: parseRustReportOverrides(rustLCOVReport),
			}

			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}
			patchWanted := coverage.PatchWanted{
				Go:         cfg.Coverage.Patch.Go.Enabled,
				Rust:       cfg.Coverage.Patch.Rust.Enabled,
				TypeScript: cfg.Coverage.Patch.TypeScript.Enabled,
			}

			current, sources, cleanup, err := coverage.Compute(ctx, dir, cfg, mode, reports, patchWanted)
			defer cleanup()
			if err != nil {
				return err
			}
			if len(current) == 0 {
				msg := "no coverage measured — no coverage tooling detected/available for any ecosystem"
				if mode == coverage.ModeSkip {
					msg += " (--tests=skip only reads an existing report — did an earlier CI step produce one at the expected path?)"
				}
				_, err := fmt.Fprintln(cmd.OutOrStdout(), msg)
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

			aggregateErr := diffReport(cmd, current, baseline)
			// sha is the same merge-base gitstate.BaseSHA already resolved for
			// the aggregate baseline lookup above — patch coverage reuses it
			// directly rather than recomputing "git merge-base HEAD origin/main"
			// a second time.
			patchErr := patchReport(cmd, ctx, dir, patchWanted, sources, sha, baseline)
			// errors.Join keeps both messages when aggregate AND patch coverage
			// regress in the same run — AGENTS.md's documented "compute and gate
			// on both, not either/or" contract must hold for the returned error
			// too, not just for what gets printed to stdout above.
			return errors.Join(aggregateErr, patchErr)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "repository root")
	cmd.Flags().StringVar(&testsMode, "tests", string(coverage.ModeRun),
		`whether to execute tests ("run", the default — good for local dev) or only parse an
existing report a prior CI step already produced ("skip" — use in CI once that step already
runs with coverage instrumentation on, so tests aren't executed a second/third time)`)
	cmd.Flags().StringVar(&goReport, "go-report", "",
		"path (relative to --dir) to an existing go coverage profile; only used with --tests=skip. Default: search coverage.out, cover.out, c.out")
	cmd.Flags().StringArrayVar(&rustReport, "rust-report", nil,
		`path (relative to --dir) to an existing cargo-llvm-cov JSON export; only used with --tests=skip.
Repeatable. A bare path applies only when exactly one Rust crate/workspace is discovered under --dir;
for multiple crates, disambiguate with "<crateDir>=<path>" (crateDir relative to --dir), e.g.
--rust-report daemon=daemon/coverage/daemon-llvm-cov.json. Default per crate: search
coverage/llvm-cov.json, llvm-cov.json, target/llvm-cov/llvm-cov.json relative to that crate's directory`)
	cmd.Flags().StringArrayVar(&rustLCOVReport, "rust-lcov-report", nil,
		`path (relative to --dir) to an existing cargo-llvm-cov lcov export, used for Rust patch coverage;
only used with --tests=skip. Repeatable, same bare-vs-"<crateDir>=<path>" syntax as --rust-report.
Default per crate: search coverage/lcov.info, lcov.info, target/llvm-cov/lcov.info relative to that
crate's directory`)
	return cmd
}

// parseRustReportOverrides parses repeated --rust-report/--rust-lcov-report
// flag values into a coverage.RustReportOverrides map. Each value is either a
// bare path (stored under the "" key, consulted only when Rust discovery
// finds exactly one crate — preserving the original single-crate CLI usage
// unchanged) or a "<crateDir>=<path>" pair disambiguating which discovered
// crate directory (relative to --dir) the override applies to.
func parseRustReportOverrides(values []string) coverage.RustReportOverrides {
	if len(values) == 0 {
		return nil
	}
	overrides := make(coverage.RustReportOverrides, len(values))
	for _, v := range values {
		if key, path, ok := strings.Cut(v, "="); ok {
			overrides[key] = path
		} else {
			overrides[""] = v
		}
	}
	return overrides
}

// computeBaselineAt checks out origin/main at sha into a throwaway worktree
// and computes coverage there, so a cache miss doesn't disturb the caller's
// own working tree/branch. This always actually runs tests (coverage.ModeRun),
// regardless of the top-level --tests flag: a historical commit's checkout
// has no pre-existing CI-produced report to find — the report itself would
// have to come from actually running the suite at that commit — so there is
// no "skip" equivalent for baseline computation. This is a one-time cost per
// main commit (cached afterward on the bulwark-state branch), not a
// per-invocation cost, so it doesn't reintroduce the duplicate-test-run
// problem --tests=skip exists to avoid.
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
	// A baseline is only ever an aggregate percentage, never a patch-coverage
	// source — patch coverage always compares against the current tree's
	// baseline lookup, never a baseline-of-a-baseline — so PatchWanted is the
	// zero value here, and the resolved sources/cleanup are discarded.
	report, _, cleanup, err := coverage.Compute(ctx, tmp, cfg, coverage.ModeRun, coverage.ReportPaths{}, coverage.PatchWanted{})
	defer cleanup()
	return report, err
}

// statusPrefix renders a bracketed status tag padded to a fixed column width
// (10 characters, e.g. "[FAIL]    "), shared by diffReport and patchReport
// so the two gates can't drift apart on formatting — action.yml's PR-comment
// builder greps for exactly this "[TAG]<spaces>" vocabulary.
func statusPrefix(tag string) string {
	bracket := "[" + tag + "]"
	pad := max(10-len(bracket), 1)
	return bracket + strings.Repeat(" ", pad)
}

// patchReport prints one bracketed status line per language whose patch
// coverage was requested (cfg.Coverage.Patch.<lang>.Enabled) and whose
// source Compute managed to resolve, gating patch% against that language's
// aggregate baseline (patch coverage has no baseline of its own — see
// CONTEXT.md). A language with no baseline yet is reported informationally,
// not failed, mirroring diffReport's [NEW] handling. A language with zero
// coverable changed lines (e.g. the diff only touched comments/imports, or
// its source couldn't be resolved) is skipped entirely — there's nothing to
// gate on.
//
// ChangedLines is called exactly once, for the union of every wanted
// language's extensions, then partitioned per language — not once per
// language — since all three langs diff the identical mergeBase..HEAD range.
func patchReport(cmd *cobra.Command, ctx context.Context, dir string, want coverage.PatchWanted, sources coverage.PatchSources, mergeBase string, baseline map[string]float64) error {
	type language struct {
		name   string
		wanted bool
		exts   []string
	}
	langs := []language{
		{name: "go", wanted: want.Go, exts: detect.Extensions[detect.Go]},
		{name: "rust", wanted: want.Rust, exts: detect.Extensions[detect.Rust]},
		{name: "typescript", wanted: want.TypeScript, exts: detect.Extensions[detect.TypeScript]},
	}

	var allExts []string
	for _, lang := range langs {
		if lang.wanted {
			allExts = append(allExts, lang.exts...)
		}
	}
	if len(allExts) == 0 {
		return nil
	}
	changed, err := coverage.ChangedLines(ctx, dir, mergeBase, allExts...)
	if err != nil {
		return err
	}

	regressed := 0
	for _, lang := range langs {
		if !lang.wanted {
			continue
		}
		langChanged := filterByExt(changed, lang.exts)

		var hit, total int
		switch lang.name {
		case "go":
			if sources.GoProfile == "" || sources.ModuleName == "" {
				continue
			}
			hits, err := coverage.ParseGoProfile(sources.GoProfile, sources.ModuleName, dir)
			if err != nil {
				continue
			}
			hit, total = coverage.PatchPercent(langChanged, hits)
		case "rust":
			if len(sources.RustLCOV) == 0 {
				continue
			}
			hit, total = rustPatchPercent(dir, sources.RustLCOV, langChanged)
		case "typescript":
			if len(sources.TSLCOV) == 0 {
				continue
			}
			hit, total = tsPatchPercent(dir, sources.TSLCOV, langChanged)
		}
		if total == 0 {
			continue
		}
		pct := float64(hit) / float64(total) * 100

		base, baseOK := baseline[lang.name]
		var tag, detail string
		switch {
		case !baseOK:
			tag, detail = "NEW", fmt.Sprintf("%s patch: %.1f%% (%d/%d new lines; no baseline yet)", lang.name, pct, hit, total)
		case pct < base:
			regressed++
			tag, detail = "FAIL", fmt.Sprintf("%s patch: %.1f%% (%d/%d new lines; baseline %.1f%%)", lang.name, pct, hit, total, base)
		default:
			tag, detail = "PASS", fmt.Sprintf("%s patch: %.1f%% (%d/%d new lines; baseline %.1f%%)", lang.name, pct, hit, total, base)
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), statusPrefix(tag)+detail); err != nil {
			return err
		}
	}
	if regressed > 0 {
		return fmt.Errorf("patch coverage regressed for %d language(s)", regressed)
	}
	return nil
}

// filterByExt returns the subset of changed whose file name ends in one of
// exts — used to partition one shared ChangedLines call per language.
func filterByExt(changed map[string][]int, exts []string) map[string][]int {
	out := map[string][]int{}
	for file, lines := range changed {
		for _, ext := range exts {
			if strings.HasSuffix(file, ext) {
				out[file] = lines
				break
			}
		}
	}
	return out
}

// tsPatchPercent sums patch coverage across every TS package independently,
// rather than merging all packages' LineHits into one shared map first: two
// packages can each have a file at the same package-relative path (e.g. both
// have src/index.ts), and a naive merge would let one clobber the other's
// hit data under Go's unordered map iteration. Packages are matched
// longest-prefix-first so a nested package claims its own changed files
// before a shorter/root package would double-count them. A package whose
// prefix has no overlap with changed at all is skipped without ever reading
// or parsing its lcov file.
func tsPatchPercent(dir string, tsLCOV map[string]string, changed map[string][]int) (hit, total int) {
	type pkg struct {
		prefix string // relative to dir, "" for dir itself
		hits   coverage.LineHits
	}
	var pkgs []pkg
	for pkgDir, lcovPath := range tsLCOV {
		rel, err := filepath.Rel(dir, pkgDir)
		if err != nil {
			continue
		}
		prefix := filepath.ToSlash(rel)
		if prefix == "." {
			prefix = ""
		}
		overlaps := false
		for file := range changed {
			if prefix == "" || strings.HasPrefix(file, prefix+"/") {
				overlaps = true
				break
			}
		}
		if !overlaps {
			continue
		}
		data, err := os.ReadFile(lcovPath) // #nosec G304 -- lcovPath is resolved by bulwark's own fixed-convention lookup under a detected package dir, not user input
		if err != nil {
			continue
		}
		pkgs = append(pkgs, pkg{prefix: prefix, hits: coverage.ParseLCOV(data, pkgDir)})
	}
	sort.Slice(pkgs, func(i, j int) bool { return len(pkgs[i].prefix) > len(pkgs[j].prefix) })

	assigned := map[string]bool{}
	for _, p := range pkgs {
		scoped := map[string][]int{}
		for file, lines := range changed {
			if assigned[file] {
				continue
			}
			rel := file
			if p.prefix != "" {
				if !strings.HasPrefix(file, p.prefix+"/") {
					continue
				}
				rel = strings.TrimPrefix(file, p.prefix+"/")
			}
			scoped[rel] = lines
			assigned[file] = true
		}
		h, t := coverage.PatchPercent(scoped, p.hits)
		hit += h
		total += t
	}
	return hit, total
}

// rustPatchPercent sums patch coverage across every discovered Rust crate
// independently, mirroring tsPatchPercent's per-package longest-prefix
// matching: multiple crates can each have a file at the same crate-relative
// path, and a naive merge would let one clobber another's hit data under
// Go's unordered map iteration.
func rustPatchPercent(dir string, rustLCOV map[string]string, changed map[string][]int) (hit, total int) {
	type crate struct {
		prefix string // relative to dir, "" for dir itself
		hits   coverage.LineHits
	}
	var crates []crate
	for crateDir, lcovPath := range rustLCOV {
		rel, err := filepath.Rel(dir, crateDir)
		if err != nil {
			continue
		}
		prefix := filepath.ToSlash(rel)
		if prefix == "." {
			prefix = ""
		}
		overlaps := false
		for file := range changed {
			if prefix == "" || strings.HasPrefix(file, prefix+"/") {
				overlaps = true
				break
			}
		}
		if !overlaps {
			continue
		}
		data, err := os.ReadFile(lcovPath) // #nosec G304 -- lcovPath is resolved by bulwark itself (a scratch path it wrote, or its own candidate-list/flag lookup), not user input
		if err != nil {
			continue
		}
		crates = append(crates, crate{prefix: prefix, hits: coverage.ParseLCOV(data, crateDir)})
	}
	sort.Slice(crates, func(i, j int) bool { return len(crates[i].prefix) > len(crates[j].prefix) })

	assigned := map[string]bool{}
	for _, c := range crates {
		scoped := map[string][]int{}
		for file, lines := range changed {
			if assigned[file] {
				continue
			}
			rel := file
			if c.prefix != "" {
				if !strings.HasPrefix(file, c.prefix+"/") {
					continue
				}
				rel = strings.TrimPrefix(file, c.prefix+"/")
			}
			scoped[rel] = lines
			assigned[file] = true
		}
		h, t := coverage.PatchPercent(scoped, c.hits)
		hit += h
		total += t
	}
	return hit, total
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
		var tag, detail string
		switch {
		case !baseOK:
			tag, detail = "NEW", fmt.Sprintf("%s: %.1f%% (no baseline yet)", lang, cur)
		case !curOK:
			tag, detail = "DROPPED", fmt.Sprintf("%s: no longer measured (baseline was %.1f%%)", lang, base)
		case cur < base:
			regressed++
			tag, detail = "FAIL", fmt.Sprintf("%s: %.1f%% (baseline %.1f%%, regressed %.1f%%)", lang, cur, base, base-cur)
		default:
			tag, detail = "PASS", fmt.Sprintf("%s: %.1f%% (baseline %.1f%%)", lang, cur, base)
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), statusPrefix(tag)+detail); err != nil {
			return err
		}
	}
	if regressed > 0 {
		return fmt.Errorf("coverage regressed for %d language(s)", regressed)
	}
	return nil
}
