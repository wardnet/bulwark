# bulwark — agent guide

Bulwark is a Go CLI that unifies code-quality and security scanning — SAST, SCA, linting, and
coverage gates — for Rust, TypeScript, and Go projects. It is the single entry point a developer
runs locally and CI runs identically, so "green locally" and "green in CI" can never drift apart.
It replaces per-repo ad hoc security workflows (CodeQL, standalone cargo-audit jobs, Codecov as a
blocking gate) across `wardnet`, `wardnet-cloud`, and `inforge` with one consistent pipeline.

## Commands

```sh
go build ./...                 # build the binary
go test -race ./...            # run tests
golangci-lint run ./...        # lint — must be clean before a PR
go run ./cmd/bulwark            # run the CLI locally

# Release build dry-run (produces dist/):
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean
```

## Layout

```
cmd/bulwark/                    # the bulwark CLI (scan, coverage, version, update)
internal/detect/                # ecosystem + TS-package detection (walks for Cargo.toml/package.json/go.mod)
internal/config/                # .bulwark.yml loading (opt-out only — see Configuration below)
internal/rust/                  # clippy, cargo-audit, cargo-deny
internal/typescript/            # self-contained pinned ESLint + eslint-plugin-security
internal/golang/                # gosec, govulncheck (installed into a version-keyed GOBIN dir)
internal/semgrep/                # pinned Semgrep, installed via pipx
internal/coverage/               # per-language coverage percentage (see Coverage below)
internal/gitstate/               # bulwark-state branch read/write (see Coverage below)
internal/executil/              # shared external-command runner every scanner package uses
assets/bulwark-logo.png         # logo — used by README and the action's PR comment (see below)
.goreleaser.yml                 # build/release config (v2 schema)
.golangci.yml                   # lint config (v2 schema)
.github/workflows/{ci,release}.yml
.github/dependabot.yml
action.yml                      # composite action: install + scan + coverage + PR comment + report
scripts/install.sh              # curl|sh installer shipped with every release
```

- Module path: `wardnet/bulwark` (not `github.com/wardnet/bulwark` — a deliberate deviation from
  the other repos in this org, to be applied there too later; do not "fix" this back).
- `bulwark` ships as a single statically-linked binary (`CGO_ENABLED=0`), built for
  linux/darwin × amd64/arm64.

## Status

All four subcommands (`scan`, `coverage`, `version`, `update`) are fully implemented — every check
is a real tool invocation (not a stub). Every scanner pins its own tool version and installs it into
a bulwark-managed cache directory rather than trusting whatever's already on the machine (see each
`internal/<lang>` package's doc comment for why). `update` follows the same pattern as `inforge`'s
self-update (checksum-verified binary replacement, refuses on dev builds, passive update nudge on
every other command). `bulwark coverage` has been verified end-to-end against this repo's own real
`bulwark-state` branch on GitHub, not just a local fixture.

## CI

`.github/workflows/ci.yml` runs three jobs on every push/PR to `main`: `lint` (golangci-lint),
`build & test` (`go build`/`go test -race`), and `self-scan` — bulwark builds itself and runs
`bulwark scan --dir .` against its own repo. `self-scan` is dogfooding, not a formality: it's the
only job that exercises the actual scan/report path end-to-end against a real repo, and it already
caught a real bug once (see the git history around the `go-version: "1.26.5"` pin below).

**Pin the exact Go patch version in workflows (`"1.26.5"`), never a bare minor (`"1.26"`).**
`actions/setup-go`'s `go-version: "1.26"` resolves to whatever `1.26.x` patch it has
cached/available, which is not necessarily the version this repo's `go.mod` `toolchain` directive
pins — and critically, `go install`-ing an *external* tool (gosec, govulncheck) does **not** consult
the current module's `go.mod` toolchain directive the way building the module itself does. This bit
us for real: `self-scan`'s `govulncheck` step passed locally (toolchain directive respected) but
failed in CI (setup-go had installed an older, vulnerable patch) until `go-version` was pinned to the
exact `1.26.5`. If `go.mod`'s `toolchain` line is ever bumped, update every `go-version:` in
`ci.yml`/`release.yml` to match in the same change.

## Configuration

