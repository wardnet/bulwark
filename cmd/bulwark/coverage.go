package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

// priorBaselineDepth bounds how far back the baseline writers look for a
// prior baseline to carry a detected-but-unmeasured language forward from.
// With the carry-forward rule applied on every recorded baseline, the nearest
// one already contains everything worth carrying, so this only needs to span
// gaps (main commits whose CI never recorded), not real history.
const priorBaselineDepth = 50

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
			// A partially-measured current tree is just as invisible as an
			// unmeasured one: the language simply doesn't appear in the report.
			detected, err := detect.Ecosystems(dir, cfg.AllExcludes())
			if err != nil {
				return err
			}
			ecosystems := enabledEcosystems(detected, cfg)
			if err := warnUnmeasured(cmd, ecosystems, current, "the current tree"); err != nil {
				return err
			}

			// Resolved leniently: with nothing measured (a repo without an
			// origin/main, say) the answer below is "no coverage measured",
			// not a merge-base error — the errors only surface once they
			// block an actual gate.
			sha, shaErr := gitstate.BaseSHA(ctx, dir)

			// Running ON the merge-base (a push to main) rather than ahead of it
			// (a PR): there is no baseline to gate against — the current commit
			// *is* the baseline — so record what was just measured and stop.
			//
			// This is what makes the gate work at all for a repo whose coverage
			// comes from a multi-job pipeline rather than from bulwark running the
			// tests itself (exactly the case --tests=skip exists to serve). Such a
			// repo can never recompute a historical baseline: computeBaselineAt's
			// throwaway worktree is a bare checkout with none of the toolchain or
			// staged reports the pipeline provides, so it measures nothing. wardnet
			// hit precisely that — and the numbers it failed to reconstruct in a
			// worktree were numbers it had already measured, and thrown away, when
			// this same command ran on main. Recording them costs nothing: no
			// re-run, no cargo-llvm-cov, no yarn — they are already in hand.
			//
			// A main run that measured NOTHING (a docs-only merge: every
			// coverage producer path-filtered away, no reports for
			// --tests=skip to read) still records — the carry-forward fills
			// every detected language from the nearest prior baseline. The
			// old early-return here left such a commit with no baseline at
			// all, so the first PR against it recomputed nothing, reported
			// every language as [NEW], and gated on nothing
			// (wardnet/wardnet#899).
			head, headErr := gitstate.HeadSHA(ctx, dir)
			if shaErr == nil && headErr == nil && head == sha {
				record, carried, err := carryForwardBaseline(ctx, cmd, dir, sha, ecosystems, current)
				if err != nil {
					return err
				}
				if len(record) == 0 {
					return printNoCoverage(cmd, mode)
				}
				if err := gitstate.WriteBaseline(ctx, dir, sha, record); err != nil {
					// Best-effort, as everywhere else: a write race with a concurrent
					// main build must not fail the build.
					_, printErr := fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to record coverage baseline for %s: %v\n", sha, err)
					return printErr
				}
				note := ""
				if len(carried) > 0 {
					note = fmt.Sprintf(" (%s carried forward from a prior baseline — detected but not measured this run)", strings.Join(carried, ", "))
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "recorded coverage baseline for %s: %s%s\n", sha, formatReport(record), note)
				return err
			}

			if len(current) == 0 {
				return printNoCoverage(cmd, mode)
			}
			if shaErr != nil {
				return shaErr
			}
			if headErr != nil {
				return headErr
			}

			baseline, hit, err := gitstate.ReadBaseline(ctx, dir, sha)
			if err != nil {
				return err
			}
			if !hit {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "no cached baseline for %s — computing one now (first PR against this main commit pays this cost)\n", sha); err != nil {
					return err
				}
				baseline, err = computeBaselineAt(ctx, cmd, dir, sha, cfg)
				if err != nil {
					return err
				}
				// Never cache a baseline that measured nothing. Compute silently
				// omits any language whose tooling it couldn't run, so a runner
				// missing (say) cargo-llvm-cov produces an empty report — and
				// caching it makes every later PR hit a "valid" baseline of
				// nothing, report every language as [NEW], and gate on nothing at
				// all, silently and permanently. That is exactly what happened to
				// wardnet: nine baselines on its bulwark-state branch, every one
				// of them `{}`. A run that measured nothing has learned nothing,
				// so there is nothing worth remembering; recomputing next time is
				// the strictly better failure mode.
				if len(baseline) == 0 {
					if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "warning: computed no coverage at all for %s — not caching it as a baseline. The gate cannot compare against a baseline of nothing; fix the missing tooling above and it will recompute.\n", sha); err != nil {
						return err
					}
				} else if err := gitstate.WriteBaseline(ctx, dir, sha, baseline); err != nil {
					// Caching is otherwise best-effort: a write failure (worktree
					// race, disk pressure, transient git error) must never fail this
					// command outright — `current` and `baseline` are already
					// computed, and diffReport below is what actually matters.
					if _, printErr := fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to cache coverage baseline for %s: %v\n", sha, err); printErr != nil {
						return printErr
					}
				}
			}

			aggregateErr := diffReport(cmd, current, baseline, ecosystems)
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
func computeBaselineAt(ctx context.Context, cmd *cobra.Command, dir, sha string, cfg config.Config) (map[string]float64, error) {
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
	if err != nil {
		return nil, err
	}
	// The baseline worktree is where measurement most often fails unnoticed: it
	// is a bare checkout with no node_modules, no CI-staged report, and only
	// whatever tooling the runner happens to have. Name what went unmeasured
	// here, or the failure surfaces later as the far more confusing "no
	// baseline yet" against a bulwark-state branch that visibly has files in it.
	detected, err := detect.Ecosystems(tmp, cfg.AllExcludes())
	if err != nil {
		return nil, err
	}
	ecosystems := enabledEcosystems(detected, cfg)
	if err := warnUnmeasured(cmd, ecosystems, report, "at "+sha[:min(8, len(sha))]); err != nil {
		return nil, err
	}
	// The bare worktree is the write path MOST likely to be partial (no
	// node_modules, no CI-staged reports, only whatever tooling the runner
	// has), and a partial baseline cached here poisons every later PR against
	// this SHA just as thoroughly as a partial record-on-main would — so it
	// gets the same carry-forward, not just the record-on-main path.
	record, _, err := carryForwardBaseline(ctx, cmd, dir, sha, ecosystems, report)
	if err != nil {
		return nil, err
	}
	return record, nil
}

