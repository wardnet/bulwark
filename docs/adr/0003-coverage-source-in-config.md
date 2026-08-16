# Coverage production is configured in `.bulwark.yml`, not by action inputs

Who produces a repo's coverage — bulwark itself, or a prior CI job — is now
declared as `coverage.source: run|report` in `.bulwark.yml` at the scan root,
alongside `coverage.go.report` and `coverage.rust.{report,lcov}` for where
that job's output lands. The composite action's `tests-mode`, `go-report`,
`rust-report` and `rust-lcov-report` inputs were **removed**, not deprecated.
`action.yml` now passes only `--dir`.

`--dir` stays an input and cannot follow the others into the file: the file
lives *at* the scan root, so bulwark has to be told the root before it can
read its own config.

## Why the file rather than an input

The old axis was named for bulwark's behavior (`tests-mode: run|skip`), which
described the symptom and left the pipeline shape behind it to be inferred.
The decision a repo actually makes is which side owns coverage production, and
that decision has exactly one answer per repo — true for every workflow that
calls the action, and unchanged between runs. An input asks each call site to
restate it identically; the file states it once, next to the code it describes.

The old inputs also could not express a multi-language monorepo without
straining. A composite-action input is a single string, so the repeatable,
crate-keyed report flags had to be smuggled through as newline-separated
`<dir>=<path>` lines and re-split in bash. `coverage.rust.report` takes a
mapping directly, and a bare scalar for the common single-crate case.

## Why removal rather than a deprecation window

Both live consumers (`wardnet/wardnet`'s `coverage.yml`, `tumika/tumika`'s
`ci.yml`) pass the inputs today, and both pin the floating `@v1` alias that
moves on each release — so removal is only safe if their `.bulwark.yml`
changes merge *before* the tag moves. That ordering was accepted deliberately
in exchange for not carrying two spellings of the same setting.

It is cheap because neither consumer's usage needed the inputs in the first
place:

- tumika passes `go-report: coverage.out` with `dir` defaulting to `.`. The
  artifact lands `coverage.out` at the repo root, which *is* the Go module
  root, and `coverage.out` is already the first default candidate
  `findReportForUnit` searches per module. The input was a no-op.
- wardnet passes `rust-report`/`rust-lcov-report` from a `restore` step that
  emits either a constant path or the empty string, depending on whether the
  daemon coverage job was path-filtered away. Both branches resolve
  identically: a missing explicit override is a miss (`findReportForUnit`
  returns not-found rather than falling back to candidates — see
  `TestFindReportOverrideMissing`), and the empty branch falls through to
  candidates that are equally absent. Its `rust-lcov-report` was additionally
  redundant, since the restore step copies the file to
  `source/daemon/coverage/lcov.info`, the first default lcov candidate. Only
  the JSON export needed naming at all, and only because it is called
  `daemon-llvm-cov.json` rather than the conventional `llvm-cov.json`.

The CLI keeps `--source`, `--go-report`, `--rust-report` and
`--rust-lcov-report` as a local-dev/one-off override outranking the file, and
`--tests` as a deprecated alias mapping `skip` to `report`. The binary is a
separate distribution surface from the action, driven by hand and by scripts,
and removing a flag there buys nothing.

## Consequences

- `internal/coverage.Mode`/`ModeRun`/`ModeSkip` are renamed
  `Source`/`SourceRun`/`SourceReport`, and `SourceReport`'s wire value changes
  from `"skip"` to `"report"`. The never-execute guarantee is unchanged;
  `TestGoCoverageSourceReportDoesNotRunTests` carries it under the new name.
- `Compute` keeps taking the source as a **parameter** and deliberately does
  not read `cfg.Coverage.Source`, even though it already receives the config.
  Baseline computation at a historical main SHA must run tests regardless of
  what the repo declares, because that commit's throwaway worktree holds no
  CI-produced report. A `Compute` that consulted the config would give every
  report-sourced repo an empty baseline forever — the `{}` poisoning the
  caller refuses to cache, leaving the gate comparing against nothing.
  `TestComputeBaselineAtRunsTestsEvenWhenSourceIsReport` guards it.
- Both source flags default to `""` rather than `"run"`, so that "unset" is
  distinguishable from "explicitly run". A `"run"` default would always
  outrank the file and `coverage.source` would never be read — a silent
  failure indistinguishable from the key not working.
- A typo'd `coverage.source` is rejected at load rather than falling back to
  `run`. Falling back would run a full suite in a job built on the assumption
  that it wouldn't, and on a runner without the toolchain would measure
  nothing while looking like a real result.
- Downstream, `pedromvgomes/gt` can drop its `reusable-bulwark.yml`
  artifact-mapping step entirely rather than extending it: a rendered caller
  passes `dir` and nothing else, and the consuming repo's own `.bulwark.yml`
  describes its coverage. That is follow-up work, not part of this change.