`.bulwark.yml` at the scan root is optional and purely **opt-out** — its job is narrowing what
bulwark's zero-config default already does (scan everything detected, every check enabled), plus
one numeric knob (`coverage.tolerance`), not tuning severity or suppressing individual findings (that's what a fix-up pass + inline
`#nosec`/`nosemgrep` annotations in the scanned repo are for). See `internal/config/config.go` for
the full schema; shape:

```yaml
rust:
  enabled: true          # set false to skip Rust entirely even if a Cargo.toml is detected
  exclude: []            # extra directory names to skip during ecosystem/package detection
typescript:
  enabled: true
  exclude: ["legacy-app"]
  install: ""            # override coverage's install-command auto-detection, e.g.
                          # "corepack enable && yarn install --immutable" (see Coverage below)
go:
  enabled: true
  exclude: []
semgrep:
  enabled: true
  config: auto           # override to a custom registry ref/path if needed
coverage:
  tolerance: 0.1         # pp a language's aggregate coverage may dip below its baseline before
                          # the gate fails; absorbs sub-tenth measurement noise ("86.1% vs
                          # baseline 86.1%, regressed 0.0%"). Compared at display precision
                          # (tenths); 0 = fail any dip the report can show. Must be finite and
                          # non-negative — Load rejects anything else.
  patch:
    tolerance: 0.1       # the patch gate's own dip allowance — deliberately independent, so
                          # loosening the aggregate knob never weakens the untested-new-code check
```

Omitting the file, or omitting a section/key within it, keeps that value at its default — see
`internal/config/config_test.go` for the exact merge semantics.

## Coverage

