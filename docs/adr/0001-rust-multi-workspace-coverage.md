# Rust coverage supports multiple discovered Cargo workspaces

bulwark's Rust coverage (`internal/coverage.rustCoverage`) and check
(`internal/rust.Check`) discover every independent Cargo workspace/crate root
under `--dir` instead of assuming `--dir` itself is the root. We chose full
multi-workspace support (map-typed `PatchSources.RustLCOV`, repeatable/keyed
`--rust-report`/`--rust-lcov-report` CLI flags) over a simpler bounded fix
that would have assumed a single Cargo workspace per repo, even though the
immediate target repo (wardnet/wardnet, PR wardnet/wardnet#822) has only one
Rust crate today. We picked the more general design to avoid a second
migration if/when a consumer repo has genuinely independent Cargo workspaces
side by side, accepting the larger CLI surface and internal plumbing now.
