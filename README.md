<p align="center">
  <img src="assets/bulwark-logo.png" alt="bulwark" width="140">
</p>

<h1 align="center">bulwark</h1>

<p align="center">
  Unified code-quality and security scanning for Rust, TypeScript, and Go — one CLI, run identically
  locally and in CI, so "green locally" and "green in CI" can never drift apart.
</p>

`bulwark` replaces ad hoc, per-repo security workflows (CodeQL, standalone `cargo-audit` jobs,
Codecov as a blocking gate) with one consistent pipeline: it auto-detects which ecosystems a repo
uses, runs each one's checks with a pinned, self-installed toolchain, and diffs test coverage
against a lazily-computed baseline — no manual setup, no "works on my machine."

## What it checks

| Ecosystem | Checks |
|---|---|
| Rust | `cargo fmt --check`, `cargo clippy` (pedantic/restriction groups come from the target repo's own `Cargo.toml`), `cargo-audit` (CVEs), `cargo-deny` (licenses + bans) |
| TypeScript | ESLint + `eslint-plugin-security`, using a toolchain `bulwark` bundles and pins itself — independent of whatever (if anything) the target package declares in its own `devDependencies` |
| Go | `gosec`, `govulncheck` |
| All of the above | [Semgrep](https://semgrep.dev) |

Every tool is pinned to an exact version and installed into a `bulwark`-managed cache directory the
first time it's needed — nothing is ever silently run at whatever version happens to already be on
`PATH`.

## Install

```sh
curl -fsSL https://github.com/wardnet/bulwark/releases/latest/download/install.sh | sh
```

This installs to `~/.local/bin` by default (override with `BULWARK_INSTALL_DIR`); pin a specific
version with `BULWARK_VERSION=1.2.3 curl ... | sh`. Update in place any time with `bulwark update`.

> **Don't** `go install wardnet/bulwark/cmd/bulwark@latest` — the module path is deliberately not a
> resolvable `github.com/...` import path (see [AGENTS.md](AGENTS.md)), so `go install` won't find
> it. Use the installer above.

## Usage

```sh
bulwark scan --dir .          # run every check for every ecosystem detected under --dir (default ".")
bulwark coverage --dir .      # diff current coverage against the cached baseline for the PR's base commit
bulwark version
bulwark update                 # self-update to the latest release
```

`bulwark scan` exits non-zero if any check fails, printing a `[PASS]`/`[FAIL]` line per check.

`bulwark coverage` defaults to running each ecosystem's test suite itself — the right choice for
local dev, one command and nothing to remember. A repo whose CI already runs a test job with
coverage instrumentation on says so once, in `.bulwark.yml`, and bulwark becomes a pure consumer of
that job's report instead of running the whole suite again:

```yaml
coverage:
  source: report        # default is "run" — bulwark produces coverage itself
```

That's usually the whole change: each language's conventional report paths are searched without
further configuration (`coverage.out`/`cover.out`/`c.out` per Go module, `coverage/llvm-cov.json`
and `coverage/lcov.info` per Rust crate, Istanbul's `coverage/coverage-summary.json` per TS
package). Only a report written somewhere unconventional needs naming:

```yaml
coverage:
  source: report
  rust:
    report: daemon/coverage/daemon-llvm-cov.json   # non-standard name, so point at it
```

`--source run|report` overrides the file for a one-off local run, as do `--go-report`,
`--rust-report` and `--rust-lcov-report` for the paths.

See [AGENTS.md](AGENTS.md#coverage) for exactly how the baseline is computed and cached, and why
`coverage.source` exists.

## Configuration

`.bulwark.yml` at the repo root is optional — the default (no file) is to scan everything detected
with every check enabled and to produce coverage by running the tests. Use it to disable a language
entirely, exclude a path from detection, point Semgrep at a custom ruleset, or describe how the
repo's coverage is produced:

```yaml
rust:
  enabled: true
  exclude: []
typescript:
  enabled: true
  exclude: ["legacy-app"]
go:
  enabled: true
  exclude: []
semgrep:
  enabled: true
  config: auto
coverage:
  source: run     # or "report" — a prior CI job produces coverage, bulwark only parses it
  tolerance: 0.1  # pp the aggregate gate tolerates below baseline (patch gate: coverage.patch.tolerance)
```

See [AGENTS.md](AGENTS.md#configuration) for the full schema and merge semantics.

## GitHub Actions

The action installs bulwark, runs `scan`/`coverage`, posts a single sticky PR comment summarizing
both, and optionally reports to the Semgrep AppSec Platform and/or Codecov:

```yaml
permissions:
  contents: write       # bulwark coverage caches baselines on the bulwark-state branch
  pull-requests: write  # for the PR summary comment
steps:
  - uses: actions/checkout@v7
    with:
      fetch-depth: 0    # bulwark coverage needs full history to resolve the PR's base commit
  - uses: wardnet/bulwark@v1
    with:
      semgrep-app-token: ${{ secrets.SEMGREP_APP_TOKEN }}  # optional — omit to keep Semgrep local-only
      codecov-token: ${{ secrets.CODECOV_TOKEN }}          # optional — omit to skip the Codecov upload
```

Both `scan` and `coverage` can be turned off independently (`run-scan: false` / `run-coverage:
false`) if a repo only wants one of them, or isn't ready to grant `contents: write` yet. See
`action.yml`'s own input descriptions for the full list (`dir`, `version`, `github-token`, and the
two optional tokens).

Note there is no input for coverage production. Who produces coverage, and where its reports live,
come from `.bulwark.yml` at the scan root — the same answer for every workflow that calls the
action, so it's stated once in the repo rather than restated at each call site. `dir` is the one
thing that can't move there, since the file lives *at* the scan root and bulwark has to be told the
root before it can read its own config.

(No release has been cut yet, so `@v1` doesn't resolve to anything until the first `v1.x.y` tag is
pushed and the floating `v1` alias is moved to it — see the `bump-version` skill for that process.
The action also can't be exercised end-to-end in bulwark's own CI until then, since its first step
downloads whatever's the *latest released* binary.)

## Contributing

See [AGENTS.md](AGENTS.md) for the development commands, package layout, and conventions.

## License

MIT — see [LICENSE](LICENSE).
