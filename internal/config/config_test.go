package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadMissingFileReturnsDefault(t *testing.T) {
	got, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Fatalf("Load with no file = %+v, want Default() = %+v", got, Default())
	}
}

func TestLoadPartialOverrideKeepsOtherDefaults(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go:\n  exclude: [\"legacy\"]\n")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Default()
	want.Go.Exclude = []string{"legacy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
	// Rust/TypeScript/Semgrep, untouched by the file, must keep Default()'s values.
	if !got.Rust.Enabled || !got.TypeScript.Enabled || !got.Semgrep.Enabled {
		t.Fatalf("an untouched section lost its default enabled=true: %+v", got)
	}
}

func TestLoadDisableLanguage(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "rust:\n  enabled: false\n")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Rust.Enabled {
		t.Fatal("rust.enabled: false in the file did not disable Rust")
	}
	if !got.Go.Enabled || !got.TypeScript.Enabled {
		t.Fatalf("disabling rust incorrectly disabled another language: %+v", got)
	}
}

func TestLoadSemgrepConfigOverride(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "semgrep:\n  config: p/security-audit\n")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Semgrep.Config != "p/security-audit" {
		t.Fatalf("Semgrep.Config = %q, want %q", got.Semgrep.Config, "p/security-audit")
	}
	if !got.Semgrep.Enabled {
		t.Fatal("overriding config incorrectly disabled semgrep")
	}
}

// Zero-config users get a small noise-absorbing tolerance on the coverage
// gates, so a sub-rounding-error dip (86.1% vs baseline 86.1%) doesn't fail
// unrelated PRs. The aggregate and patch knobs default independently.
func TestDefaultCoverageTolerance(t *testing.T) {
	if got := Default().Coverage.Tolerance; got != 0.1 {
		t.Fatalf("Coverage.Tolerance default = %v, want 0.1", got)
	}
	if got := Default().Coverage.Patch.Tolerance; got != 0.1 {
		t.Fatalf("Coverage.Patch.Tolerance default = %v, want 0.1", got)
	}
}

// Tolerances that would invert the gate (negative) or silently disable it
// (NaN, ±Inf are all valid YAML floats) must be rejected at load time with
// an error naming the key, not flow into the comparison.
func TestLoadRejectsInvalidTolerance(t *testing.T) {
	cases := map[string]string{
		"negative aggregate": "coverage:\n  tolerance: -0.1\n",
		"nan aggregate":      "coverage:\n  tolerance: .nan\n",
		"inf aggregate":      "coverage:\n  tolerance: .inf\n",
		"negative patch":     "coverage:\n  patch:\n    tolerance: -1\n",
		"nan patch":          "coverage:\n  patch:\n    tolerance: .nan\n",
	}
	for name, yml := range cases {
		dir := t.TempDir()
		write(t, dir, yml)
		if _, err := Load(dir); err == nil {
			t.Errorf("%s: Load accepted an invalid tolerance (%q), want an error", name, yml)
		} else if !strings.Contains(err.Error(), "tolerance") {
			t.Errorf("%s: error should name the offending key, got: %v", name, err)
		}
	}
}

// An explicit tolerance: 0 tightens the gate to "fail any dip the report can
// display" — the merge must honor an explicitly-present zero, not treat it
// as "keep the default".
func TestLoadCoverageToleranceExplicitZero(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "coverage:\n  tolerance: 0\n")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Coverage.Tolerance != 0 {
		t.Fatalf("Coverage.Tolerance = %v, want 0 after explicit override", got.Coverage.Tolerance)
	}
	if !got.Coverage.Patch.Go.Enabled {
		t.Fatalf("setting coverage.tolerance incorrectly disabled patch coverage: %+v", got.Coverage)
	}
}

func TestLoadPatchCoverageDefaultsEnabled(t *testing.T) {
	got, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Coverage.Patch.Go.Enabled || !got.Coverage.Patch.Rust.Enabled || !got.Coverage.Patch.TypeScript.Enabled {
		t.Fatalf("patch coverage must default to enabled for every language: %+v", got.Coverage)
	}
}

func TestLoadPatchCoverageOptOut(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "coverage:\n  patch:\n    go:\n      enabled: false\n")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Coverage.Patch.Go.Enabled {
		t.Fatal("coverage.patch.go.enabled: false in the file did not disable Go patch coverage")
	}
	if !got.Coverage.Patch.Rust.Enabled || !got.Coverage.Patch.TypeScript.Enabled {
		t.Fatalf("disabling go patch coverage incorrectly disabled another language: %+v", got.Coverage)
	}
}

func TestLoadTypeScriptInstallOverride(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "typescript:\n  install: \"corepack enable && yarn install --immutable\"\n")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TypeScript.Install != "corepack enable && yarn install --immutable" {
		t.Fatalf("TypeScript.Install = %q, want the configured override", got.TypeScript.Install)
	}
	// TypeScriptLanguage's embedded Language fields must still merge onto
	// Default() normally alongside the new Install field.
	if !got.TypeScript.Enabled {
		t.Fatal("setting typescript.install incorrectly disabled TypeScript")
	}
}

func TestLoadTypeScriptInstallDefaultsEmpty(t *testing.T) {
	got, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TypeScript.Install != "" {
		t.Fatalf("TypeScript.Install = %q, want empty (auto-detect) by default", got.TypeScript.Install)
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "rust: [this is not a mapping\n")

	if _, err := Load(dir); err == nil {
		t.Fatal("expected an error parsing invalid YAML, got nil")
	}
}

func write(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