`bulwark coverage` diffs the current branch's per-language coverage against a lazily-computed,
per-main-commit-SHA baseline cached on a dedicated `bulwark-state` branch (never `main` — bot-owned
generated cache data, not source, needs no PR/review and never pollutes main's history):

- **A run on main records its own coverage as that commit's baseline.** When `HeadSHA == BaseSHA`
  (bulwark is running on main, not ahead of it) there is nothing to gate against — the current commit
  *is* the baseline — so `cmd/bulwark/coverage.go` writes what it just measured to `bulwark-state`
  and stops. This is the *primary* way baselines get created, and the only one that works for a repo
  whose coverage is produced by a multi-job CI pipeline rather than by bulwark running the tests
  itself (precisely what `--tests=skip` exists to serve): such a repo can never *re*compute a
  historical baseline, because `computeBaselineAt`'s throwaway worktree is a bare checkout with none
  of the toolchain (`cargo-llvm-cov`, yarn/Node) or staged reports the pipeline provides — it
  measures nothing. wardnet ran that way for months: the numbers it kept failing to reconstruct in a
  worktree were numbers it had *already measured, and thrown away*, when this same command ran on
  main. Recording them costs nothing — no test re-run, no extra tooling, they are already in hand.
  **Consumers must therefore run `bulwark coverage` on pushes to main, not only on PRs.**
- **Baseline writes merge; a partial run never shrinks the baseline.** A consumer's CI may
  path-filter its coverage jobs (wardnet skips frontend coverage on a Rust-only change), and the
  cache-miss worktree routinely lacks tooling, so either write path can legitimately measure only
  some of the detected languages. Recording only what was measured would silently drop the
  unmeasured language's entry — every later PR would see it as `[NEW]`, compared against nothing,
  permanently, and the "never cache an empty baseline" guard doesn't catch it because the report
  isn't empty, just partial. So *both* writers (record-on-main and `computeBaselineAt`'s cache-miss
  path) run `cmd/bulwark/coverage.go`'s `carryForwardBaseline` first: it copies the entry for every
  *detected-but-unmeasured* language from the nearest prior baseline via `gitstate.PriorBaselines`,
  which starts at the recorded SHA **itself** (so a re-run or a concurrent per-language job never
  clobbers a fresher same-commit entry with an ancestor's stale value — and a shallow depth-1
  checkout can still see it) before walking first-parent ancestors, best-effort, skipping poisoned
  `{}` entries. A language that's genuinely gone — source deleted, or `enabled: false` in
  `.bulwark.yml` (`enabledEcosystems` strips disabled languages from the detected set) — is no
  longer *detected*, so its entry still dies with it; only "the code is there but this run didn't
  measure it" carries forward. The `recorded coverage baseline` line names anything carried, and an
  unmeasured language *no* prior had is named in a stderr warning (shallow history is the usual
  culprit) instead of vanishing silently. This applies even when a main run measures **nothing**
  (a docs-only merge: every producer path-filtered away, no reports for `--tests=skip` to read) —
  the whole baseline is carried rather than skipping the record, because a main commit with no
  baseline forces every PR against it into the recompute-nothing → all-`[NEW]` → gate-on-nothing
  hole (wardnet/wardnet#899). "No coverage measured" is only printed when there was truly nothing
  to record: nothing measured *and* no priors to carry.
- `internal/gitstate.BaseSHA` resolves `git merge-base HEAD origin/main`.
- `internal/gitstate.ReadBaseline` fetches `bulwark-state` and reads `<sha>.json` via `git show`
  (no checkout) — a missing branch or missing file is a cache miss, not an error.
- On a cache miss, `cmd/bulwark/coverage.go`'s `computeBaselineAt` checks out `origin/main` at that
  SHA into a throwaway `git worktree` (never disturbing the caller's own working tree/branch),
  computes coverage there, and `internal/gitstate.WriteBaseline` pushes it to `bulwark-state` (via
  another throwaway worktree — creating the branch as an orphan the first time). `bulwark-state` is
  shared and busy — every CI run on the repo may push to it — so `WriteBaseline` fetches the fresh
  remote ref immediately before staging each attempt (the local tracking ref is as stale as the
  job's checkout, minutes old by the time a scan finishes) and retries a rejected push from the
  re-fetched ref, treating "the fetched branch already has this exact content" as success. A push
  that never lands is returned as an error: the PR-side cache-miss caller downgrades it to a
  warning (it already holds the computed baseline), but the record-on-main path must never print
  "recorded" for a baseline that was lost — that exact silent loss (stale ref → non-fast-forward
  rejection → swallowed) is how wardnet's main runs kept recording nothing while every PR
  re-hit a cache miss.
- `internal/coverage.Compute` gets the actual number per detected ecosystem: `go tool cover -func`'s
  total line for Go, `cargo llvm-cov --json`'s `data[0].totals.lines.percent` for Rust, and — for
  TypeScript, best-effort only — a package's own `test:coverage` script plus Vitest/Istanbul's
  `coverage-summary.json`, since unlike a linter there's no single canonical coverage-invocation
  convention to standardize on across arbitrary TS packages. A language whose coverage can't be
  measured is silently omitted from the report, not failed.
- Rust never assumes `--dir` itself is the crate/workspace root — `internal/detect.RustCrateDirs`
  discovers every independent Cargo crate/workspace root under `--dir` (deduping a workspace
  member's own `Cargo.toml` under its ancestor workspace root), and both `internal/rust.Check` and
  `internal/coverage.rustCoverage` iterate every discovered root, averaging coverage across them the
  same way TypeScript averages across packages. `--rust-report`/`--rust-lcov-report` are therefore
  repeatable, keyed flags (`--rust-report <crateDir>=<path>`, crateDir relative to `--dir`) rather
  than a single path — a bare value (no `=`) is only honored when discovery finds exactly one crate,
  preserving the original single-crate invocation unchanged.
- **An empty baseline is never cached, and an empty cached baseline is a miss.** `Compute` silently
  omits any language whose tooling it can't run (deliberate — a repo with no coverage tooling
  shouldn't hard fail). But baseline computation runs in a *bare worktree*: no `node_modules`, no
  CI-staged report, only whatever tooling the runner happens to have. `internal/coverage.rustCoverage`
  in `ModeRun` requires `cargo-llvm-cov` on `PATH` and bulwark does **not** install it (unlike
  cargo-audit/cargo-deny/gosec/semgrep, which it pins and installs itself) — so on a runner without
  it, the baseline computes to `{}`. Cached, that `{}` is indistinguishable from a real entry: every
  later PR gets a cache *hit*, every language reports `[NEW]`, and the gate enforces **nothing**,
  silently and permanently, with no way to self-heal. wardnet ran this way — all nine baselines on
  its `bulwark-state` branch were `{}`, and its coverage gate had never once compared anything.
  `cmd/bulwark/coverage.go` therefore refuses to cache an empty report, `gitstate.ReadBaseline`
  treats a cached `{}` as a miss (which heals already-poisoned branches without a manual purge), and
  `warnUnmeasured` names every detected-but-unmeasured ecosystem on stderr rather than dropping it in
  silence. If a language is missing from the gate, bulwark now says so.
- A language with no prior baseline entry (new) is reported but doesn't fail the check on its own;
  a language whose current coverage dipped below its baseline by more than `coverage.tolerance`
  (default 0.1pp, compared at the report's tenth-of-a-point display precision) does. To keep
  tolerated dips from compounding — each merge lowering the baseline the next PR gates against —
  the baseline writers restore any within-tolerance dip to the prior (high-water) value when
  recording; only a beyond-tolerance drop, which was FAIL-visible on the PR that introduced it,
  resets the baseline. A language the baseline has but the
  current run doesn't splits on detection: still detected in the tree means its coverage step just
  didn't run this time (path-filtered CI job, missing tooling) and it's reported as `[UNMEASURED]`;
  no longer detected means the source actually left the tree and it's reported as `[DROPPED]`
  (wardnet/wardnet#892 showed a Rust-only PR as "typescript: no longer measured" when the TS code
  was untouched — only the frontend coverage job had been skipped). Neither fails on its own.

### `--tests=run` vs `--tests=skip`

Unlike Codecov or Sonar — which never execute your tests, only ingest a coverage report your build
already produced — `bulwark coverage`'s default (`--tests=run`) actually runs each ecosystem's test
suite itself (`go test -coverprofile`, `cargo llvm-cov`, a package's `test:coverage` script). That's
the right default for local dev (one command, no separate step to remember), but it's wrong for CI
if a test job already runs with coverage instrumentation on — running tests again would duplicate
work that may already be expensive (wardnet/wardnet-cloud's existing pipelines already run tests
twice: once plain for pass/fail, once instrumented for coverage; `bulwark coverage` piling on a third
run would make it worse, not better).

`--tests=skip` fixes this: it never executes anything, only looks for a report file a prior step
already produced — `internal/coverage.findReport` checks an explicit `--go-report`/`--rust-report`
override first, then a built-in candidate list (`coverage.out`/`cover.out`/`c.out` for Go;
`coverage/llvm-cov.json`/`llvm-cov.json`/`target/llvm-cov/llvm-cov.json` for Rust — TypeScript has
no override, since `coverage/coverage-summary.json` is already Istanbul's own fixed convention, not
something projects vary). In CI, the intended shape is: the existing test job already produces
coverage as a side effect of running tests once (e.g. `cargo llvm-cov nextest` *as* the test runner,
not a second pass after a plain `cargo test`), and `bulwark coverage --tests=skip` runs afterward as
a pure report-consumer.

One exception: computing a **baseline** at a historical main SHA (a cache miss) always uses
`--tests=run` internally (`cmd/bulwark/coverage.go`'s `computeBaselineAt` hardcodes
`coverage.ModeRun`), regardless of the top-level flag — a historical commit's throwaway checkout has
no CI-produced report sitting in it, so there's nothing to skip to. This only costs a real test run
once per main commit (cached afterward on `bulwark-state`), not once per PR, so it doesn't reintroduce
the duplication `--tests=skip` exists to avoid.

TypeScript's `ModeRun` path also runs a one-time dependency install per workspace root before
executing each package's `test:coverage` script — a fresh checkout (baseline computation's throwaway
worktree, but also any other `ModeRun` invocation) has no `node_modules` a prior step could have
already installed. `internal/coverage.resolvePackageManager` auto-detects npm/yarn/pnpm from the
root's lockfile (`package-lock.json`/`yarn.lock`/`pnpm-lock.yaml`); a root with more than one
recognized lockfile is treated as ambiguous and install is skipped there rather than guessing a
priority order. `typescript.install` in `.bulwark.yml` overrides auto-detection entirely with an
explicit shell command (e.g. `corepack enable && yarn install --immutable`), for Corepack-pinned or
otherwise nonstandard install flows auto-detection can't infer, or to resolve an ambiguous root.
`internal/coverage.tsWorkspaceRoots` dedupes so a shared root serving multiple nested packages is
only installed once, not once per package.

### Patch coverage

Aggregate coverage and patch coverage catch disjoint regression classes: aggregate catches
coverage lost in code the current PR never touches (e.g. a deleted test file — none of those lines
are in the diff, so aggregate is the only gate that notices); patch coverage catches untested new
code even when the codebase is big enough that it doesn't move the aggregate percentage. Neither
bounds the other, so `bulwark coverage` computes and gates on both, not either/or — patch coverage
is a second, independent check alongside `diffReport`'s existing aggregate gate, not a replacement.

Patch coverage has **no baseline or threshold of its own** — it always gates against that same
language's aggregate baseline already read from `bulwark-state` (`patch% >= baseline% -
coverage.patch.tolerance` — its own knob, deliberately independent of `coverage.tolerance`, so
loosening the noisy aggregate gate never silently weakens this one). A language
with no aggregate baseline yet is reported informationally (`[NEW]`), not failed, mirroring
aggregate's own handling of a first-time-seen language. It's opt-out, not opt-in, per language, via
`.bulwark.yml`:

```yaml
coverage:
  patch:
    go:
      enabled: false   # defaults to true
```

Changed lines come from a hand-rolled unified-diff hunk parser (`internal/coverage.ChangedLines`,
`git diff --relative --unified=0 <merge-base>..HEAD`) — deliberately not a diff library, since the
format needed is a small, stable subset (hunk headers + `+` lines). `--relative` matters: the
command runs in `--dir`, and every consumer of the changed-line map works in `--dir`-relative
paths (crate/package prefixes, lcov normalization). Without it, git emits repo-root-relative paths,
so with `--dir` pointing at a subdirectory (wardnet's `--dir source`) every changed file failed the
prefix match and the patch gate silently measured nothing. `mergeBase` is the exact same SHA
`gitstate.BaseSHA` already resolved for the aggregate baseline lookup, reused as-is rather than
recomputed. The parser does no language-aware filtering of comments/blank lines/imports — that
happens later, when changed lines are intersected with a coverage report's line-hit data
(`internal/coverage.PatchPercent` counts only lines the report actually mentions).

**A Go coverage profile is the exception, and it bit us.** lcov (Rust, TypeScript) lists only
executable lines, so "absent from the report" safely means "not executable, don't count it". A Go
profile records *blocks*, not statements — every line between a block's braces is in the report,
comments and blank lines included.

The dividing line is the *format*, not the tooling: lcov simply has no slot for a non-executable
line, whereas a Go profile has no notion of a line at all, only a brace-to-brace span that bulwark
itself expands. Don't infer it from how the tool measures — Vitest's default `v8` provider is
range-based exactly like Go's profile, and `llvm-cov`'s own text report does print counts beside
comment lines inside a function. Both still emit clean lcov (v8 maps ranges back onto statements via
`v8-to-istanbul`; `llvm-cov --lcov` only emits `DA:` for lines carrying a coverage segment) —
verified directly against both producers with a comment and a blank line inside an uncovered
function, neither of which appeared in the resulting lcov. So Go needs the filtering below and the
other two genuinely don't. So a comment added inside an uncovered function counted as an
uncovered new line, and a comment-only PR scored 0% patch coverage and failed the gate
(`wardnet/inforge#216`, whose entire diff was `nosemgrep` annotations and workflow YAML).
`internal/coverage.ParseGoProfile` therefore reads each profiled source file and drops blank and
`//`-comment lines before they ever reach `LineHits`. It deliberately does **not** try to track
`/* */` comments (that needs a lexer — `/*` inside a string literal opens nothing) or treat a
leading `*` as a comment continuation (`*p = x` is a pointer assignment): over-counting a rare block
comment merely understates patch coverage, while wrongly dropping a statement would let genuinely
untested code through the gate.

Per-ecosystem line-hit sources, all converging on the same `LineHits` (`map[file]map[line]hits`)
shape:

- **Go**: `internal/coverage.ParseGoProfile` reads the same `coverage.out` profile
  `go tool cover -func` already parses for the aggregate percentage — no separate format, no second
  `go test` run. `Compute`'s returned `PatchSources.GoProfile` is that resolved path, kept alive
  until the caller's `cleanup()` runs.
- **Rust**: `cargo llvm-cov` doesn't emit per-line data in its `--json` export, so patch coverage
  additionally produces an `--lcov` report, per discovered crate/workspace root (see the Coverage
  section above). Under `--tests=run`, this doesn't cost a second test execution: `cargo llvm-cov
  --no-report` runs each crate's suite once and keeps raw profile data on disk, then both `--no-run
  --json` (aggregate, unchanged) and `--no-run --lcov` (patch, new) regenerate their reports from
  that same profile. Under `--tests=skip`, the lcov file is another `findReportForCrate` lookup per
  crate — an explicit `--rust-lcov-report <crateDir>=<path>` override, else a candidate list
  (`coverage/lcov.info`, `lcov.info`, `target/llvm-cov/lcov.info`) resolved relative to that crate's
  own directory, mirroring `--rust-report` exactly. `Compute`'s returned `PatchSources.RustLCOV` is
  a `map[string]string` keyed by crate dir (like TypeScript's `TSLCOV`, not a single path) —
  `cmd/bulwark/coverage.go`'s `rustPatchPercent` resolves each crate's contribution independently,
  mirroring `tsPatchPercent`'s longest-prefix matching so two crates can't clobber each other's hit
  data for a same-named file. A crate with no resolvable lcov file is omitted from patch coverage,
  not a failure — but not silently when it matters (see the `[UNMEASURED]` paragraph below).
- **TypeScript**: reads `<pkgDir>/coverage/lcov.info` (Istanbul/Vitest's native lcov output) — fixed
  convention, no override flag, matching the existing no-override precedent for TS aggregate
  coverage. This only works if the consumer's own test config already has an `lcov` reporter
  enabled; otherwise it's omitted, the same best-effort caveat AGENTS.md already documents for TS
  aggregate coverage.

`cmd/bulwark/coverage.go`'s `patchReport` prints one bracketed status line per language using the
same `[PASS]/[FAIL]/[NEW]` vocabulary the aggregate gate already uses (e.g.
`[FAIL]    go patch: 0.0% (0/9 new lines; baseline 55.68%)`) — this needs no changes to
`action.yml`'s PR-comment builder, since its `cov_detail` regex is generic and already picks up any
matching bracketed line.

**A skipped patch gate must say so.** When a *detected* language's patch gate is enabled and the
diff touches that language's files, but no per-line source was resolved (no lcov for the crate, no
`lcov` reporter in a TS package), `patchReport` prints an `[UNMEASURED] <lang> patch: ...` line and
a stderr warning naming the missing wiring — it never fails the gate on its own, mirroring
`diffReport`'s `[UNMEASURED]` handling. The old behavior was a bare `continue`, and it read as
"patch coverage passed" in the PR comment: wardnet/wardnet#957 shipped a green bulwark summary
(aggregate flat, patch never ran for want of an lcov path) while Codecov — fed the very same lcov
export the pipeline had already produced — failed that diff's patch coverage. The two reports can
only stay aligned if a gate that didn't run is visibly distinct from a gate that passed. The
report stays scoped to detected ecosystems, since the patch gates default to enabled for all three
languages regardless of what the repo contains — a stray changed `.rs` file in a pure Go repo must
not produce a rust line.

## Semgrep: token-bearing vs token-less runs

`internal/semgrep.Check` picks its subcommand from whether `SEMGREP_APP_TOKEN` is set: `semgrep ci`
(diff-aware, applies the org's platform policy, uploads to the AppSec Platform) when it is, plain
`semgrep scan --config <ruleset> --error` when it isn't. Those two modes disagree about **scope**,
and that disagreement was a standing CI defect: GitHub deliberately withholds repo secrets from
`dependabot[bot]` events, so every Dependabot PR arrived with an empty token, silently fell back to
a *whole-repo* scan, and blocked on the consuming repo's pre-existing findings — findings no
token-bearing run had ever reported, in code the PR never touched. Whether a PR was green depended
on who opened it.

`bulwark scan --diff-base <ref>` closes that gap: in scan mode it passes Semgrep
`--baseline-commit`, so the fallback blocks on the same thing `semgrep ci` would — what the change
introduces — and nothing else. `--diff-base auto` resolves the merge-base with `origin/main` via the
same `internal/gitstate.BaseSHA` the coverage gate already uses, so a PR's scan and its coverage
agree on what "this change" means. `action.yml` passes `auto` on every `pull_request` event.

Two deliberate choices in `cmd/bulwark/scan.go`'s `resolveDiffBase`:

- **A token short-circuits it entirely** — `semgrep ci` already scopes itself to the diff, so
  resolving a merge-base would cost a `git fetch` nothing reads, and would newly demand a
  full-history checkout from token-bearing consumers that don't need one today.
- **An unresolvable `auto` is an error, not a silent full scan.** Falling back would reintroduce
  the exact surprise the flag exists to remove: a scan that quietly widens its own scope. A shallow
  checkout is a fixable misconfiguration (`fetch-depth: 0`), so bulwark says so and fails.

Default (`--diff-base` empty) is still a full-repo scan — that's what a local `bulwark scan` wants,
and it's what a push to `main` gets.

Restoring `semgrep ci` on Dependabot PRs (for the platform dashboard's sake) is a *consumer-side*
option, not a bulwark one: the token has to be added to the repo's separate **Dependabot secrets**
store (`gh secret set SEMGREP_APP_TOKEN --app dependabot`), since Actions secrets are not visible to
Dependabot events. It is not required for CI to be green — the diff-aware fallback above is — and it
does hand an upload token to a workflow that executes the bumped dependency's code, so it's a
per-repo judgment call.

## The `action.yml` composite action

Unlike `inforge`'s action (install-only — its invocations vary too much per call site to bake in),
bulwark's usage is uniform enough (`.bulwark.yml` already carries all the config) that the action
owns the whole install → run → report flow: install bulwark, run `scan`/`coverage` (each toggleable
independently via `run-scan`/`run-coverage`), post one sticky PR comment summarizing both (upsert,
not a fresh comment every run — via `marocchino/sticky-pull-request-comment`), and optionally
upload to Codecov (non-blocking, purely for its dashboard/history) and/or switch bulwark's own
Semgrep check into `semgrep ci` mode (diff-aware + uploads to the Semgrep AppSec Platform) when a
`SEMGREP_APP_TOKEN`-equivalent input is supplied. The Codecov upload is two `codecov/codecov-action`
invocations sharing the same `codecov-token` gate — one `report_type: coverage`, one
`report_type: test_results` — both relying entirely on that action's own recursive workspace
auto-discovery rather than bulwark passing explicit `files:`/`directory:` paths itself. This is
why a consumer's CI only needs to hand bulwark a token: bulwark owns the whole Codecov
relationship (coverage *and* JUnit test-results), so the calling workflow never has to install a
Codecov CLI or push to Codecov directly itself.

The PR comment's header embeds `assets/bulwark-logo.png` by **absolute raw URL**
(`raw.githubusercontent.com/wardnet/bulwark/main/...`), never a repo-relative path — the comment is
posted into the *consuming* repo's PR, where a relative `assets/...` would resolve against that repo
and 404. It's pinned to bulwark's default branch, not a release tag, so the image keeps resolving for
consumers pinned to an older bulwark version. Renaming or moving that file therefore breaks the logo
in every consumer's PR comment retroactively — treat its path as a public API.

**Never interpolate `${{ inputs.* }}` or `${{ steps.*.outputs.* }}` directly into a `run:` script
body** — pass it via that step's `env:` block instead, and reference the env var name (`"$DIR"`,
not `"${{ inputs.dir }}"`) inside the script. Semgrep's own `yaml.github-actions.security.run-shell-injection`
rule caught this exact mistake once already (see git history) — expression interpolation directly
into a shell script is a real script-injection vector if the interpolated value could ever contain
shell metacharacters, regardless of how trusted the input value looks today. `if:` conditions and
`with:` blocks on a `uses:` step are fine to interpolate directly — only `run:` script bodies are
the risk, since that's the only place text gets spliced into something a shell then executes.

## Conventions

- **Version injection:** `cmd/bulwark` exposes `var version = "dev"`, overridden at release via
  `-ldflags "-X main.version=<tag>"`. Keep that variable name and package stable.
- **goreleaser & golangci-lint both use the v2 config schema.** In golangci-lint v2, `gosimple` is
  part of `staticcheck` — don't add it as a separate linter (it will error).
- Lint must pass with zero issues; `errcheck` is on, so check returned errors.

## Boundaries

- **Always:** run `go build ./...`, `go test -race ./...`, and `golangci-lint run ./...` before
  proposing a PR.
- **Ask first:** changing the Go version, renaming the binary/`cmd` dir, altering the release
  archive layout, or editing CI.
- **Never:** introduce cgo, commit `dist/` or secrets, or skip the lint/test gates.

## Worktrees

This repo uses a bare-repo + typed-worktree layout managed by the `gt` CLI — one session, one
`gt wt add <type/name>` worktree; never use raw `git worktree` or edit inside `.bare/`.
