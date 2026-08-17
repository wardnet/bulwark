package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
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
// prior baseline — to carry a detected-but-unmeasured language forward from,
// and to anchor within-tolerance dips to (withToleratedDipsRestored).
// With the carry-forward rule applied on every recorded baseline, the nearest
// one already contains everything worth carrying, so this only needs to span
// gaps (main commits whose CI never recorded), not real history.
const priorBaselineDepth = 50

func newCoverageCmd() *cobra.Command {
	var dir string
	var sourceFlag string
	var testsMode string
	var goReport []string
	var rustReport []string
	var rustLCOVReport []string
	cmd := &cobra.Command{
		Use:   "coverage",
		Short: "Diff current coverage against the bulwark-state baseline for the PR's base commit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, err := config.Load(dir)
			if err != nil {
				return err
			}
			source, err := resolveSource(cmd, sourceFlag, testsMode, cfg.Coverage.Source)
			if err != nil {
				return err
			}
			reports := resolveReports(cfg, goReport, rustReport, rustLCOVReport)

			patchWanted := coverage.PatchWanted{
				Go:         cfg.Coverage.Patch.Go.Enabled,
				Rust:       cfg.Coverage.Patch.Rust.Enabled,
				TypeScript: cfg.Coverage.Patch.TypeScript.Enabled,
			}

			// Detection runs before Compute, not after, because the toolchain
			// each ecosystem needs has to be in place before Compute shells
			// out to `go test` / `cargo llvm-cov` / a package's
			// test:coverage. Compute detects again internally; the walk is
			// cheap and idempotent, and duplicating it is the smaller cost
			// against threading a pre-detected list through its signature.
			//
			// This also covers the SourceReport path, which executes no tests
			// but still needs `go list -m` to resolve a profile's module
			// paths.
			detected, err := detect.Ecosystems(dir, cfg.AllExcludes())
			if err != nil {
				return err
			}
			ecosystems := enabledEcosystems(detected, cfg)
			if err := ensureToolchains(ctx, cmd, dir, cfg, ecosystems); err != nil {
				return err
			}

			current, sources, cleanup, err := coverage.Compute(ctx, dir, cfg, source, reports, patchWanted)
			defer cleanup()
			if err != nil {
				return err
			}
			// A partially-measured current tree is just as invisible as an
			// unmeasured one: the language simply doesn't appear in the report.
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
			// tests itself (exactly the case `coverage.source: report` exists to
			// serve). Such a repo can never recompute a historical baseline:
			// computeBaselineAt's
			// throwaway worktree is a bare checkout with none of the toolchain or
			// staged reports the pipeline provides, so it measures nothing. wardnet
			// hit precisely that — and the numbers it failed to reconstruct in a
			// worktree were numbers it had already measured, and thrown away, when
			// this same command ran on main. Recording them costs nothing: no
			// re-run, no cargo-llvm-cov, no yarn — they are already in hand.
			//
			// A main run that measured NOTHING (a docs-only merge: every
			// coverage producer path-filtered away, no reports for a
			// report-sourced repo to read) still records — the carry-forward fills
			// every detected language from the nearest prior baseline. The
			// old early-return here left such a commit with no baseline at
			// all, so the first PR against it recomputed nothing, reported
			// every language as [NEW], and gated on nothing
			// (wardnet/wardnet#899).
			head, headErr := gitstate.HeadSHA(ctx, dir)
			if shaErr == nil && headErr == nil && head == sha {
				record, carried, err := carryForwardBaseline(ctx, cmd, dir, sha, ecosystems, current, cfg.Coverage.Tolerance)
				if err != nil {
					return err
				}
				if len(record) == 0 {
					return printNoCoverage(cmd, source)
				}
				// Keyed by tree. The commit is incidental; the tree is what the
				// coverage describes, and it is the identifier this commit shares
				// with the pull request that produced it.
				key := sha
				if t, err := gitstate.TreeSHA(ctx, dir, sha); err == nil {
					key = t
				}
				if err := gitstate.WriteBaseline(ctx, dir, key, record); err != nil {
					// Best-effort, as everywhere else: a write race with a concurrent
					// main build must not fail the build.
					_, printErr := fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to record coverage baseline for %s: %v\n", key, err)
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
				return printNoCoverage(cmd, source)
			}
			if shaErr != nil {
				return shaErr
			}
			if headErr != nil {
				return headErr
			}

			// Tree first, commit second. The tree of the base commit is the tree
			// some earlier pull request already measured and recorded — a squash
			// merge lands the merged tree verbatim — so the number is usually
			// already there. The SHA key is only for baselines written before
			// this, so existing state keeps resolving.
			baseTree, treeErr := gitstate.TreeSHA(ctx, dir, sha)
			if treeErr != nil {
				baseTree = ""
			}
			baseline, hit, err := gitstate.ReadBaseline(ctx, dir, baseTree, sha)
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
				} else if err := gitstate.WriteBaseline(ctx, dir, firstNonEmpty(baseTree, sha), baseline); err != nil {
					// Caching is otherwise best-effort: a write failure (worktree
					// race, disk pressure, transient git error) must never fail this
					// command outright — `current` and `baseline` are already
					// computed, and diffReport below is what actually matters.
					if _, printErr := fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to cache coverage baseline for %s: %v\n", sha, err); printErr != nil {
						return printErr
					}
				}
			}

			// Record what this run measured, keyed by the tree it measured.
			//
			// On a pull request HEAD is refs/pull/N/merge, and a squash merge
			// lands a commit with exactly that tree — verified on a real merge:
			// the tree measured on the PR and the tree of the squash commit are
			// the same object. So this IS the baseline for the commit this pull
			// request is about to become, already measured, by the pipeline that
			// knows how to measure it.
			//
			// Without this the number is thrown away and the next pull request
			// falls back to computeBaselineAt, which measures a bare worktree
			// with none of the toolchain or staged reports the pipeline provides
			// — for a `coverage.source: report` repository that is the wrong
			// number, or none at all.
			//
			// Best-effort, like every other write here: a failure must not fail
			// the gate that is about to run.
			if len(current) > 0 {
				if headTree, err := gitstate.TreeSHA(ctx, dir, "HEAD"); err == nil {
					if _, hit, _ := gitstate.ReadBaseline(ctx, dir, headTree); !hit {
						if err := gitstate.WriteBaseline(ctx, dir, headTree, current); err != nil {
							if _, printErr := fmt.Fprintf(cmd.ErrOrStderr(),
								"warning: failed to record this tree's coverage for %s: %v\n", headTree, err); printErr != nil {
								return printErr
							}
						}
					}
				}
			}

			aggregateErr := diffReport(cmd, current, baseline, cfg.Coverage.Tolerance, ecosystems)
			// sha is the same merge-base gitstate.BaseSHA already resolved for
			// the aggregate baseline lookup above — patch coverage reuses it
			// directly rather than recomputing "git merge-base HEAD origin/main"
			// a second time.
			patchErr := patchReport(cmd, ctx, dir, patchWanted, sources, sha, baseline, cfg.Coverage.Patch.Tolerance, ecosystems)
			// errors.Join keeps both messages when aggregate AND patch coverage
			// regress in the same run — AGENTS.md's documented "compute and gate
			// on both, not either/or" contract must hold for the returned error
			// too, not just for what gets printed to stdout above.
			return errors.Join(aggregateErr, patchErr)
		},
	}
	// --dir stays a flag and cannot move into .bulwark.yml: the file lives AT
	// the scan root, so bulwark has to know the root before it can read its
	// own config. Everything else about coverage production now has a home in
	// that file, and the flags below are the local-dev/one-off escape hatch.
	cmd.Flags().StringVar(&dir, "dir", ".", "repository root")
	cmd.Flags().StringVar(&sourceFlag, "source", "",
		`who produces the coverage data: "run" (bulwark executes each ecosystem's test suite
itself) or "report" (a prior CI job already produced one; bulwark only parses it). Defaults to
coverage.source in .bulwark.yml, which is where this normally belongs — it's a property of how
the repo's pipeline is built, not of one invocation. Falls back to "run" with no file.`)
	cmd.Flags().StringVar(&testsMode, "tests", "",
		`deprecated alias for --source: "run" maps to --source=run, "skip" to --source=report`)
	if err := cmd.Flags().MarkDeprecated("tests", `use --source ("skip" is now "report"), or set coverage.source in .bulwark.yml`); err != nil {
		panic(err) // only errors on a flag name that doesn't exist, which is a programming error
	}
	cmd.Flags().StringArrayVar(&goReport, "go-report", nil,
		`path (relative to --dir) to an existing go coverage profile; only used with --source=report.
Overrides coverage.go.report in .bulwark.yml, which is the usual home for it. Repeatable. A bare
path applies only when exactly one Go module is discovered under --dir; for multiple modules,
disambiguate with "<moduleDir>=<path>" (moduleDir relative to --dir), e.g.
--go-report wctl=coverage/wctl.out. Default per module: search coverage.out, cover.out, c.out
relative to that module's directory`)
	cmd.Flags().StringArrayVar(&rustReport, "rust-report", nil,
		`path (relative to --dir) to an existing cargo-llvm-cov JSON export; only used with --source=report.
Overrides coverage.rust.report in .bulwark.yml. Repeatable. A bare path applies only when exactly one
Rust crate/workspace is discovered under --dir; for multiple crates, disambiguate with
"<crateDir>=<path>" (crateDir relative to --dir), e.g.
--rust-report daemon=daemon/coverage/daemon-llvm-cov.json. Default per crate: search
coverage/llvm-cov.json, llvm-cov.json, target/llvm-cov/llvm-cov.json relative to that crate's directory`)
	cmd.Flags().StringArrayVar(&rustLCOVReport, "rust-lcov-report", nil,
		`path (relative to --dir) to an existing cargo-llvm-cov lcov export, used for Rust patch coverage;
only used with --source=report. Overrides coverage.rust.lcov in .bulwark.yml. Repeatable, same
bare-vs-"<crateDir>=<path>" syntax as --rust-report.
Default per crate: search coverage/lcov.info, lcov.info, target/llvm-cov/lcov.info relative to that
crate's directory`)
	return cmd
}

