// Package config loads .bulwark.yml, an optional config file: bulwark's
// default (no file present) is to scan everything it detects with every
// check enabled and to produce coverage itself. The file cannot tune
// severity or suppress individual findings (that's what a fix-up pass +
// #nosec/nosemgrep annotations in the scanned repo itself are for). What it
// can do falls in two groups:
//
//   - Opt out: disable a language's checks entirely, exclude specific paths
//     from ecosystem/package detection, override Semgrep's ruleset, adjust
//     the coverage gates' noise tolerance.
//   - Describe the repo's pipeline: coverage.source says whether bulwark or
//     a prior CI job owns coverage production, and coverage.{go,rust} locate
//     that job's reports. These aren't narrowing anything — they're facts
//     about how the repo is built that every invocation in it shares, which
//     is exactly why they belong in a file at the scan root rather than in a
//     flag each caller has to remember to repeat.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the config file bulwark looks for at the scan root.
const FileName = ".bulwark.yml"

// Language is the opt-out surface for one of the three supported ecosystems.
type Language struct {
	Enabled bool     `yaml:"enabled"`
	Exclude []string `yaml:"exclude"`
}

// TypeScriptLanguage extends Language with TS-only coverage install
// configuration.
type TypeScriptLanguage struct {
	Language `yaml:",inline"`
	// Install overrides coverage's install-command auto-detection (npm ci /
	// corepack enable && yarn install --immutable / pnpm install
	// --frozen-lockfile, chosen by the root's lockfile) with an explicit
	// shell command. Needed for Corepack-pinned or otherwise nonstandard
	// install flows auto-detection can't infer, or to resolve an ambiguous
	// multi-lockfile root that auto-detection otherwise skips. Only
	// consulted by coverage (internal/coverage), never by scan. Unset means:
	// use auto-detection, falling back to no install step if no single
	// recognized lockfile is found.
	Install string `yaml:"install,omitempty"`
}

// Semgrep is the opt-out/override surface for the Semgrep check.
type Semgrep struct {
	Enabled bool   `yaml:"enabled"`
	Config  string `yaml:"config"`
}

// Source names who produces the coverage data bulwark gates on. It is
// deliberately not phrased as "run tests or not": that framing describes
// bulwark's own behavior, while the decision a repo actually makes is which
// side of the pipeline owns coverage production. A repo whose CI already
// runs an instrumented test job owns it; a repo with no such job hands the
// job to bulwark.
type Source string

const (
	// SourceRun has bulwark execute each ecosystem's test suite itself (`go
	// test -coverprofile`, `cargo llvm-cov`, a package's `test:coverage`
	// script). The default, and the right one for local dev and for a repo
	// whose CI has no separate instrumented test job: one command, nothing
	// to sequence beforehand.
	SourceRun Source = "run"
	// SourceReport has a prior job own coverage production; bulwark never
	// executes anything and only parses the report that job already wrote.
	// This mirrors how Codecov and Sonar work. Use it whenever a test job
	// already runs with coverage instrumentation on, so the suite isn't
	// executed a second (or third) time.
	SourceReport Source = "report"
)

// Reports maps a discovered unit directory — a Rust crate/workspace root, or
// a Go module root — relative to the scan root, to an explicit report path
// (also relative to the scan root). The empty-string key is the override
// applied when discovery finds exactly one such unit.
//
// It accepts either YAML shape, because the two cases want different
// ergonomics and forcing one on the other is how config files get verbose
// for the common case or unexpressive for the rare one:
//
//	report: coverage.out                 # single unit — stored under ""
//	report:                              # multi-unit — keyed by unit dir
//	  wctl: coverage/wctl.out
//	  sdk/wardnet-go: coverage/sdk.out
//
// Only consulted under SourceReport; under SourceRun bulwark writes its own
// reports and nothing here is read.
type Reports map[string]string

// UnmarshalYAML accepts a bare scalar (the single-unit form, stored under
// the "" key that findReportForUnit already reserves for exactly that) or a
// mapping of unit dir to path.
//
// An explicit null (`report:` with no value) and an explicit empty string
// (`report: ""`) are both unset rather than an empty path. An empty path
// would join to the scan root directory itself, and since os.Stat succeeds on
// a directory the lookup would report the scan root as a *found* report —
// neither missing nor falling back to the conventional candidates, just
// silently wrong. Treating both spellings as absent is the only reading that
// can't produce that.
func (r *Reports) UnmarshalYAML(value *yaml.Node) error {
	switch {
	case value.Tag == "!!null":
		*r = nil
		return nil
	case value.Kind == yaml.ScalarNode:
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		if strings.TrimSpace(s) == "" {
			*r = nil
			return nil
		}
		*r = Reports{"": s}
		return nil
	case value.Kind == yaml.MappingNode:
		m := map[string]string{}
		if err := value.Decode(&m); err != nil {
			return err
		}
		*r = m
		return nil
	default:
		return fmt.Errorf("line %d: expected a report path or a mapping of unit directory to report path", value.Line)
	}
}