// formatReport renders a coverage report as a stable, sorted one-liner
// ("go: 58.5%, rust: 85.7%") for the baseline-recorded message on main. Sorted
// so the line doesn't reshuffle between runs over Go's map iteration order.
func formatReport(report map[string]float64) string {
	langs := make([]string, 0, len(report))
	for lang := range report {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	parts := make([]string, 0, len(langs))
	for _, lang := range langs {
		parts = append(parts, fmt.Sprintf("%s: %.1f%%", lang, report[lang]))
	}
	return strings.Join(parts, ", ")
}

// warnUnmeasured prints a warning for every ecosystem bulwark detected in dir
// but produced no coverage number for. coverage.Compute drops such a language
// silently — deliberately, since a repo with no coverage tooling shouldn't hard
// fail — but silent omission is also how a whole ecosystem can vanish from the
// gate without anyone noticing. Saying so out loud costs nothing and is the
// difference between "rust is unmeasured because cargo-llvm-cov isn't
// installed" and a mystery.
func warnUnmeasured(cmd *cobra.Command, ecosystems []detect.Ecosystem, report map[string]float64, where string) error {
	for _, lang := range unmeasuredLanguages(ecosystems, report) {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s detected %s, but measured no coverage for it — its coverage tooling is missing or failed, so it is absent from the gate\n", where, lang); err != nil {
			return err
		}
	}
	return nil
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
// language's coverage regressed. A language with no baseline entry is [NEW];
// one the baseline has but this run didn't measure is [UNMEASURED] while its
// source is still detected (its coverage step just didn't run) and [DROPPED]
// once it isn't (the source actually left the tree). None of those fail the
// check on its own — only a measured decrease does.
func diffReport(cmd *cobra.Command, current, baseline map[string]float64, detected []detect.Ecosystem) error {
	detectedSet := make(map[string]bool, len(detected))
	for _, e := range detected {
		detectedSet[string(e)] = true
	}
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
		// A language still detected in the tree but absent from this run's
		// measurements isn't gone — its coverage step just didn't run (a
		// path-filtered CI job, missing tooling). Say that, and reserve
		// DROPPED for a language whose source actually left the tree.
		case !curOK && detectedSet[lang]:
			tag, detail = "UNMEASURED", fmt.Sprintf("%s: not measured this run (baseline %.1f%%)", lang, base)
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

// printNoCoverage reports a run that measured nothing and — on main — had no
// prior baseline entries to carry forward either: there is nothing to gate
// and nothing worth recording.
func printNoCoverage(cmd *cobra.Command, mode coverage.Mode) error {
	msg := "no coverage measured — no coverage tooling detected/available for any ecosystem"
	if mode == coverage.ModeSkip {
		msg += " (--tests=skip only reads an existing report — did an earlier CI step produce one at the expected path?)"
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), msg)
	return err
}

// enabledEcosystems drops languages disabled in .bulwark.yml from the
// detected set. For the coverage gate, `enabled: false` means "stop gating
// this language", so it must behave exactly like source removal: undetected,
// its baseline entry dies on the next record instead of being carried
// forward (and [UNMEASURED]-reported) forever.
func enabledEcosystems(detected []detect.Ecosystem, cfg config.Config) []detect.Ecosystem {
	var out []detect.Ecosystem
	for _, e := range detected {
		enabled := true
		switch e {
		case detect.Rust:
			enabled = cfg.Rust.Enabled
		case detect.TypeScript:
			enabled = cfg.TypeScript.Enabled
		case detect.Go:
			enabled = cfg.Go.Enabled
		}
		if enabled {
			out = append(out, e)
		}
	}
	return out
}

// unmeasuredLanguages returns, sorted, every detected language the report has
// no entry for. It is the single source of truth for "detected but not
// measured this run" — the unmeasured warning, the carry-forward, and the
// missing-entry warning all key off this one predicate so they cannot drift.
func unmeasuredLanguages(detected []detect.Ecosystem, report map[string]float64) []string {
	var out []string
	for _, e := range detected {
		if _, measured := report[string(e)]; !measured {
			out = append(out, string(e))
		}
	}
	sort.Strings(out)
	return out
}

// mergeCarried returns a copy of current with each unmeasured language filled
// from prior where possible: carried lists what was filled, missing what no
// prior baseline had. Measured values are never overwritten.
func mergeCarried(current map[string]float64, unmeasured []string, prior map[string]float64) (map[string]float64, []string, []string) {
	if len(unmeasured) == 0 {
		return current, nil, nil
	}
	record := make(map[string]float64, len(current)+len(prior))
	maps.Copy(record, current)
	var carried, missing []string
	for _, lang := range unmeasured {
		if val, ok := prior[lang]; ok {
			record[lang] = val
			carried = append(carried, lang)
		} else {
			missing = append(missing, lang)
		}
	}
	return record, carried, missing
}

// carryForwardBaseline returns the report to record as sha's baseline: every
// measured value, plus — for each detected-but-unmeasured language — its
// entry from the nearest prior baseline that has one (starting at sha itself,
// so a re-run or concurrent job's fresher same-commit entry beats any
// ancestor's). A partial run (a path-filtered CI job, a bare baseline
// worktree missing tooling) must not shrink the baseline: recording only what
// was measured silently drops the unmeasured language from every later PR's
// gate. An undetected language is never carried — its source left the tree
// (or it was disabled in .bulwark.yml), so its entry dies with it. Anything
// that stayed unfilled is named on stderr rather than dropped in silence.
func carryForwardBaseline(ctx context.Context, cmd *cobra.Command, dir, sha string, detected []detect.Ecosystem, current map[string]float64) (map[string]float64, []string, error) {
	unmeasured := unmeasuredLanguages(detected, current)
	if len(unmeasured) == 0 {
		return current, nil, nil
	}
	prior := gitstate.PriorBaselines(ctx, dir, sha, unmeasured, priorBaselineDepth)
	record, carried, missing := mergeCarried(current, unmeasured, prior)
	for _, lang := range missing {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s is detected but was not measured this run, and no prior baseline entry was found to carry forward — recording a baseline without it (if this repo has prior baselines, is the checkout shallow? fetch-depth: 0 lets bulwark walk prior main commits)\n", lang); err != nil {
			return nil, nil, err
		}
	}
	return record, carried, nil
}
