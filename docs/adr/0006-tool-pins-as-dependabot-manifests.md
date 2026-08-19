# Tool version pins live in package manifests, not Go constants

Every tool bulwark installs — ESLint, Biome, cargo-audit, cargo-deny, gosec, govulncheck,
Semgrep — used to be pinned in a Go string constant, and `.github/dependabot.yml` watched only
`gomod` and `github-actions`. So none of the pins were visible to Dependabot, and nothing could
age them out. Pinning the toolchain is bulwark's whole premise; a pinned *security* toolchain
that nothing bumps is a scanner that goes quietly stale while still reporting `[PASS]`, which is
the worst failure available to it. Each pin now lives in a real package-manager manifest,
colocated with the package that uses it and watched by its own Dependabot entry.

Where the manifest can be the sole runtime source of truth it is, and the constant is deleted:
the npm toolchains install straight from a committed `package.json`/`package-lock.json` with
`npm ci`, and the Cargo and pip pins are parsed out of their manifests at startup. No constant
means no drift.

## One manifest per cargo tool

cargo-audit and cargo-deny get a manifest each rather than sharing one. They cannot resolve
together — cargo-deny's `krates` requires `petgraph =0.8.1` while cargo-audit's `cargo-lock`
pulls `0.8.2` — and a `[package]` manifest with no target does not parse at all. Either way
cargo errors, Dependabot's updater fails inside a job log nobody reads, and the pin silently
stops being bumped: the very failure this ADR is about, reintroduced one level down. `cargo
install` never shares a resolution graph between two tools, which is why the conflict does not
show up in bulwark's actual use of them.

## Why Go is the exception

`gosecPkg`/`govulncheckPkg` are const expressions that concatenate the version at compile time,
and `go:embed` cannot read files inside a nested module — so the versions genuinely cannot be
read from the manifest at runtime. The constants stay, and `internal/golang/pins_test.go` fails
CI if a Dependabot bump isn't mirrored into them. A two-step bump is worse than the others, but
it is loud, which was the entire point.

The pins live in a **separate module** (`internal/golang/go-pin/go.mod`) rather than as `tool`
directives in bulwark's own `go.mod`. The latter works and is more idiomatic, but it drags
gosec's whole dependency graph — grpc, protobuf, `google.golang.org/api` — into a module that
never links any of it, measured at go.mod 17→69 lines and go.sum 17→114, and would then aim
Dependabot at dozens of transitive dependencies bulwark does not use.

## Consequences

The manifests are real `package.json`/`Cargo.toml`/`go.mod` files sitting in the tree, so
detection would treat each as a package to lint, a crate to audit or a module to scan. bulwark
therefore gains its first `.bulwark.yml`, excluding them from its own self-scan.

Adding a future pin is three edits, not one — manifest, Dependabot entry, `.bulwark.yml`
exclude — and forgetting the third is silent rather than loud, so
`internal/config`'s `TestPinDirectoriesAreExcluded` fails when a `*-pin` directory isn't
excluded.

The npm cache directory is now keyed by a hash of the lockfile rather than a hand-maintained
string of concatenated versions. That key could previously be forgotten when a dependency was
added — the exact mechanism by which "every `.ts` file is silently skipped" would have returned
when the TypeScript parser was introduced.
