// Package coverage computes per-language test coverage percentages for
// whatever ecosystems are detected under a directory, reusing each
// language's own existing coverage tooling rather than reimplementing it.
package coverage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/cover"

	"wardnet/bulwark/internal/config"
	"wardnet/bulwark/internal/detect"
	"wardnet/bulwark/internal/executil"
)

// Mode controls whether Compute executes tests itself or only parses an
// already-produced coverage report.
type Mode string

const (
	// ModeRun executes each ecosystem's test suite (with coverage
	// instrumentation) itself. The right default for local dev: one command,
	// no separate test step to remember to run first.
	ModeRun Mode = "run"
	// ModeSkip never executes tests — it only looks for a report file each
	// ecosystem's coverage tooling would already have produced as part of a
	// prior, separate test step (e.g. CI's own test job), and parses it. This
	// mirrors how Codecov/Sonar work: they never run your tests themselves,
	// only ingest a report your build already produced. Use this in CI so a
	// test step that already runs with coverage instrumentation on doesn't
	// get executed a second (or, for repos whose CI already runs a plain
	// pass/fail test job separately from an instrumented coverage job, a
	// third) time.
	ModeSkip Mode = "skip"
)

// ReportOverrides maps a discovered unit directory — a Rust crate/workspace
// root, or a Go module root — relative to --dir, to an explicit report path
// override (also relative to --dir), for use with ModeSkip. The empty-string
// key ("") is the override applied when discovery finds exactly one such unit
// — this preserves the single-unit CLI usage of a bare `--rust-report <path>`
// or `--go-report <path>` unchanged even though those flags are now
// repeatable/keyed for multi-unit repos.
type ReportOverrides map[string]string

// RustReportOverrides and GoReportOverrides name ReportOverrides at the call
// sites carrying one language's overrides, so a signature says which language
// it is about.
type (
	RustReportOverrides = ReportOverrides
	GoReportOverrides   = ReportOverrides
)

// ReportPaths overrides the default report-file search candidates per
// language, for a repo whose coverage output doesn't land at one of the
// conventional locations findReport/findReportForUnit checks. A zero value
// uses the built-in candidate list for that language. Only meaningful with
// ModeSkip.
type ReportPaths struct {
	Go       GoReportOverrides
	Rust     RustReportOverrides
	RustLCOV RustReportOverrides
}

// PatchWanted says which languages' patch-coverage line-hit sources Compute
// should try to resolve alongside the aggregate percentage it always
// computes for a detected ecosystem — set from cfg.Coverage.Patch.*.Enabled
// by the caller. Requesting a language Compute didn't detect is a no-op.
type PatchWanted struct {
	Go, Rust, TypeScript bool
}

// PatchSources holds whatever on-disk artifacts patch coverage needs to
// derive per-line hit data, resolved as a side effect of Compute so tests
// are never executed a second time just to get line-level detail. A field
// is empty/nil when that language's patch coverage wasn't requested, or
// requested but unresolvable (tool unavailable, no report found) — the
// caller treats a missing source as "can't compute patch coverage for this
// language", the same soft-omission Compute already applies to aggregate
// percentages.
type PatchSources struct {
	GoProfiles map[string]GoModuleProfile // Go module dir -> its profile and module path
	RustLCOV   map[string]string          // Rust crate dir -> its cargo-llvm-cov lcov export
	TSLCOV     map[string]string          // TS package dir -> its coverage/lcov.info
}

// GoModuleProfile locates one discovered Go module's coverage profile,
// together with what turning that profile's package-qualified file names into
// --dir-relative paths takes: the module path to strip off the front, and the
// module's own directory to put back on.
//
// Both halves are per-module because neither generalises. A repo's modules
// need share no prefix and a module path need not resemble its directory —
// wardnet's two are `github.com/wardnet/wardnet/source/wctl` (under
// `wctl/`) and `wardnet.network/go` (under `sdk/wardnet-go/`) — so one global
// module path, which is what bulwark used to carry, can only ever resolve one
// module's entries and silently drops the rest.
type GoModuleProfile struct {
	// Profile is the path to the module's `go test -coverprofile` output.
	Profile string
	// ModuleName is the module's path, as `go list -m` reports it.
	ModuleName string
	// RelDir is the module's directory relative to --dir, "" when the module
	// root is --dir itself.
	RelDir string
}

