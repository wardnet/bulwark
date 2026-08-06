// Package config loads .bulwark.yml, an optional, purely opt-out config file:
// bulwark's default (no file present) is to scan everything it detects with
// every check enabled. The file can only narrow that — disable a language's
// checks entirely, exclude specific paths from ecosystem/package detection,
// override Semgrep's ruleset, or adjust the coverage gates' noise tolerance
// — not tune severity or suppress individual findings (that's what a fix-up
// pass + #nosec/nosemgrep annotations in the scanned repo itself are for).
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

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
type Coverage struct {
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

// Config is bulwark's full, resolved configuration for one scan.
type Config struct {
	Rust       Language           `yaml:"rust"`
	TypeScript TypeScriptLanguage `yaml:"typescript"`
	Go         Language           `yaml:"go"`
	Semgrep    Semgrep            `yaml:"semgrep"`
	Coverage   Coverage           `yaml:"coverage"`
}

// Default returns bulwark's zero-config behavior: every language and Semgrep
// enabled, no excludes, Semgrep's ruleset set to "auto", every language's
// patch-coverage gate enabled.
func Default() Config {
	return Config{
		Rust:       Language{Enabled: true},
		TypeScript: TypeScriptLanguage{Language: Language{Enabled: true}},
		Go:         Language{Enabled: true},
		Semgrep:    Semgrep{Enabled: true, Config: "auto"},
		Coverage: Coverage{
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
	return cfg, nil
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