// resolveSource settles who produces coverage for this invocation, in
// descending precedence: --source, then the deprecated --tests, then
// coverage.source in .bulwark.yml, which config.Default() already resolved to
// SourceRun when the file is absent or silent.
//
// Both flags default to "" rather than to "run" so that "unset" is
// distinguishable from "explicitly asked for run". With a "run" default the
// flag would always be populated, always outrank the file, and
// coverage.source would never once be consulted — the failure would be
// silent, and it would look exactly like the config key not working.
func resolveSource(cmd *cobra.Command, sourceFlag, testsMode string, fromFile config.Source) (coverage.Source, error) {
	switch {
	case sourceFlag != "":
		source := coverage.Source(sourceFlag)
		if source != coverage.SourceRun && source != coverage.SourceReport {
			return "", fmt.Errorf("--source must be %q or %q, got %q", coverage.SourceRun, coverage.SourceReport, sourceFlag)
		}
		if testsMode != "" {
			if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning: both --source and the deprecated --tests were given; --source wins"); err != nil {
				return "", err
			}
		}
		return source, nil
	case testsMode != "":
		// The old vocabulary, kept working for anyone driving the binary
		// directly. "skip" and "report" name the same behavior — never
		// execute, only parse — from the two ends of the same decision.
		switch testsMode {
		case "run":
			return coverage.SourceRun, nil
		case "skip":
			return coverage.SourceReport, nil
		default:
			return "", fmt.Errorf(`--tests must be "run" or "skip", got %q`, testsMode)
		}
	default:
		// config.Load already rejected anything that isn't one of the two
		// values, so this needs no second validation pass.
		return coverage.Source(fromFile), nil
	}
}