// Compute returns a coverage percentage per detected ecosystem under dir,
// plus whatever patch-coverage sources want asked for. An ecosystem is
// silently omitted from the percentage map (not an error) when its coverage
// tooling isn't available or produces no measurable result — coverage
// tooling is more varied across projects than a linter, so bulwark reports
// what it can rather than failing the whole run over one package's missing
// test script.
//
// The initial ecosystem-detection pass uses cfg.AllExcludes() (it doesn't yet
// know which language a given excluded directory belongs to), but each
// language-specific pass below uses only that language's own exclude list —
// a Rust-only exclude must never cause a real TypeScript package to be
// silently dropped from coverage, matching how cmd/bulwark/scan.go scopes
// excludes per language.
//
// The returned cleanup func must be called once the caller is done with any
// PatchSources paths (it removes the scratch directory ModeRun writes
// reports into; ModeSkip's cleanup is a no-op since it only ever reads
// files the caller/CI already produced).
func Compute(ctx context.Context, dir string, cfg config.Config, mode Mode, reports ReportPaths, want PatchWanted) (map[string]float64, PatchSources, func(), error) {
	ecosystems, err := detect.Ecosystems(dir, cfg.AllExcludes())
	if err != nil {
		return nil, PatchSources{}, func() {}, err
	}

	workDir := ""
	cleanup := func() {}
	if mode == ModeRun {
		tmp, err := os.MkdirTemp("", "bulwark-coverage-*")
		if err != nil {
			return nil, PatchSources{}, func() {}, err
		}
		workDir = tmp
		cleanup = func() { _ = os.RemoveAll(tmp) }
	}

	report := map[string]float64{}
	var sources PatchSources
	for _, e := range ecosystems {
		var pct float64
		var ok bool
		switch e {
		case detect.Rust:
			var lcovPaths map[string]string
			pct, lcovPaths, ok = rustCoverage(ctx, dir, workDir, mode, cfg.Rust.Exclude, reports.Rust, reports.RustLCOV, want.Rust)
			if ok && want.Rust {
				sources.RustLCOV = lcovPaths
			}
		case detect.Go:
			var goProfiles map[string]GoModuleProfile
			pct, goProfiles, ok = goCoverage(ctx, dir, workDir, mode, cfg.Go.Exclude, reports.Go)
			if ok && want.Go {
				sources.GoProfiles = goProfiles
			}
		case detect.TypeScript:
			pct, ok = tsCoverage(ctx, dir, cfg.TypeScript.Exclude, mode, cfg.TypeScript.Install)
			if ok && want.TypeScript {
				pkgDirs, _ := detect.TSPackageDirs(dir, cfg.TypeScript.Exclude)
				sources.TSLCOV = tsLCOVSources(pkgDirs)
			}
		}
		if ok {
			report[string(e)] = pct
		}
	}
	return report, sources, cleanup, nil
}

// moduleName returns the Go module path rooted at moduleDir (e.g.
// "wardnet/bulwark"), needed to strip the package-qualified prefix
// x/tools/cover puts on each file name in a coverage profile. A lookup
// failure just means this module can't be measured — never a fatal error,
// matching PatchSources' overall soft-omission contract.
//
// moduleDir must be a module root, not a directory that merely contains one:
// `go list -m` run above a module answers "command-line-arguments", which is
// not an error and not a prefix any profile entry carries, so a caller that
// passed the wrong directory gets silent nonsense rather than a failure.
func moduleName(ctx context.Context, moduleDir string) string {
	r := executil.Run(ctx, moduleDir, "go", "list", "-m")
	if !r.Ok() {
		return ""
	}
	name := strings.TrimSpace(r.Output)
	// A workspace prints one module per line; a non-module root prints the
	// placeholder. Neither identifies a single module to strip.
	if name == "" || name == "command-line-arguments" || strings.Contains(name, "\n") {
		return ""
	}
	return name
}

// tsLCOVSources looks for an lcov.info Istanbul/Vitest may have already
// written (as a side effect of the same test:coverage run tsCoverage just
// executed, or a prior CI step under ModeSkip) alongside each package's
// coverage-summary.json — no separate test execution needed either way.
func tsLCOVSources(pkgDirs []string) map[string]string {
	sources := map[string]string{}
	for _, pkgDir := range pkgDirs {
		p := filepath.Join(pkgDir, "coverage", "lcov.info")
		if _, err := os.Stat(p); err == nil {
			sources[pkgDir] = p
		}
	}
	return sources
}

