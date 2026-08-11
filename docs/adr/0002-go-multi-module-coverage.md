# Go coverage supports multiple discovered modules, and skips generated code

bulwark's Go coverage (`internal/coverage.goCoverage`) discovers every Go
module under `--dir` (`detect.GoModuleDirs`, already used by
`internal/golang.Check`) instead of assuming `--dir` is itself a module root,
mirroring ADR 0001's decision for Cargo workspaces. The report flag follows
Rust's shape too: `--go-report` is now repeatable and keyed
(`<moduleDir>=<path>`), with a bare path still accepted when exactly one
module is discovered.

The old single-module assumption did not fail loudly, which is why it survived
so long. `go test`, `go list -m` and `go tool cover -func` are all
module-scoped, and bulwark ran all three at `--dir`. In a monorepo that is the
*parent* of every module: `go tool cover -func` fails outright ("no required
module provides package ...: go.mod file not found") and `go list -m` answers
`command-line-arguments` — not an error, and not a prefix any profile entry
carries. Go coverage was therefore absent from wardnet's gate entirely, both
aggregate and patch, while the run stayed green. A repo can lose a language's
gate without anyone being told.

Two consequences worth recording:

* **The aggregate percentage is computed from the profile, not from `go tool
  cover -func`.** It is the same covered-over-total statement ratio, but
  parsing works from anywhere, which removes the module-root requirement that
  caused the bug. `PatchSources` gained a per-module `GoModuleProfile`
  (profile path, module path, directory relative to `--dir`) because one
  global module path cannot map two modules: wardnet's are
  `github.com/wardnet/wardnet/source/wctl` under `wctl/` and
  `wardnet.network/go` under `sdk/wardnet-go/`, sharing no prefix and neither
  matching its own directory.

* **Generated files are excluded from both gates**, matched on Go's own
  `// Code generated ... DO NOT EDIT.` convention
  (<https://golang.org/s/generatedcode>) rather than a filename pattern, which
  is what golangci-lint and Codecov key off too. This is not cosmetic: it is
  the difference between a gate that measures testing and one that measures
  code generation. wardnet's regenerated REST client was 983 of one PR's 1007
  changed Go lines, and dragged its SDK module's aggregate to 2%. Excluding it
  moved that repo to a measured `go: 35.5%` aggregate and `go patch: 64.1%
  (84/131 new lines)`.

The alternative — requiring consumers to add a `go.work` at the scan root —
was rejected. It would fix only the aggregate (`go list -m` in a workspace
lists every module, so patch coverage stays broken), it pushes a real
constraint on a repo's own toolchain and linting for bulwark's convenience,
and it leaves generated code in the denominator regardless.