// resolveReports merges the report-path flags over .bulwark.yml's
// coverage.{go,rust} sections. Precedence is per language and per kind, not
// per key: passing --rust-report replaces the file's rust.report wholesale
// rather than merging entry-by-entry into it. Merging would make a flag
// unable to *remove* a stale keyed entry the file declares, so the override
// could only ever add — and "the flag I passed didn't take effect" is a worse
// failure than having to restate the handful of paths a repo has.
func resolveReports(cfg config.Config, goReport, rustReport, rustLCOVReport []string) coverage.ReportPaths {
	pick := func(flag []string, file config.Reports) coverage.ReportOverrides {
		if len(flag) > 0 {
			return parseReportOverrides(flag)
		}
		if len(file) == 0 {
			return nil
		}
		return coverage.ReportOverrides(file)
	}
	return coverage.ReportPaths{
		Go:       pick(goReport, cfg.Coverage.Go.Report),
		Rust:     pick(rustReport, cfg.Coverage.Rust.Report),
		RustLCOV: pick(rustLCOVReport, cfg.Coverage.Rust.LCOV),
	}
}

// parseReportOverrides parses repeated --go-report/--rust-report/
// --rust-lcov-report flag values into a coverage.ReportOverrides map. Each
// value is either a bare path (stored under the "" key, consulted only when
// discovery finds exactly one crate/module — preserving the original
// single-unit CLI usage unchanged) or a "<unitDir>=<path>" pair
// disambiguating which discovered directory (relative to --dir) the override
// applies to.
func parseReportOverrides(values []string) coverage.ReportOverrides {
	if len(values) == 0 {
		return nil
	}
	overrides := make(coverage.ReportOverrides, len(values))
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
// own working tree/branch.
//
// This always actually runs tests (coverage.SourceRun), regardless of
// coverage.source or --source: a historical commit's checkout has no
// pre-existing CI-produced report to find — the report itself would have to
// come from actually running the suite at that commit — so there is no
// report-sourced equivalent for baseline computation. Passing the repo's own
// coverage.source through here instead would hand every report-sourced repo
// an empty baseline forever, which is the `{}` poisoning the caller then
// refuses to cache; the gate would silently compare against nothing. This is
// a one-time cost per main commit (cached afterward on the bulwark-state
// branch), not a per-invocation cost, so it doesn't reintroduce the
// duplicate-test-run problem `coverage.source: report` exists to avoid.
// firstNonEmpty returns the first non-empty string, so a tree lookup that
// failed falls back to the commit SHA rather than writing an empty key.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

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
	report, _, cleanup, err := coverage.Compute(ctx, tmp, cfg, coverage.SourceRun, coverage.ReportPaths{}, coverage.PatchWanted{})
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
	record, _, err := carryForwardBaseline(ctx, cmd, dir, sha, ecosystems, report, cfg.Coverage.Tolerance)
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

// regressedBeyond reports whether cur dipped below base by more than
// tolerance percentage points, comparing at the report's display precision
// (tenths) so what's shown and what's gated always agree: a raw float64
// subtraction decides an exactly-at-tolerance dip by representation noise
// (86.2-86.1 exceeds 0.1 but 86.1-86.0 does not), failing one "regressed
// 0.1%" while passing an identical-looking other. Shared by diffReport and
// patchReport so the two gates can't drift apart on the comparison, like
// statusPrefix does for formatting.
func regressedBeyond(cur, base, tolerance float64) bool {
	return math.Round((base-cur)*10) > math.Round(tolerance*10)
}

// patchReport prints one bracketed status line per language whose patch
// coverage was requested (cfg.Coverage.Patch.<lang>.Enabled) and whose
// source Compute managed to resolve, gating patch% against that language's
// aggregate baseline (patch coverage has no baseline of its own — see
// CONTEXT.md). A language with no baseline yet is reported informationally,
// not failed, mirroring diffReport's [NEW] handling. A language with zero
// coverable changed lines (the diff only touched comments/imports) is
// skipped entirely — there's nothing to gate on.
//
// A detected language whose files the diff DID touch but whose per-line
// source couldn't be resolved is reported as [UNMEASURED], not skipped in
// silence: a silent skip here reads as "patch coverage passed" in the PR
// comment, when in fact it never ran — wardnet/wardnet#957 shipped a green
// bulwark comment that way while Codecov, fed the very same lcov export,
// failed the diff's patch coverage. Like diffReport's [UNMEASURED], it never
// fails the gate on its own; a stderr warning names the wiring that's
// missing.
//
// ChangedLines is called exactly once, for the union of every wanted
// language's extensions, then partitioned per language — not once per
// language — since all three langs diff the identical mergeBase..HEAD range.
func patchReport(cmd *cobra.Command, ctx context.Context, dir string, want coverage.PatchWanted, sources coverage.PatchSources, mergeBase string, baseline map[string]float64, tolerance float64, detected []detect.Ecosystem) error {
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

	detectedSet := make(map[string]bool, len(detected))
	for _, e := range detected {
		detectedSet[string(e)] = true
	}

	regressed := 0
	for _, lang := range langs {
		if !lang.wanted {
			continue
		}
		langChanged := filterByExt(changed, lang.exts)

		var hit, total int
		resolved := true
		switch lang.name {
		case "go":
			if len(sources.GoProfiles) == 0 {
				resolved = false
				break
			}
			hit, total = goPatchPercent(dir, sources.GoProfiles, langChanged)
		case "rust":
			if len(sources.RustLCOV) == 0 {
				resolved = false
				break
			}
			hit, total = rustPatchPercent(dir, sources.RustLCOV, langChanged)
		case "typescript":
			if len(sources.TSLCOV) == 0 {
				resolved = false
				break
			}
			hit, total = tsPatchPercent(dir, sources.TSLCOV, langChanged)
		}
		// The unmeasured report is scoped to detected ecosystems: patch gates
		// default to enabled for all three languages regardless of what the
		// repo actually contains, and a stray .rs file changed in (say) a pure
		// Go repo shouldn't produce a rust line.
		if !resolved {
			if detectedSet[lang.name] {
				if err := reportPatchUnmeasured(cmd, lang.name, langChanged); err != nil {
					return err
				}
			}
			continue
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
		// The patch gate has its own tolerance knob
		// (coverage.patch.tolerance) — deliberately not coverage.tolerance,
		// so loosening the noisy aggregate gate never weakens this one.
		case regressedBeyond(pct, base, tolerance):
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

// patchSourceHints names, per language, the wiring that gets a per-line
// coverage source resolved — what the stderr warning points at when the
// patch gate had changed lines to measure but nothing to measure them with.
var patchSourceHints = map[string]string{
	"go":         "the same Go profile the aggregate gate reads (--go-report, or coverage.out/cover.out/c.out) also feeds patch coverage — if aggregate Go coverage was measured this run, this shouldn't happen",
	"rust":       "pass --rust-lcov-report, or place a cargo-llvm-cov lcov export at coverage/lcov.info in the crate directory",
	"typescript": "enable an lcov reporter in each package's coverage config so coverage/lcov.info is written alongside coverage-summary.json",
}

// reportPatchUnmeasured surfaces a patch gate that had changed lines to
// measure but no per-line source to measure them with: an [UNMEASURED] line
// on stdout (so it lands in the PR comment next to the aggregate result, via
// action.yml's generic bracketed-tag grep) and a stderr warning naming the
// missing wiring. A diff that touched none of the language's files stays
// silent — nothing was skipped, there was simply nothing to gate.
func reportPatchUnmeasured(cmd *cobra.Command, lang string, changed map[string][]int) error {
	if len(changed) == 0 {
		return nil
	}
	lines := 0
	for _, l := range changed {
		lines += len(l)
	}
	detail := fmt.Sprintf("%s patch: not measured (%d changed line(s) across %d file(s), but no per-line coverage source was resolved)", lang, lines, len(changed))
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), statusPrefix("UNMEASURED")+detail); err != nil {
		return err
	}
	_, err := fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s patch coverage is enabled and this diff touches %s files, but no per-line coverage source was resolved — %s\n", lang, lang, patchSourceHints[lang])
	return err
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

// goPatchPercent merges every discovered Go module's per-line hits into one
// set and scores the changed lines against it. A module whose profile can't
// be parsed is skipped rather than failing the gate, matching how the Rust
// path treats an unreadable lcov — the remaining modules still get measured.
//
// The per-crate prefix scoping rustPatchPercent needs doesn't apply here:
// ParseGoProfile already returns --dir-relative paths (it puts each module's
// own directory back on), so two modules cannot collide on one key.
func goPatchPercent(dir string, goProfiles map[string]coverage.GoModuleProfile, changed map[string][]int) (hit, total int) {
	merged := coverage.LineHits{}
	for _, src := range goProfiles {
		hits, err := coverage.ParseGoProfile(src, dir)
		if err != nil {
			continue
		}
		for file, lines := range hits {
			merged[file] = lines
		}
	}
	if len(merged) == 0 {
		return 0, 0
	}
	return coverage.PatchPercent(changed, merged)
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
// check on its own — only a measured decrease beyond the configured noise
// tolerance does (see regressedBeyond).
func diffReport(cmd *cobra.Command, current, baseline map[string]float64, tolerance float64, detected []detect.Ecosystem) error {
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
		// Dips within the configured noise tolerance don't count — see
		// config.Coverage.Tolerance for the rationale.
		case regressedBeyond(cur, base, tolerance):
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
func printNoCoverage(cmd *cobra.Command, source coverage.Source) error {
	msg := "no coverage measured — no coverage tooling detected/available for any ecosystem"
	if source == coverage.SourceReport {
		msg += " (coverage.source is \"report\", which only reads an existing report — did an earlier CI job produce one at the expected path?)"
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
func carryForwardBaseline(ctx context.Context, cmd *cobra.Command, dir, sha string, detected []detect.Ecosystem, current map[string]float64, tolerance float64) (map[string]float64, []string, error) {
	unmeasured := unmeasuredLanguages(detected, current)
	// Priors are fetched for every detected language, not just unmeasured
	// ones: measured values need them too, for the anti-ratchet restore
	// below.
	langs := make([]string, 0, len(detected))
	for _, e := range detected {
		langs = append(langs, string(e))
	}
	prior := gitstate.PriorBaselines(ctx, dir, sha, langs, priorBaselineDepth)
	record, carried, missing := mergeCarried(current, unmeasured, prior)
	record = withToleratedDipsRestored(record, prior, tolerance)
	for _, lang := range missing {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s is detected but was not measured this run, and no prior baseline entry was found to carry forward — recording a baseline without it (if this repo has prior baselines, is the checkout shallow? fetch-depth: 0 lets bulwark walk prior main commits)\n", lang); err != nil {
			return nil, nil, err
		}
	}
	return record, carried, nil
}

// withToleratedDipsRestored returns record with every measured value that
// dipped below its prior baseline by no more than the gate's tolerance
// restored to the prior value. Recording the dipped number verbatim would
// turn the tolerance into an unbounded downward ratchet: each merged PR may
// dip up to the tolerance and pass, the main run records the lower number as
// the new baseline, and the next PR gets another free dip from that lower
// floor — coverage bleeds one tolerance per merge with every gate green.
// Restoring within-tolerance dips anchors the baseline to its high-water
// mark, capping total tolerated drift at one tolerance. A dip beyond the
// tolerance is recorded as measured: it was FAIL-visible on the PR that
// introduced it, so accepting it on main is a deliberate reset, not leakage.
func withToleratedDipsRestored(record, prior map[string]float64, tolerance float64) map[string]float64 {
	out := maps.Clone(record)
	for lang, val := range out {
		if p, ok := prior[lang]; ok && val < p && !regressedBeyond(val, p, tolerance) {
			out[lang] = p
		}
	}
	return out
}
