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

**Linter**:
The engine backing bulwark's TypeScript check: Biome, and only Biome. `typescript.linter` in `.bulwark.yml` accepts `biome` alone; the retired `eslint` value is rejected with an error rather than accepted and quietly run under Biome.
_Note_: the TypeScript check gates on **correctness** as well as security, so it is not "security findings only" the way the other language checks are. The ESLint stack it replaced covered a different set of security rules — Node/backend heuristics with no Biome equivalent — so this is a change in what is gated, not only in what runs it. See [ADR 0008](docs/adr/0008-biome-as-the-only-typescript-linter.md).
_Avoid_: "linting mode", "the TS linter setting", describing Biome as opt-in.

**Pin**:
The exact version of a tool bulwark installs and runs, recorded in a real package-manager manifest (`package.json`, `Cargo.toml`, `go.mod`, `requirements.txt`) so Dependabot can see and bump it — never only in a Go constant. The distinguishing property of a pin is that something must be able to *age it out*: a pin nothing can bump is indistinguishable from a scanner that has silently stopped being current.
_Avoid_: "vendored version", "bundled version" (bulwark vendors no tool source; it installs pinned releases).

## Flagged ambiguities
**"Threshold"** — the original patch-coverage feature request proposed a `coverage.patch.threshold` config (an arbitrary fixed percentage per language). This was superseded: patch coverage has no independent threshold — it always gates against the aggregate baseline (see **Patch coverage**). Only an `enabled: bool` (default `true`, opt-out) remains in config.

## Example dialogue
> **Dev:** If a PR adds 9 new lines with 0% patch coverage, does bulwark fail it?
> **Domain expert:** Only if the aggregate baseline for that language is above 0% — patch coverage gates against baseline, not a fixed number. If the language has no baseline yet (first time seen), it's reported informationally and doesn't fail, same as aggregate coverage's `[NEW]` case.
> **Dev:** What about a changed comment line — does that count against patch coverage?
> **Domain expert:** No — it's never a coverable line, so it's excluded automatically once we intersect changed lines with what the coverage tool actually reports.
