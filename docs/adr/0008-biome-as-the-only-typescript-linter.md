# Biome is the only TypeScript linter

`internal/typescript` runs Biome and nothing else. The ESLint stack —
`eslint`, `eslint-plugin-security`, `@typescript-eslint/parser`, and the
`typescript` package the parser needs to read `.ts` at all — is removed, along
with `eslint-pin/`, the bundled `eslint.config.mjs`, and the Dependabot
ecosystem watching them. `typescript.linter` survives as a key with one
accepted value, `biome`.

This supersedes [ADR 0005](0005-optional-biome-linter.md), which made Biome
opt-in per repo on the reasoning that a repo migrating off ESLint should flip
the key when it is ready. That was right while both engines worked. It shipped
as part of v2.0.0, alongside the other breaking changes, because it is one.

## Why now

ESLint's TypeScript support is not a linter dependency, it is a compiler
dependency. `@typescript-eslint/parser` declares `typescript` as a peer with an
upper bound — `>=4.8.4 <6.1.0` at the time of writing — so bulwark's own pin
manifest had to carry a `typescript` version inside that window. That ceiling
moves whenever @typescript-eslint ships support for a new compiler, which is
always after the compiler is released.

The package doc in `internal/typescript` had already recorded the hazard in as
many words: "which is why the typescript pin is 5.x and not the 7.x now on npm
latest. A Dependabot PR bumping typescript across that ceiling must bump the
parser too." A Dependabot PR then bumped `typescript` to 7.0.2, crossed the
ceiling, and merged. `npm ci` on the committed lockfile has failed with
`ERESOLVE` ever since, which means `installNPMToolchain` fails and every
consumer with TypeScript gets an error instead of a lint result.

Two things about that are worse than the outage:

* **CI stayed green through it.** bulwark's own repository has no TypeScript to
  scan — its only `package.json` files are the pin manifests, which
  `.bulwark.yml` excludes by name — so `self-scan` never exercises the install
  path. The one job that dogfoods the scanner cannot reach this code.
* **Documenting the constraint was not enough to hold it.** The doc was correct,
  specific, and ignored, because nothing executable enforced it.

Biome has no such coupling. It parses TypeScript with its own Rust parser and
depends on no compiler package, so `biome-pin` is a single dependency with no
peer range to cross. The failure class does not exist for it.

## What this costs, measured rather than assumed

Biome is not a replacement for `eslint-plugin-security` — its `security` group
is six JSX/eval/secret rules, and only `noGlobalEval` coincides with anything
the plugin had. But Biome was never where that coverage had to come from.
**Semgrep is**, and bulwark already runs it across every ecosystem at
`config: auto`.

Checked directly, by scanning a TypeScript fixture containing all four
vulnerability classes with the pinned Semgrep this repo ships:

| `eslint-plugin-security` rule | Covered by Semgrep at `config: auto` |
|---|---|
| `detect-child-process` | Yes — `javascript.lang.security.detect-child-process`, and with taint tracking: it names the tainted argument rather than flagging the import |
| `detect-non-literal-fs-filename` | Yes — as `...audit.path-traversal.path-join-resolve-traversal`, which reports the vulnerability class the ESLint rule is a proxy for |
| `detect-unsafe-regex` | Partly — `...audit.detect-non-literal-regexp` catches a pattern built from a variable; static ReDoS analysis of a *literal* pattern has no equivalent |
| `detect-object-injection` | No |

Several of the Semgrep rules are named after the ESLint ones because they are
ports of them, and the taint-tracking versions are stronger than the syntactic
heuristics they replace.

So the real loss is **`detect-object-injection`, plus literal-pattern ReDoS
analysis** — not the four rules. `detect-object-injection` flags every
`obj[key]` where the key is not a literal, which in ordinary TypeScript is
most property access through a variable; it is the most commonly disabled rule
in the plugin, and the consuming repos were not shown to depend on it.

The alternative was keeping ESLint working, which means tracking
@typescript-eslint's compiler ceiling forever: every TypeScript major release
starts a window in which bulwark's pin cannot advance, and closing that window
is upstream's decision, not ours. Paying that indefinitely, for one noisy rule
and one regex analysis, is the trade this rejects.

Biome's own plugin system was considered and rejected as the place to rebuild
any of this. GritQL plugins match syntax patterns against the CST and can emit
diagnostics and fixes, but they carry no dataflow or taint analysis — Biome
2.5's cross-file linting is CSS class matching, explicitly not data
propagation — and there is no distribution mechanism for sharing rules yet.
`detect-object-injection` is syntactic enough to hand-write as a plugin; the
ReDoS analysis is not expressible at all. Rebuilding a weaker copy of what
Semgrep already does with taint would be the wrong direction.

## Migrating

A `.bulwark.yml` carrying `typescript.linter: eslint` is **rejected**, with an
error naming the removal, rather than accepted and silently run under Biome.
A repo that set that key stated which rule set it gates on; switching it without
saying so would change what the scan measures while every run still reported
`[PASS]`, which is the failure this repo rejects unknown config values to
prevent. A value that used to be valid earns the same treatment and a better
sentence.

`internal/config.validateLinter` owns that, and `LinterESLint` remains defined
for no other purpose than to be recognised and refused.
