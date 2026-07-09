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
cmd/bulwark/                    # the bulwark CLI
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

This repo is newly scaffolded. `bulwark scan` and `bulwark coverage` are stubbed (they detect the
ecosystems present but do not yet run any scanner) — see the two subcommand files in `cmd/bulwark/`
for the current state before assuming either is functional. `version` and `update` are fully
implemented, following the same pattern as `inforge`'s self-update (checksum-verified binary
replacement, refuses on dev builds, passive update nudge on every other command).

Planned scanner integration, once implemented:
- **Rust**: clippy (pedantic + restriction groups), cargo-audit (CVE gate), cargo-deny (licenses +
  bans only — `advisories` disabled to avoid duplicating cargo-audit), Semgrep.
- **TypeScript**: a self-contained ESLint + `eslint-plugin-security` toolchain that `bulwark` itself
  pins and invokes via `npx`, independent of the target package's own devDependencies, plus Semgrep.
- **Go**: gosec, govulncheck, Semgrep.
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
