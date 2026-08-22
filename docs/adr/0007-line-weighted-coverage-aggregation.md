# A language's coverage is its units' summed line counts, plus a per-unit floor

`internal/coverage` reduces a language's per-unit measurements to one figure by
summing counts — `Σ covered / Σ total` across every discovered Go module, Rust
crate/workspace root and TypeScript package — rather than by taking the mean of
the units' percentages. Each unit is therefore weighted by its size. The
per-unit measurement functions return a `LineCount` (Go counts statements,
because that is what a Go profile records; the ratio is the same quantity) and
`Compute` returns the `Unit` list alongside the per-language percentages.

ADR 0001 and ADR 0002 decided that Rust and Go coverage must span every
discovered crate and module, and each chose the mean because it was the obvious
reduction and because the repos in front of them had one unit. This is the
document that contradicts that half of them.

The mean is not a mild approximation. On `pedromvgomes/hatua`, a nine-package
pnpm monorepo, one commit added a 39-line untested file to `apps/playground`
(230 lines) while adding ~1,250 well-tested lines elsewhere:

| Aggregation | baseline | after | reported change |
| --- | --- | --- | --- |
| Unweighted mean | 85.31% | 83.13% | **−2.2** |
| `Σ covered / Σ total` | 94.18% | 94.62% | **+0.44** |

The gate failed a change that improved line coverage, and it failed it by five
times the true magnitude in the opposite direction. `packages/expressions` is
4,494 lines and `apps/playground` is 230, and the mean gave them equal votes.
Widening `coverage.tolerance` to hide this was rejected outright: its whole
purpose is absorbing sub-tenth instrumentation noise, and a tolerance wide
enough to swallow a 2.2-point artefact swallows a genuine two-point regression
with it.

## What line-weighting costs, and `coverage.floor`

Weighting by lines is deliberately blind to a small unit nobody tests. In that
same repo `packages/layout` has zero tests — 0 of 8 lines. Under the mean that
cost about 11 points and was most of why the baseline read 85% rather than 94%;
under line-weighting it is 8 lines in 8,305, so the headline moves by 0.1% and
the gate says nothing at all about a package with no tests.

So the old number was accidentally answering a second, different question, and
losing it silently would have been the worse trade:

* *"Did this change leave code untested?"* → `Σ covered / Σ total`
* *"Is there a unit nobody tests at all?"* → the minimum per-unit percentage

`coverage.floor` in `.bulwark.yml` is the second question, stated directly.
It is the minimum percentage any single measured unit must reach, it defaults
to `0` (off), and `cmd/bulwark/coverage.go`'s `floorReport` prints one `[FAIL]`
line per unit below it. Opt-in, because upgrading bulwark must not start failing
a repo over a gap it has always had.

Three properties follow from a floor having no baseline, and all three separate
it from the other two gates:

* **No ratchet.** A floor is an absolute standard; comparing it against a prior
  value would make a unit that has never had tests permanently acceptable,
  which is the gap it exists to close.
* **It runs on a push to main.** The aggregate and patch gates compare against
  a baseline, and on main the current commit *is* that baseline, so they have
  nothing to compare and return early. A floor compares against a number the
  repo stated once, which reads the same on either side — and skipping it there
  would leave main ungated on a unit that arrived through a path no pull request
  measured. The baseline is still recorded first, and unconditionally.
* **A unit whose report is missing is `[UNMEASURED]`, not passing and not 0%.**
  A per-unit gate that quietly covers only the units that happened to measure is
  the same silent-pass failure this repo already fixed once for patch coverage.

## Cached baselines are invalidated by moving them, not by reinterpreting them

Every entry on a consumer's `bulwark-state` branch was recorded under the mean.
Comparing today's line-weighted figure against one of those compares two
different quantities, and in the repo above the difference is +9 points — large
enough to read as a step change in either direction depending on which way a
repo's small units lean.

Baselines therefore move to `v2/<key>.json` on the same branch
(`gitstate.StatePath`). Every consumer takes one clean cache miss and re-records
under the new metric. The alternatives were both worse: a marker inside the file
would have turned the entries from a plain language → percentage object into a
wrapper, losing the property that they can be read by hand on the branch; and
leaving the path alone would have every consumer silently compare across a
metric change for at least one PR. Moving rather than deleting also leaves the
old entries in place to be inspected.

Release notes must say this, because the step change is visible whatever bulwark
does about it.