// GoCoverage carries Go's coverage-production settings — where an
// already-produced profile lives, when a prior job owns production.
type GoCoverage struct {
	// Report locates each Go module's `go test -coverprofile` output.
	// Usually unnecessary: each discovered module's own
	// coverage.out/cover.out/c.out is found without configuration.
	Report Reports `yaml:"report,omitempty"`
}

// RustCoverage carries Rust's coverage-production settings. Rust needs two
// paths where Go needs one: cargo-llvm-cov's JSON export has no per-line
// data, so the aggregate percentage and the patch gate read different files.
type RustCoverage struct {
	// Report locates each crate's `cargo llvm-cov --json` export, which the
	// aggregate percentage is read from. Default per crate:
	// coverage/llvm-cov.json, llvm-cov.json, target/llvm-cov/llvm-cov.json.
	Report Reports `yaml:"report,omitempty"`
	// LCOV locates each crate's `cargo llvm-cov --lcov` export, which patch
	// coverage's per-line hit data is read from. Default per crate:
	// coverage/lcov.info, lcov.info, target/llvm-cov/lcov.info.
	LCOV Reports `yaml:"lcov,omitempty"`
}

// PatchLanguage is the opt-out surface for one language's patch-coverage gate.
type PatchLanguage struct {
	Enabled bool `yaml:"enabled"`
}

// PatchCoverage is the opt-out surface for the patch-coverage gate, per
// language. Patch coverage has no threshold of its own — it always gates
// against that language's existing aggregate baseline (patch% >=
// baseline% - tolerance).
type PatchCoverage struct {
	Rust       PatchLanguage `yaml:"rust"`
	TypeScript PatchLanguage `yaml:"typescript"`
	Go         PatchLanguage `yaml:"go"`
	// Tolerance is the patch gate's own dip allowance in percentage points,
	// deliberately independent of Coverage.Tolerance: patch% is an exact
	// hit/total ratio with no measurement noise, but the baseline it gates
	// against is a noisy aggregate — hence a small default of its own.
	// Keeping the knobs separate means raising the aggregate tolerance for a
	// noisy test suite never silently weakens the untested-new-code check.
	Tolerance float64 `yaml:"tolerance"`
}

// Coverage is the opt-out/override surface for coverage gating.
//
// There is deliberately no `typescript:` section here. Istanbul/Vitest write
// <pkgDir>/coverage/{coverage-summary.json,lcov.info} by fixed convention,
// not by project-varying choice, so there has never been anything for an
// override to say — the existing no-override precedent for TS carries over
// unchanged.
type Coverage struct {
	// Source says who produces the coverage data: bulwark itself
	// (SourceRun, the default) or a prior CI job (SourceReport). This lives
	// in .bulwark.yml rather than in a CLI flag or action input because it
	// is a property of how the repo's pipeline is built, not of a single
	// invocation — the same answer holds for every run in that repo.
	//
	// One invocation deliberately ignores it: computing a baseline at a
	// historical main commit always runs tests, because that commit's
	// throwaway worktree contains no CI-produced report to read. See
	// cmd/bulwark/coverage.go's computeBaselineAt.
	Source Source `yaml:"source"`
	// Go and Rust locate already-produced reports, for SourceReport. Both
	// are usually unnecessary — each language's conventional per-unit paths
	// are searched without configuration.
	Go    GoCoverage    `yaml:"go"`
	Rust  RustCoverage  `yaml:"rust"`
	Patch PatchCoverage `yaml:"patch"`
	// Tolerance is the number of percentage points a language's aggregate
	// coverage may dip below its baseline before the gate fails. Coverage
	// measurement is noisy at the sub-tenth level (timing-dependent
	// instrumentation, tool version drift), so a strict comparison fails PRs
	// with "86.1% vs baseline 86.1%, regressed 0.0%" — a dip smaller than
	// the displayed precision — even when the PR touches no code in that
	// language. The comparison happens at the report's display precision
	// (tenths), so 0 means "fail any dip the report can show" rather than
	// "fail any bit-level difference". Within-tolerance dips are also
	// restored to the prior value when a main run records a new baseline, so
	// tolerated dips can't compound across merges. The patch gate has its
	// own knob (Patch.Tolerance).
	Tolerance float64 `yaml:"tolerance"`
}

