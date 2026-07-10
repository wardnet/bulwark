# bulwark
bulwark unifies SAST/SCA/lint/coverage gating for Rust, TypeScript, and Go projects into one CLI, run identically locally and in CI.

## Language

**Aggregate coverage**:
The current whole-tree coverage percentage for one ecosystem, compared against a cached per-main-commit baseline (`bulwark-state` branch). Catches regressions in code the current PR never touches (e.g. a deleted test file).
_Avoid_: "total coverage", "overall coverage".

**Patch coverage**:
The coverage percentage of only the lines added/modified by the current PR (HEAD vs. merge-base), gated against that same ecosystem's aggregate baseline (`patch% >= baseline%`). Catches untested new code even when the codebase is too large for it to move the aggregate percentage. Computed alongside aggregate coverage, not instead of it — they catch disjoint regression classes.
_Avoid_: "diff coverage" (used interchangeably by other tools like Codecov, but this repo standardizes on "patch coverage").

**Baseline**:
The aggregate coverage value cached on the `bulwark-state` branch for a specific main-branch commit SHA, computed once per SHA. Both aggregate and patch coverage compare against this same value — patch coverage has no baseline concept of its own.

**Coverable line**:
A source line that a language's own coverage tool (`go tool cover`, `cargo llvm-cov`, Istanbul) reports an entry for. Comments, blank lines, imports, and braces are never coverable — they simply never appear in a coverage report, so patch coverage's denominator (coverable changed lines) excludes them automatically, without bulwark doing any language-aware filtering itself.

## Flagged ambiguities
**"Threshold"** — the original patch-coverage feature request proposed a `coverage.patch.threshold` config (an arbitrary fixed percentage per language). This was superseded: patch coverage has no independent threshold — it always gates against the aggregate baseline (see **Patch coverage**). Only an `enabled: bool` (default `true`, opt-out) remains in config.

## Example dialogue
> **Dev:** If a PR adds 9 new lines with 0% patch coverage, does bulwark fail it?
> **Domain expert:** Only if the aggregate baseline for that language is above 0% — patch coverage gates against baseline, not a fixed number. If the language has no baseline yet (first time seen), it's reported informationally and doesn't fail, same as aggregate coverage's `[NEW]` case.
> **Dev:** What about a changed comment line — does that count against patch coverage?
> **Domain expert:** No — it's never a coverable line, so it's excluded automatically once we intersect changed lines with what the coverage tool actually reports.