// findReport resolves the coverage report file to parse: override if given
// (relative to dir), otherwise the first of candidates (also relative to
// dir) that actually exists. Returns false if nothing is found.
func findReport(dir, override string, candidates []string) (string, bool) {
	if override != "" {
		p := filepath.Join(dir, override)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
		return "", false
	}
	for _, c := range candidates {
		p := filepath.Join(dir, c)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// goReportCandidates are the conventional relative paths a `go test
// -coverprofile=...` profile tends to land at when a repo's own CI already
// produces one.
var goReportCandidates = []string{"coverage.out", "cover.out", "c.out"}

// goCoverage reads the statement coverage percentage for every independent Go
// module discovered under dir (see detect.GoModuleDirs), averaging across the
// modules that produced a result — mirroring how rustCoverage averages across
// crates and tsCoverage across packages. Returns a map of module dir -> what
// patch coverage needs to read that module's profile back.
//
// Discovering modules rather than treating dir as one is what makes a
// monorepo work at all: `go test` and `go list -m` are both module-scoped, so
// running them at a dir that merely *contains* modules measures nothing.
func goCoverage(ctx context.Context, dir, workDir string, mode Mode, exclude []string, overrides GoReportOverrides) (float64, map[string]GoModuleProfile, bool) {
	moduleDirs, err := detect.GoModuleDirs(dir, exclude)
	if err != nil || len(moduleDirs) == 0 {
		return 0, nil, false
	}
	solo := len(moduleDirs) == 1

	var total, count float64
	profiles := map[string]GoModuleProfile{}
	for i, moduleDir := range moduleDirs {
		rel, err := filepath.Rel(dir, moduleDir)
		if err != nil {
			continue
		}
		if rel == "." {
			rel = ""
		}
		pct, src, ok := goCoverageOne(ctx, dir, moduleDir, rel, workDir, mode, overrides, solo, i)
		if !ok {
			continue
		}
		total += pct
		count++
		profiles[moduleDir] = src
	}
	if count == 0 {
		return 0, nil, false
	}
	return total / count, profiles, true
}

// goCoverageOne measures one module, either running `go test -coverprofile`
// inside it (ModeRun) or parsing a profile another step already produced
// (ModeSkip). Every command runs in moduleDir rather than dir, because that
// is the only place a module-scoped Go command works.
func goCoverageOne(ctx context.Context, dir, moduleDir, moduleRelDir, workDir string, mode Mode, overrides GoReportOverrides, solo bool, idx int) (float64, GoModuleProfile, bool) {
	var profile string
	switch mode {
	case ModeSkip:
		found, ok := findReportForUnit(dir, moduleDir, moduleRelDir, overrides, solo, goReportCandidates)
		if !ok {
			return 0, GoModuleProfile{}, false
		}
		profile = found
	default:
		// Distinct file per module: one shared cover.out would have each
		// module overwrite the last, leaving only the final module measured.
		profile = filepath.Join(workDir, fmt.Sprintf("cover-%d.out", idx))
		if r := executil.Run(ctx, moduleDir, "go", "test", "-coverprofile="+profile, "./..."); !r.Ok() {
			return 0, GoModuleProfile{}, false
		}
	}

	name := moduleName(ctx, moduleDir)
	if name == "" {
		return 0, GoModuleProfile{}, false
	}
	src := GoModuleProfile{Profile: profile, ModuleName: name, RelDir: moduleRelDir}
	pct, ok := goProfilePercent(src, dir)
	if !ok {
		return 0, GoModuleProfile{}, false
	}
	return pct, src, true
}

// goProfilePercent computes a module's statement coverage straight from its
// profile: the covered-statements-over-total-statements ratio `go tool cover
// -func` prints on its `total:` line, minus generated files.
//
// Parsing rather than shelling out to `go tool cover -func` is what lets this
// work from anywhere. That command resolves each profile entry's
// package-qualified name through the module graph, so it only succeeds when
// run from inside the module the profile came from — from a monorepo root it
// fails outright ("no required module provides package ...: go.mod file not
// found"), which is how Go coverage came to be silently absent from the gate
// in wardnet. The number is the same either way; it is already in the file.
//
// It is also the only place a generated file can be dropped from the
// denominator, which `go tool cover` offers no way to do.
func goProfilePercent(src GoModuleProfile, dir string) (float64, bool) {
	profiles, err := cover.ParseProfiles(src.Profile)
	if err != nil {
		return 0, false
	}
	var covered, total int
	for _, p := range profiles {
		if isGeneratedGoFile(filepath.Join(dir, filepath.FromSlash(goRelPath(p.FileName, src)))) {
			continue
		}
		for _, b := range p.Blocks {
			total += b.NumStmt
			if b.Count > 0 {
				covered += b.NumStmt
			}
		}
	}
	if total == 0 {
		return 0, false
	}
	return float64(covered) / float64(total) * 100, true
}

// llvmCovExport is the subset of `cargo llvm-cov --json`'s export format
// (https://llvm.org/docs/CommandGuide/llvm-cov.html#export) bulwark needs.
type llvmCovExport struct {
	Data []struct {
		Totals struct {
			Lines struct {
				Percent float64 `json:"percent"`
			} `json:"lines"`
		} `json:"totals"`
	} `json:"data"`
}

// rustReportCandidates are the conventional relative paths bulwark checks for
// an existing cargo-llvm-cov JSON export another step already produced.
var rustReportCandidates = []string{"coverage/llvm-cov.json", "llvm-cov.json", "target/llvm-cov/llvm-cov.json"}

// rustLCOVReportCandidates are the conventional relative paths bulwark checks
// for an existing cargo-llvm-cov lcov export another step already produced.
var rustLCOVReportCandidates = []string{"coverage/lcov.info", "lcov.info", "target/llvm-cov/lcov.info"}

// findReportForUnit resolves a coverage report path for one discovered unit
// directory — a Rust crate/workspace root, or a Go module root: first any
// override keyed by unitRelDir (unitDir's path relative to dir), then — only
// when solo is true, i.e. exactly one such unit was discovered — the override
// keyed by "", then the candidate list resolved relative to unitDir itself
// (matching where the language's own tooling writes output when run in-place
// inside each unit directory). Override path values are resolved relative to
// dir, preserving the documented "relative to --dir" contract for the
// override strings themselves.
func findReportForUnit(dir, unitDir, unitRelDir string, overrides ReportOverrides, solo bool, candidates []string) (string, bool) {
	if override, ok := overrides[unitRelDir]; ok {
		p := filepath.Join(dir, override)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
		return "", false
	}
	if solo {
		if override, ok := overrides[""]; ok {
			p := filepath.Join(dir, override)
			if _, err := os.Stat(p); err == nil {
				return p, true
			}
			return "", false
		}
	}
	for _, c := range candidates {
		p := filepath.Join(unitDir, c)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// rustCoverage reads the total line coverage percentage for every
// independent Cargo crate/workspace root discovered under dir (see
// detect.RustCrateDirs), averaging across crates that produced a result —
// mirroring how tsCoverage averages across TS packages. Returns a map of
// crate dir -> its resolved lcov export path (when wantLCOV is set and one
// was resolved for that crate), for patch coverage.
func rustCoverage(ctx context.Context, dir, workDir string, mode Mode, exclude []string, reportOverrides, lcovReportOverrides RustReportOverrides, wantLCOV bool) (float64, map[string]string, bool) {
	crateDirs, err := detect.RustCrateDirs(dir, exclude)
	if err != nil || len(crateDirs) == 0 {
		return 0, nil, false
	}
	solo := len(crateDirs) == 1

	var total, count float64
	lcovPaths := map[string]string{}
	for i, crateDir := range crateDirs {
		rel, err := filepath.Rel(dir, crateDir)
		if err != nil {
			continue
		}
		if rel == "." {
			rel = ""
		}
		pct, lcovPath, ok := rustCoverageOne(ctx, dir, crateDir, rel, workDir, mode, reportOverrides, lcovReportOverrides, solo, wantLCOV, i)
		if !ok {
			continue
		}
		total += pct
		count++
		if lcovPath != "" {
			lcovPaths[crateDir] = lcovPath
		}
	}
	if count == 0 {
		return 0, nil, false
	}
	return total / count, lcovPaths, true
}

// rustCoverageOne reads one crate's total line coverage percentage from a
// cargo-llvm-cov JSON export, either running `cargo llvm-cov` itself
// (ModeRun — requires cargo-llvm-cov already installed, a cargo subcommand
// like cargo-audit/cargo-deny that bulwark doesn't auto-install) or parsing
// an existing export another step already produced (ModeSkip — needs no
// tool installed at all, since nothing is executed).
//
// When wantLCOV is set, it also resolves an lcov export for patch coverage:
// under ModeSkip this is just another findReportForCrate lookup; under
// ModeRun, `cargo llvm-cov --no-report` runs the tests exactly once, keeping
// raw profile data on disk, and both the JSON and lcov reports are then
// regenerated from that same profile via `--no-run` — no second test
// execution.
func rustCoverageOne(ctx context.Context, dir, crateDir, crateRelDir, workDir string, mode Mode, reportOverrides, lcovReportOverrides RustReportOverrides, solo, wantLCOV bool, idx int) (float64, string, bool) {
	var data []byte
	var lcovPath string
	switch mode {
	case ModeSkip:
		found, ok := findReportForUnit(dir, crateDir, crateRelDir, reportOverrides, solo, rustReportCandidates)
		if !ok {
			return 0, "", false
		}
		d, err := os.ReadFile(found) // #nosec G304 -- found is resolved from bulwark's own candidate list or an explicit CLI flag, not user input
		if err != nil {
			return 0, "", false
		}
		data = d
		if wantLCOV {
			if p, ok := findReportForUnit(dir, crateDir, crateRelDir, lcovReportOverrides, solo, rustLCOVReportCandidates); ok {
				lcovPath = p
			}
		}
	default:
		if !executil.Available("cargo-llvm-cov") {
			return 0, "", false
		}
		if !wantLCOV {
			r := executil.Run(ctx, crateDir, "cargo", "llvm-cov", "--summary-only", "--json")
			if !r.Ok() {
				return 0, "", false
			}
			data = []byte(r.Output)
			break
		}
		if r := executil.Run(ctx, crateDir, "cargo", "llvm-cov", "--no-report"); !r.Ok() {
			return 0, "", false
		}
		r := executil.Run(ctx, crateDir, "cargo", "llvm-cov", "--no-run", "--summary-only", "--json")
		if !r.Ok() {
			return 0, "", false
		}
		data = []byte(r.Output)
		lcovOut := filepath.Join(workDir, fmt.Sprintf("rust-lcov-%d.info", idx))
		if r := executil.Run(ctx, crateDir, "cargo", "llvm-cov", "--no-run", "--lcov", "--output-path", lcovOut); r.Ok() {
			lcovPath = lcovOut
		}
	}

	var export llvmCovExport
	if err := json.Unmarshal(data, &export); err != nil || len(export.Data) == 0 {
		return 0, "", false
	}
	return export.Data[0].Totals.Lines.Percent, lcovPath, true
}

// istanbulSummary is the subset of Vitest/Istanbul's coverage-summary.json
// bulwark needs.
type istanbulSummary struct {
	Total struct {
		Lines struct {
			Pct float64 `json:"pct"`
		} `json:"lines"`
	} `json:"total"`
}

// tsLockfiles maps each recognized lockfile name to the package manager it
// identifies.
var tsLockfiles = map[string]string{
	"package-lock.json": "npm",
	"yarn.lock":         "yarn",
	"pnpm-lock.yaml":    "pnpm",
}

// resolvePackageManager inspects root for exactly one recognized lockfile
// (package-lock.json -> npm, yarn.lock -> yarn, pnpm-lock.yaml -> pnpm). If
// more than one is present at the same root — often a sign of stale/leftover
// files — resolution is ambiguous and returns ("", false) rather than
// silently guessing a priority order; the caller skips auto-detected install
// for that root entirely rather than picking one arbitrarily.
func resolvePackageManager(root string) (string, bool) {
	var found []string
	for file, manager := range tsLockfiles {
		if _, err := os.Stat(filepath.Join(root, file)); err == nil {
			found = append(found, manager)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// hasAnyLockfile reports whether dir contains any recognized lockfile,
// regardless of ambiguity — used to find workspace roots, where presence is
// what matters, not which single manager it identifies.
func hasAnyLockfile(dir string) bool {
	for file := range tsLockfiles {
		if _, err := os.Stat(filepath.Join(dir, file)); err == nil {
			return true
		}
	}
	return false
}

// tsWorkspaceRoots returns, for each pkgDir, the nearest ancestor directory
// (including pkgDir itself, not walking above dir) containing a recognized
// lockfile — deduped, since TSPackageDirs returns one entry per package.json
// including nested workspace members that all share one root lockfile, and
// an install command must run once per shared root, not once per member. A
// pkgDir with no lockfile anywhere in its ancestry up to dir is omitted —
// nothing to auto-install there.
func tsWorkspaceRoots(dir string, pkgDirs []string) []string {
	seen := map[string]bool{}
	var roots []string
	for _, pkgDir := range pkgDirs {
		d := pkgDir
		for {
			if hasAnyLockfile(d) {
				if !seen[d] {
					seen[d] = true
					roots = append(roots, d)
				}
				break
			}
			if d == dir {
				break
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	return roots
}

// tsInstall runs one install command per unique workspace root in roots,
// before any test:coverage script executes. override, when non-empty
// (cfg.TypeScript.Install), replaces auto-detection entirely and is run via
// a shell at every root — a free-form user-authored command legitimately
// needs shell semantics (&&, env expansion), unlike bulwark's other,
// hardcoded tool invocations. Failures are non-fatal here: tsCoverage's
// existing per-package soft-omission (hasCoverageScript / test:coverage
// failure) already handles a still-broken package after a failed or skipped
// install.
func tsInstall(ctx context.Context, roots []string, override string) {
	for _, root := range roots {
		if override != "" {
			executil.Run(ctx, root, "sh", "-c", override) // #nosec G204 -- override comes from the target repo's own .bulwark.yml, authored by whoever configured bulwark for that repo, not remote/untrusted input
			continue
		}
		manager, ok := resolvePackageManager(root)
		if !ok {
			continue
		}
		switch manager {
		case "npm":
			executil.Run(ctx, root, "npm", "ci")
		case "yarn":
			// Best-effort: corepack may already be enabled, or absent on an
			// older Node — either way, yarn install below still runs.
			executil.Run(ctx, root, "corepack", "enable")
			executil.Run(ctx, root, "yarn", "install", "--immutable")
		case "pnpm":
			executil.Run(ctx, root, "pnpm", "install", "--frozen-lockfile")
		}
	}
}

// tsCoverage looks for Vitest/Istanbul's coverage-summary.json in each
// detected package (the tool's own standard output location — unlike Go/Rust
// there's no bulwark-configurable override, since this path is already the
// de facto convention, not something projects vary). In ModeRun it first
// installs each workspace root's dependencies (auto-detected by lockfile, or
// install if set) — a fresh checkout (e.g. coverage baseline computation's
// throwaway git worktree) has no node_modules a prior step could have
// already installed — then runs each package's own "test:coverage" script
// (skipping packages that don't declare one) to produce that file; in
// ModeSkip it only reads a file a prior step already produced, running
// nothing.
func tsCoverage(ctx context.Context, dir string, exclude []string, mode Mode, install string) (float64, bool) {
	pkgDirs, err := detect.TSPackageDirs(dir, exclude)
	if err != nil || len(pkgDirs) == 0 {
		return 0, false
	}

	if mode == ModeRun {
		tsInstall(ctx, tsWorkspaceRoots(dir, pkgDirs), install)
	}

	var total, count float64
	for _, pkgDir := range pkgDirs {
		if mode == ModeRun {
			if !hasCoverageScript(pkgDir) {
				continue
			}
			if r := executil.Run(ctx, pkgDir, "npm", "run", "test:coverage"); !r.Ok() {
				continue
			}
		}
		summaryPath := filepath.Join(pkgDir, "coverage", "coverage-summary.json")
		data, err := os.ReadFile(summaryPath) // #nosec G304 -- summaryPath is a fixed relative path under a detected package dir, not user input
		if err != nil {
			continue
		}
		var summary istanbulSummary
		if err := json.Unmarshal(data, &summary); err != nil {
			continue
		}
		total += summary.Total.Lines.Pct
		count++
	}
	if count == 0 {
		return 0, false
	}
	return total / count, true
}

// packageJSON is the subset of package.json bulwark needs to detect whether
// a package already has a coverage script it can reuse.
type packageJSON struct {
	Scripts map[string]string `json:"scripts"`
}

func hasCoverageScript(pkgDir string) bool {
	data, err := os.ReadFile(filepath.Join(pkgDir, "package.json")) // #nosec G304 -- pkgDir comes from bulwark's own detection walk, not user input
	if err != nil {
		return false
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	_, ok := pkg.Scripts["test:coverage"]
	return ok
}
