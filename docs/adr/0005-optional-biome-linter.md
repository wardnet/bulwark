# TypeScript projects may opt into Biome, one repo at a time

The consuming projects are migrating from ESLint to Biome, so `.bulwark.yml` grows
`typescript.linter: eslint|biome` — a validated enum defaulting to `eslint`, mirroring
`coverage.source`'s precedent of naming the decision the repo makes rather than bulwark's
behavior. There is no flag day: ESLint stays the default and is untouched, and a repo flips
the key when it is ready.

## Why no `both`

The two engines' rule sets barely overlap, which initially argued *for* running both:
`eslint-plugin-security` is Node/backend heuristics (`detect-child-process`,
`detect-object-injection`, `detect-non-literal-fs-filename`, …) while Biome's `security`
group is six JSX/eval/secret rules, and only `noGlobalEval` genuinely coincides. So "both"
would strictly dominate on coverage.

It was rejected anyway: the key describes *where a repo has got to in its migration*, and a
migration state is one-of-N. A third value that is neither the old world nor the new one
would outlive the migration it was meant to serve, and every repo would then have to decide
whether it is "done" — a question with no answer, since the union always finds more. Repos
that want ESLint's `detect-*` heuristics keep `linter: eslint` until they don't.

## Consequences

Switching a repo changes what bulwark gates on, in both directions. It loses ESLint's
`detect-*` heuristics, and it gains Biome's `correctness` group — bulwark's TypeScript check
is "security findings only" under ESLint but not under Biome. That is accepted because it is
opt-in per repo and never happens by surprise, and because Semgrep still runs across every
ecosystem either way.

A misspelled value is a hard error rather than a fallback to `eslint`. A silently-ignored
opt-in is the one outcome nobody can detect: the repo believes it has migrated, and bulwark
reports `[PASS]` from the linter it thinks it left behind.
