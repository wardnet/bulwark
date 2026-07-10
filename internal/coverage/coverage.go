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
func Compute(ctx context.Context, dir string, cfg config.Config) (map[string]float64, error) {
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
			pct, ok = rustCoverage(ctx, dir)
		case detect.Go:
			pct, ok = goCoverage(ctx, dir)
		case detect.TypeScript:
			pct, ok = tsCoverage(ctx, dir, cfg.TypeScript.Exclude)
		}
		if ok {
			report[string(e)] = pct
		}
	}
	return report, nil
}

// goCoverage runs `go test -coverprofile` and parses the total percentage
// from `go tool cover -func`'s summary line.
func goCoverage(ctx context.Context, dir string) (float64, bool) {
	tmp, err := os.MkdirTemp("", "bulwark-gocov-*")
	if err != nil {
		return 0, false
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	profile := filepath.Join(tmp, "cover.out")

	if r := executil.Run(ctx, dir, "go", "test", "-coverprofile="+profile, "./..."); !r.Ok() {
		return 0, false
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

// rustCoverage runs cargo-llvm-cov's JSON summary and reads the workspace's
// total line coverage percentage. Requires cargo-llvm-cov to already be
// installed (a cargo subcommand, like cargo-audit/cargo-deny — not something
// bulwark auto-installs, since it isn't a plain go-installable/npm-installable
// tool bulwark can pin the same way).
func rustCoverage(ctx context.Context, dir string) (float64, bool) {
	if !executil.Available("cargo-llvm-cov") {
		return 0, false
	}
	r := executil.Run(ctx, dir, "cargo", "llvm-cov", "--summary-only", "--json")
	if !r.Ok() {
		return 0, false
	}
	var export llvmCovExport
	if err := json.Unmarshal([]byte(r.Output), &export); err != nil || len(export.Data) == 0 {
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

// tsCoverage looks for a "test:coverage" script in each detected package and,
// if present, runs it and reads Vitest/Istanbul's coverage-summary.json.
// Packages without that script are skipped, not failed.
func tsCoverage(ctx context.Context, dir string, exclude []string) (float64, bool) {
	pkgDirs, err := detect.TSPackageDirs(dir, exclude)
	if err != nil || len(pkgDirs) == 0 {
		return 0, false
	}

	var total, count float64
	for _, pkgDir := range pkgDirs {
		if !hasCoverageScript(pkgDir) {
			continue
		}
		if r := executil.Run(ctx, pkgDir, "npm", "run", "test:coverage"); !r.Ok() {
			continue
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
