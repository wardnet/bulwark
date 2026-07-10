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
internal/executil/              # shared external-command runner every scanner package uses
.goreleaser.yml                 # build/release config (v2 schema)
.golangci.yml                   # lint config (v2 schema)
.github/workflows/{ci,release}.yml
.github/dependabot.yml
action.yml                      # composite action: install the released binary
scripts/install.sh              # curl|sh installer shipped with every release
```

- Module path: `wardnet/bulwark` (not `github.com/wardnet/bulwark` — a deliberate deviation from
  the other repos in this org, to be applied there too later; do not "fix" this back).
- `bulwark` ships as a single statically-linked binary (`CGO_ENABLED=0`), built for
  linux/darwin × amd64/arm64.

## Status

`bulwark scan` is implemented for Rust, TypeScript, and Go, plus Semgrep — every check is a real
tool invocation (not a stub), verified end-to-end against this repo itself. Every scanner pins its
own tool version and installs it into a bulwark-managed cache directory rather than trusting
whatever's already on the machine (see each `internal/<lang>` package's doc comment for why).
`version` and `update` are fully implemented and tested, following the same pattern as `inforge`'s
self-update (checksum-verified binary replacement, refuses on dev builds, passive update nudge on
every other command). `bulwark coverage` is fully implemented too — see Coverage below — verified
end-to-end against this repo's own real `bulwark-state` branch on GitHub (not just a local fixture).

## Configuration

`.bulwark.yml` at the scan root is optional and purely **opt-out** — its job is narrowing what
bulwark's zero-config default already does (scan everything detected, every check enabled), not
tuning severity or suppressing individual findings (that's what a fix-up pass + inline
`#nosec`/`nosemgrep` annotations in the scanned repo are for). See `internal/config/config.go` for
the full schema; shape:

```yaml
rust:
  enabled: true          # set false to skip Rust entirely even if a Cargo.toml is detected
  exclude: []            # extra directory names to skip during ecosystem/package detection
typescript:
  enabled: true
  exclude: ["legacy-app"]
go:
  enabled: true
  exclude: []
semgrep:
  enabled: true
  config: auto           # override to a custom registry ref/path if needed
```

Omitting the file, or omitting a section/key within it, keeps that value at its default — see
`internal/config/config_test.go` for the exact merge semantics.

## Coverage

`bulwark coverage` diffs the current branch's per-language coverage against a lazily-computed,
per-main-commit-SHA baseline cached on a dedicated `bulwark-state` branch (never `main` — bot-owned
generated cache data, not source, needs no PR/review and never pollutes main's history):

- `internal/gitstate.BaseSHA` resolves `git merge-base HEAD origin/main`.
- `internal/gitstate.ReadBaseline` fetches `bulwark-state` and reads `<sha>.json` via `git show`
  (no checkout) — a missing branch or missing file is a cache miss, not an error.
- On a cache miss, `cmd/bulwark/coverage.go`'s `computeBaselineAt` checks out `origin/main` at that
  SHA into a throwaway `git worktree` (never disturbing the caller's own working tree/branch),
  computes coverage there, and `internal/gitstate.WriteBaseline` pushes it to `bulwark-state` (via
  another throwaway worktree — creating the branch as an orphan the first time). A push race with
  another concurrent cache-miss is non-fatal: caching is an optimization, the caller already has its
  computed value regardless.
- `internal/coverage.Compute` gets the actual number per detected ecosystem: `go tool cover -func`'s
  total line for Go, `cargo llvm-cov --json`'s `data[0].totals.lines.percent` for Rust, and — for
  TypeScript, best-effort only — a package's own `test:coverage` script plus Vitest/Istanbul's
  `coverage-summary.json`, since unlike a linter there's no single canonical coverage-invocation
  convention to standardize on across arbitrary TS packages. A language whose coverage can't be
  measured is silently omitted from the report, not failed.
- A language with no prior baseline entry (new) is reported but doesn't fail the check on its own;
  a language whose current coverage is below its baseline does; a language dropped from the current
  run (baseline had it, current doesn't) is reported as `[DROPPED]` and also doesn't fail on its own.

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