// Toolchain is the override surface for language-toolchain provisioning —
// making sure the Go, Rust and Node runtimes bulwark's checks need are
// present at the version the repo requires.
//
// It carries no version by default, and that is the design rather than an
// omission. The versions come from the files that already state them and that
// the language's own tooling already enforces: the `go` and `toolchain`
// directives in every discovered go.mod, the channel in rust-toolchain.toml,
// engines.node or .nvmrc. Restating one here could only agree redundantly or
// drift silently, and a stale duplicate is worse than no duplicate because it
// reads as authoritative. The fields below exist for the case where a repo
// needs a deliberate local exception to what its manifests say — an override,
// not a source of truth.
type Toolchain struct {
	// Enabled turns provisioning off while keeping the diagnostics. Set false
	// on an air-gapped runner, or one where every toolchain is already
	// provisioned by the image: bulwark still reports what each ecosystem
	// declares and whether PATH satisfies it, but never downloads or installs.
	Enabled bool `yaml:"enabled"`
	// Go, Rust and Node override the version read from the manifests. Unset
	// (the intended state) means "use what the repo declares".
	Go   string `yaml:"go,omitempty"`
	Rust string `yaml:"rust,omitempty"`
	Node string `yaml:"node,omitempty"`
}

// Config is bulwark's full, resolved configuration for one scan.
type Config struct {
	Rust       Language           `yaml:"rust"`
	TypeScript TypeScriptLanguage `yaml:"typescript"`
	Go         Language           `yaml:"go"`
	Semgrep    Semgrep            `yaml:"semgrep"`
	Coverage   Coverage           `yaml:"coverage"`
	Toolchain  Toolchain          `yaml:"toolchain"`
}

// Default returns bulwark's zero-config behavior: every language and Semgrep
// enabled, no excludes, Semgrep's ruleset set to "auto", bulwark producing
// coverage itself, every language's patch-coverage gate enabled, and
// toolchain provisioning on with every version taken from the repo's own
// manifests.
func Default() Config {
	return Config{
		Rust:       Language{Enabled: true},
		TypeScript: TypeScriptLanguage{Language: Language{Enabled: true}},
		Go:         Language{Enabled: true},
		Semgrep:    Semgrep{Enabled: true, Config: "auto"},
		Toolchain:  Toolchain{Enabled: true},
		Coverage: Coverage{
			Source:    SourceRun,
			Tolerance: 0.1,
			Patch: PatchCoverage{
				Rust:       PatchLanguage{Enabled: true},
				TypeScript: PatchLanguage{Enabled: true},
				Go:         PatchLanguage{Enabled: true},
				Tolerance:  0.1,
			},
		},
	}
}

// Load reads .bulwark.yml from root if present, merging it onto Default().
// A missing file is not an error — it's the common case. Merge semantics:
// yaml.Unmarshal only overwrites fields explicitly present in the file, so a
// section omitted entirely (or a key omitted within a present section) keeps
// its Default() value rather than being zeroed.
func Load(root string) (Config, error) {
	cfg := Default()
	path := filepath.Join(root, FileName)
	data, err := os.ReadFile(path) // #nosec G304 -- root is the CLI's own --dir flag, supplied by whoever runs bulwark, not untrusted remote input
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := validateTolerances(cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := validateSource(cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// validateSource rejects any coverage.source other than the two defined
// values. Silently falling back to "run" on a typo ("reports", "skip") would
// have bulwark execute a full test suite in a CI job built on the assumption
// that it wouldn't — slow, and on a runner without the toolchain, a measured
// nothing that reads as a real result.
func validateSource(cfg Config) error {
	switch cfg.Coverage.Source {
	case SourceRun, SourceReport:
		return nil
	default:
		return fmt.Errorf("coverage.source must be %q or %q, got %q", SourceRun, SourceReport, cfg.Coverage.Source)
	}
}

// validateTolerances rejects tolerance values that silently invert or
// disable the coverage gates: a negative tolerance fails languages whose
// coverage held steady or improved ("regressed -0.3%"), and NaN (or ±Inf,
// both valid YAML) makes the gate comparison unconditionally false, turning
// both gates off while still printing [PASS] lines.
func validateTolerances(cfg Config) error {
	for name, tol := range map[string]float64{
		"coverage.tolerance":       cfg.Coverage.Tolerance,
		"coverage.patch.tolerance": cfg.Coverage.Patch.Tolerance,
	} {
		if math.IsNaN(tol) || math.IsInf(tol, 0) || tol < 0 {
			return fmt.Errorf("%s must be a finite, non-negative number of percentage points, got %v", name, tol)
		}
	}
	return nil
}

// AllExcludes merges every language's exclude list — used by callers (scan,
// coverage) whose initial ecosystem-detection pass doesn't yet know which
// language a given excluded directory belongs to.
func (c Config) AllExcludes() []string {
	var out []string
	out = append(out, c.Rust.Exclude...)
	out = append(out, c.TypeScript.Exclude...)
	out = append(out, c.Go.Exclude...)
	return out
}
