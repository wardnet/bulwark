# bulwark

Unified code-quality and security scanning for Rust, TypeScript, and Go — one CLI, run identically
locally and in CI, so "green locally" and "green in CI" can never drift apart.

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

`bulwark coverage` defaults to running each ecosystem's test suite itself (`--tests=run`) — the
right choice for local dev, one command and nothing to remember. In CI, where a test step has
already run with coverage instrumentation on, pass `--tests=skip` so `bulwark coverage` only parses
the report that step already produced instead of running the whole suite again:

```sh
bulwark coverage --dir . --tests=skip \
  --go-report coverage.out \
  --rust-report coverage/llvm-cov.json
```

See [AGENTS.md](AGENTS.md#coverage) for exactly how the baseline is computed and cached, and why
`--tests=run`/`--tests=skip` exist.

## Configuration

`.bulwark.yml` at the repo root is optional and purely **opt-out** — the default (no file) is to
scan everything detected with every check enabled. Use it to disable a language entirely, exclude a
path from detection, or point Semgrep at a custom ruleset:

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
`action.yml`'s own input descriptions for the full list (`dir`, `tests-mode`, `go-report`,
`rust-report`, `github-token`).

(No release has been cut yet, so `@v1` doesn't resolve to anything until the first `v1.x.y` tag is
pushed and the floating `v1` alias is moved to it — see the `bump-version` skill for that process.
The action also can't be exercised end-to-end in bulwark's own CI until then, since its first step
downloads whatever's the *latest released* binary.)

## Contributing

See [AGENTS.md](AGENTS.md) for the development commands, package layout, and conventions.

## License

MIT — see [LICENSE](LICENSE).
