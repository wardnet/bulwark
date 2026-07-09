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
every other command). `bulwark coverage` is still stubbed — see `cmd/bulwark/coverage.go`.

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

Planned, not yet implemented:
- **Coverage**: a lazy, per-main-commit-SHA baseline stored on a `bulwark-state` branch (not `main`)
  — no separate "on merge to main" trigger; the first PR against a new main SHA computes and caches
  the baseline, every subsequent PR against that SHA reuses it.

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
