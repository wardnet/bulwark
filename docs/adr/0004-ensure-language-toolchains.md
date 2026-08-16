# bulwark ensures the language toolchain each detected ecosystem needs

`internal/toolchain` makes sure a Go, Rust or Node runtime is available at the
version the scanned repository declares, before any scanner or coverage
command runs. It is called from both `cmd/bulwark/scan.go` and
`cmd/bulwark/coverage.go`, for the enabled ecosystems `internal/detect` found.

This completes the "pin the exact toolchain, don't reuse ambient installs"
principle stated in `internal/golang`. bulwark already provisions everything
it *runs* — gosec and govulncheck via `go install` into a version-keyed cache,
cargo-audit/cargo-deny via `cargo install`, ESLint via npm, Semgrep via pipx —
but assumed the toolchain it does that provisioning *with* was on PATH.

## Scope of the problem

Nothing was broken. On GitHub-hosted runners Go is preinstalled and Go 1.21+
fetches whatever `go.mod` requires, so the assumption held. What was missing
was robustness and determinism: a self-hosted or container runner without Go
fails at `go install`; the version used is whatever the image ships rather
than what the repo declares; and with no shared module cache every run
re-downloads.

## Versions come from the repo's manifests, not from `.bulwark.yml`

| Ecosystem | Source |
|---|---|
| Go | `go` and `toolchain` directives in every discovered `go.mod`, highest wins |
| Rust | `channel` in `rust-toolchain.toml`, else the legacy bare `rust-toolchain` |
| TypeScript | `engines.node`, else `.nvmrc` (package directory, then scan root) |

These files are already authoritative and already enforced by the language's
own tooling. A version restated in `.bulwark.yml` could only agree redundantly
or drift silently, and a stale duplicate is worse than an absent one because
it reads as authoritative. `toolchain.{go,rust,node}` exist as a deliberate
local override — an exception to what the manifests say, not a competing
statement of the same fact. `toolchain.enabled: false` keeps the diagnostics
and skips the downloads, for air-gapped or fully-preprovisioned runners.

Reading *every* manifest rather than one at a fixed path is the part that
makes this work on a monorepo, and is why it lives here rather than in CI.

## Ambient toolchains are preferred

An already-installed toolchain that satisfies the declared version is used
unchanged; provisioning happens only when one is missing or too old.
Downloading something already present and correct is pure cost, and on the
runners bulwark actually runs on that is the common case — so the common path
performs no network I/O.

Two edges are decided deliberately. An *unpinned* requirement — a `stable`
rust channel, an `lts/*` .nvmrc, no declaration at all — is satisfied by any
present toolchain, because the repo named no floor to be below. A toolchain
that will not identify itself is treated as too old, because it cannot be
*shown* to satisfy a pin and provisioning is the safe side of that doubt.

## Each ecosystem is provisioned by its own official mechanism

Only one of the three involves bulwark downloading anything.

- **Go** delegates to `GOTOOLCHAIN`. Any Go 1.21+ fetches another toolchain
  itself via the module proxy, verified against Go's checksum database. Only
  with no Go, or one older than 1.21, does bulwark download a tarball from
  go.dev with the SHA-256 taken from the release index.
- **Rust** delegates to rustup, which already reads the same
  `rust-toolchain.toml`. This extends `internal/rust`'s existing stance that
  the toolchain version is the target repo's responsibility, adding only that
  the channel is materialised up front with the `clippy` and `rustfmt`
  components — rustup would otherwise install it lazily inside `cargo clippy`,
  where a missing component reads as a check failure rather than a setup step.
  With no rustup present bulwark reports that and continues; installing rustup
  itself would be a larger, less reversible thing to do unasked.
- **Node** is downloaded and unpacked by bulwark, from nodejs.org, with the
  digest read from that release's `SHASUMS256.txt`. There is no assumable
  equivalent of GOTOOLCHAIN or rustup: nvm, fnm and volta are all optional and
  mutually exclusive.

Setting `GOTOOLCHAIN` has a second effect worth naming. Its default (`auto`)
consults `go.mod` only when the go command runs *inside* that module, and
`internal/golang` runs `go install <tool>@<version>` outside any module — so
gosec and govulncheck were built with whatever Go the runner shipped, and a
govulncheck built by an older Go rejects newer source outright. That is the
failure AGENTS.md records against wardnet's CI, worked around there by pinning
`go-version` in every workflow. Consumers no longer need that pin on bulwark's
account.

## Failures warn rather than fail the scan

Provisioning is preparation, not a gate. Falling through to "whatever is on
PATH" is precisely today's behavior, so a network blip must not turn a working
scan into a hard failure — and if the toolchain really is absent, the next
step fails loudly and specifically. What must not happen is failing silently,
so every reuse, substitution, skip and failure is named on stderr.

Stderr rather than stdout is deliberate: `action.yml`'s PR-comment builder
scrapes bracketed tags from coverage stdout with a pattern that is not
line-anchored, so unrelated chatter on stdout would surface in the comment.

## Consequences

- `config.Config` gains a `Toolchain` section; `internal/toolchain` does not
  import `internal/config`, so config stays a leaf. `cmd/bulwark/toolchain.go`
  maps between them.
- The resolved environment is applied by mutating this process's `PATH` and
  variables, rather than threading an `Env` through every `Check` and
  `Compute` signature. bulwark is a single-shot CLI; this is what a shell
  `export` ahead of it would do. `Ensure` returns the `Env` rather than
  applying it so the decision stays visible at the call site and tests can
  inspect it without touching global state.
- `cmd/bulwark/coverage.go` now detects ecosystems *before* calling
  `coverage.Compute` rather than after, since the toolchain has to be in place
  before Compute shells out. Compute detects again internally; the walk is
  cheap and idempotent.
- Downloads land in the existing version-keyed `~/.cache/bulwark` layout, so a
  consumer already caching that path picks up language toolchains with no
  cache-key change. Installs stage into a sibling directory and are renamed
  into place, so an interrupted download cannot leave a half-populated
  toolchain that the next run treats as complete.
- Archive extraction refuses entries that escape the destination. Note the
  containment check runs on the *unclean* path: cleaning
  `go/../../escaped` first collapses it to `../escaped`, after which dropping
  the archive's leading component eats the traversal and yields a
  benign-looking `escaped` — contained, but written under a name unrelated to
  what the archive asked for. Splitting the raw name leaves the traversal
  intact to be rejected.
- Only enabled ecosystems are provisioned. A language disabled in
  `.bulwark.yml` is one whose tools never run, and on a repo that disabled
  Rust precisely because its runner has no Rust, provisioning it would be
  actively unhelpful.
