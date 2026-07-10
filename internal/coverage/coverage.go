// Package coverage computes per-language test coverage percentages for
// whatever ecosystems are detected under a directory, reusing each
// language's own existing coverage tooling rather than reimplementing it.
package coverage

import (
	"context"
	"encoding/json"
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

// ReportPaths overrides the default report-file search candidates per
// language, for a repo whose coverage output doesn't land at one of the
// conventional locations findReport checks. A zero value uses the built-in
// candidate list for that language. Only meaningful with ModeSkip.
type ReportPaths struct {
	Go   string
	Rust string
}

// Compute returns a coverage percentage per detected ecosystem under dir.
// An ecosystem is silently omitted (not an error) when its coverage tooling
// isn't available or produces no measurable result — coverage tooling is
// more varied across projects than a linter, so bulwark reports what it can
// rather than failing the whole run over one package's missing test script.
//
// The initial ecosystem-detection pass uses cfg.AllExcludes() (it doesn't yet
// know which language a given excluded directory belongs to), but each
// language-specific pass below uses only that language's own exclude list —
// a Rust-only exclude must never cause a real TypeScript package to be
// silently dropped from coverage, matching how cmd/bulwark/scan.go scopes
// excludes per language.
func Compute(ctx context.Context, dir string, cfg config.Config, mode Mode, reports ReportPaths) (map[string]float64, error) {
	ecosystems, err := detect.Ecosystems(dir, cfg.AllExcludes())
	if err != nil {
		return nil, err
	}

	report := map[string]float64{}
	for _, e := range ecosystems {
		var pct float64
		var ok bool
		switch e {
		case detect.Rust:
			pct, ok = rustCoverage(ctx, dir, mode, reports.Rust)
		case detect.Go:
			pct, ok = goCoverage(ctx, dir, mode, reports.Go)
		case detect.TypeScript:
			pct, ok = tsCoverage(ctx, dir, cfg.TypeScript.Exclude, mode)
		}
		if ok {
			report[string(e)] = pct
		}
	}
	return report, nil
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
// line, either running `go test -coverprofile` itself (ModeRun) or parsing
// an existing profile another step already produced (ModeSkip) — either way
// the profile is fed through the same `go tool cover -func` formatting step,
// which does not re-run any tests.
func goCoverage(ctx context.Context, dir string, mode Mode, reportPath string) (float64, bool) {
	var profile string
	switch mode {
	case ModeSkip:
		found, ok := findReport(dir, reportPath, goReportCandidates)
		if !ok {
			return 0, false
		}
		profile = found
	default:
		tmp, err := os.MkdirTemp("", "bulwark-gocov-*")
		if err != nil {
			return 0, false
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		profile = filepath.Join(tmp, "cover.out")
		if r := executil.Run(ctx, dir, "go", "test", "-coverprofile="+profile, "./..."); !r.Ok() {
			return 0, false
		}
	}

	r := executil.Run(ctx, dir, "go", "tool", "cover", "-func="+profile)
	if !r.Ok() {
		return 0, false
	}
	return parseGoTotalPercent(r.Output)
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

// rustCoverage reads the workspace's total line coverage percentage from a
// cargo-llvm-cov JSON export, either running `cargo llvm-cov` itself
// (ModeRun — requires cargo-llvm-cov already installed, a cargo subcommand
// like cargo-audit/cargo-deny that bulwark doesn't auto-install) or parsing
// an existing export another step already produced (ModeSkip — needs no
// tool installed at all, since nothing is executed).
func rustCoverage(ctx context.Context, dir string, mode Mode, reportPath string) (float64, bool) {
	var data []byte
	switch mode {
	case ModeSkip:
		found, ok := findReport(dir, reportPath, rustReportCandidates)
		if !ok {
			return 0, false
		}
		d, err := os.ReadFile(found) // #nosec G304 -- found is resolved from bulwark's own candidate list or an explicit CLI flag, not user input
		if err != nil {
			return 0, false
		}
		data = d
	default:
		if !executil.Available("cargo-llvm-cov") {
			return 0, false
		}
		r := executil.Run(ctx, dir, "cargo", "llvm-cov", "--summary-only", "--json")
		if !r.Ok() {
			return 0, false
		}
		data = []byte(r.Output)
	}

	var export llvmCovExport
	if err := json.Unmarshal(data, &export); err != nil || len(export.Data) == 0 {
		return 0, false
	}
	return export.Data[0].Totals.Lines.Percent, true
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

// tsCoverage looks for Vitest/Istanbul's coverage-summary.json in each
// detected package (the tool's own standard output location — unlike Go/Rust
// there's no bulwark-configurable override, since this path is already the
// de facto convention, not something projects vary). In ModeRun it first
// runs the package's own "test:coverage" script (skipping packages that
// don't declare one) to produce that file; in ModeSkip it only reads a file
// a prior step already produced, running nothing.
func tsCoverage(ctx context.Context, dir string, exclude []string, mode Mode) (float64, bool) {
	pkgDirs, err := detect.TSPackageDirs(dir, exclude)
	if err != nil || len(pkgDirs) == 0 {
		return 0, false
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
