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
	"strconv"
	"strings"

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

// RustReportOverrides maps a discovered Rust crate directory (relative to
// --dir) to an explicit report path override (also relative to --dir), for
// use with ModeSkip. The empty-string key ("") is the override applied when
// Rust discovery (detect.RustCrateDirs) finds exactly one crate — this
// preserves the single-crate CLI usage of a bare `--rust-report <path>`
// unchanged even though the flag is now repeatable/keyed for multi-crate
// repos.
type RustReportOverrides map[string]string

// ReportPaths overrides the default report-file search candidates per
// language, for a repo whose coverage output doesn't land at one of the
// conventional locations findReport/findReportForCrate checks. A zero value
// uses the built-in candidate list for that language. Only meaningful with
// ModeSkip.
type ReportPaths struct {
	Go       string
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
	GoProfile  string
	RustLCOV   map[string]string // Rust crate dir -> its cargo-llvm-cov lcov export
	TSLCOV     map[string]string // TS package dir -> its coverage/lcov.info
	ModuleName string            // this module's path, needed to resolve GoProfile's package-qualified file names
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
			var profilePath string
			pct, profilePath, ok = goCoverage(ctx, dir, workDir, mode, reports.Go)
			if ok && want.Go {
				sources.GoProfile = profilePath
				sources.ModuleName = moduleName(ctx, dir)
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

// moduleName returns this repo's Go module path (e.g. "wardnet/bulwark"),
// needed to strip the package-qualified prefix `go tool cover`/x/tools/cover
// put on each file name in a coverage profile. A lookup failure just means
// Go patch coverage can't be computed (ModuleName stays empty) — never a
// fatal error, matching PatchSources' overall soft-omission contract.
func moduleName(ctx context.Context, dir string) string {
	r := executil.Run(ctx, dir, "go", "list", "-m")
	if !r.Ok() {
		return ""
	}
	return strings.TrimSpace(r.Output)
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

// goCoverage gets the total percentage from `go tool cover -func`'s summary
// line, either running `go test -coverprofile` itself into workDir (ModeRun)
// or parsing an existing profile another step already produced (ModeSkip) —
// either way the profile is fed through the same `go tool cover -func`
// formatting step, which does not re-run any tests. The resolved profile
// path is also returned (workDir under ModeRun persists until the caller's
// Compute-returned cleanup runs, so patch coverage can reparse it later
// without a second `go test` invocation).
func goCoverage(ctx context.Context, dir, workDir string, mode Mode, reportPath string) (float64, string, bool) {
	var profile string
	switch mode {
	case ModeSkip:
		found, ok := findReport(dir, reportPath, goReportCandidates)
		if !ok {
			return 0, "", false
		}
		profile = found
	default:
		profile = filepath.Join(workDir, "cover.out")
		if r := executil.Run(ctx, dir, "go", "test", "-coverprofile="+profile, "./..."); !r.Ok() {
			return 0, "", false
		}
	}

	r := executil.Run(ctx, dir, "go", "tool", "cover", "-func="+profile)
	if !r.Ok() {
		return 0, "", false
	}
	pct, ok := parseGoTotalPercent(r.Output)
	return pct, profile, ok
}

// parseGoTotalPercent extracts the percentage from `go tool cover -func`'s
// final summary line, which has the fixed form:
//
//	total:						(statements)		87.3%
func parseGoTotalPercent(output string) (float64, bool) {
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "total:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return 0, false
		}
		last := strings.TrimSuffix(fields[len(fields)-1], "%")
		pct, err := strconv.ParseFloat(last, 64)
		if err != nil {
			return 0, false
		}
		return pct, true
	}
	return 0, false
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

// findReportForCrate resolves a coverage report path for one discovered Rust
// crate directory: first any override keyed by crateRelDir (crateDir's path
// relative to dir), then — only when solo is true, i.e. exactly one crate
// was discovered — the override keyed by "", then the candidate list
// resolved relative to crateDir itself (matching where cargo llvm-cov
// naturally writes output when run in-place inside each crate directory).
// Override path values are resolved relative to dir, preserving the
// documented "relative to --dir" contract for the override strings
// themselves.
func findReportForCrate(dir, crateDir, crateRelDir string, overrides RustReportOverrides, solo bool, candidates []string) (string, bool) {
	if override, ok := overrides[crateRelDir]; ok {
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
		p := filepath.Join(crateDir, c)
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
		found, ok := findReportForCrate(dir, crateDir, crateRelDir, reportOverrides, solo, rustReportCandidates)
		if !ok {
			return 0, "", false
		}
		d, err := os.ReadFile(found) // #nosec G304 -- found is resolved from bulwark's own candidate list or an explicit CLI flag, not user input
		if err != nil {
			return 0, "", false
		}
		data = d
		if wantLCOV {
			if p, ok := findReportForCrate(dir, crateDir, crateRelDir, lcovReportOverrides, solo, rustLCOVReportCandidates); ok {
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
