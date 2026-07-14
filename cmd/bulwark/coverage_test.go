package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"wardnet/bulwark/internal/config"
	"wardnet/bulwark/internal/detect"
)

// unmeasuredLanguages is the single source of truth for "detected but not
// measured this run" — the predicate the unmeasured warning, the
// carry-forward trigger, and the merge all share, so it cannot drift between
// them. An undetected language in the report (shouldn't happen, but) is not
// unmeasured; a detected one absent from the report is.
func TestUnmeasuredLanguages(t *testing.T) {
	got := unmeasuredLanguages(
		[]detect.Ecosystem{detect.Rust, detect.TypeScript, detect.Go},
		map[string]float64{"rust": 86},
	)
	if !reflect.DeepEqual(got, []string{"go", "typescript"}) {
		t.Errorf("unmeasuredLanguages = %v, want [go typescript] (sorted)", got)
	}
	if got := unmeasuredLanguages([]detect.Ecosystem{detect.Rust}, map[string]float64{"rust": 86}); len(got) != 0 {
		t.Errorf("fully measured: unmeasuredLanguages = %v, want none", got)
	}
}

// mergeCarried is what makes a partial run safe to record as a baseline: a
// detected-but-unmeasured language keeps its entry from the prior lookup
// (the code is still there; only this run's measurement is missing), while
// anything the lookup couldn't fill is returned as missing so the caller
// warns instead of shrinking the baseline in silence. Measured values are
// never touched, and the input map is never mutated.
func TestMergeCarried(t *testing.T) {
	current := map[string]float64{"rust": 86}
	record, carried, missing := mergeCarried(current, []string{"go", "typescript"}, map[string]float64{"typescript": 93.8})

	want := map[string]float64{"rust": 86, "typescript": 93.8}
	if !reflect.DeepEqual(record, want) {
		t.Errorf("mergeCarried record = %v, want %v", record, want)
	}
	if !reflect.DeepEqual(carried, []string{"typescript"}) || !reflect.DeepEqual(missing, []string{"go"}) {
		t.Errorf("carried = %v missing = %v, want [typescript] / [go]", carried, missing)
	}
	if len(current) != 1 {
		t.Errorf("mergeCarried mutated its input: %v", current)
	}

	record, carried, missing = mergeCarried(current, nil, nil)
	if !reflect.DeepEqual(record, current) || len(carried) != 0 || len(missing) != 0 {
		t.Errorf("nothing unmeasured: record = %v carried = %v missing = %v, want record == current and empty lists", record, carried, missing)
	}
}

// enabledEcosystems makes `enabled: false` in .bulwark.yml behave exactly
// like source removal for the coverage gate: the language stops being
// "detected", so its baseline entry dies on the next record instead of being
// carried forward (and [UNMEASURED]-reported) forever.
func TestEnabledEcosystemsDropsDisabledLanguages(t *testing.T) {
	cfg := config.Default()
	cfg.TypeScript.Enabled = false
	got := enabledEcosystems([]detect.Ecosystem{detect.Rust, detect.TypeScript, detect.Go}, cfg)
	if !reflect.DeepEqual(got, []detect.Ecosystem{detect.Rust, detect.Go}) {
		t.Errorf("enabledEcosystems = %v, want [rust go] with typescript disabled", got)
	}
}

func runDiffReport(t *testing.T, current, baseline map[string]float64, detected ...detect.Ecosystem) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	err := diffReport(cmd, current, baseline, detected)
	return buf.String(), err
}

func TestDiffReportNewLanguage(t *testing.T) {
	out, err := runDiffReport(t, map[string]float64{"go": 50}, map[string]float64{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "[NEW]") || !strings.Contains(out, "go") {
		t.Fatalf("expected a [NEW] line for go, got: %q", out)
	}
}

// TestDiffReportDroppedLanguage guards the fix for a language present in the
// baseline but missing from current (its coverage tooling became unavailable,
// or a real regression removed it entirely from the measured set): it must be
// reported, not silently omitted — regression for the bug where diffReport
// only iterated current's keys and such a language was never mentioned at all.
func TestDiffReportDroppedLanguage(t *testing.T) {
	out, err := runDiffReport(t, map[string]float64{}, map[string]float64{"typescript": 85})
	if err != nil {
		t.Fatalf("a dropped language must not fail the check on its own, got error: %v", err)
	}
	if !strings.Contains(out, "[DROPPED]") || !strings.Contains(out, "typescript") {
		t.Fatalf("expected a [DROPPED] line mentioning typescript, got: %q", out)
	}
}

// A language that is still detected in the tree but produced no measurement
// this run (a path-filtered CI job skipped its coverage step — wardnet PR
// #892) is not "no longer measured": the code is right there. It must be
// reported as [UNMEASURED], reserving [DROPPED] for a language whose source
// actually left the tree. Neither fails the check on its own.
func TestDiffReportUnmeasuredWhenLanguageStillDetected(t *testing.T) {
	out, err := runDiffReport(t, map[string]float64{"rust": 86},
		map[string]float64{"rust": 85.8, "typescript": 93.8},
		detect.Rust, detect.TypeScript)
	if err != nil {
		t.Fatalf("an unmeasured-but-detected language must not fail the check, got error: %v", err)
	}
	if !strings.Contains(out, "[UNMEASURED]") || !strings.Contains(out, "not measured this run") {
		t.Fatalf("expected an [UNMEASURED] line for typescript, got: %q", out)
	}
	if strings.Contains(out, "[DROPPED]") {
		t.Fatalf("a detected language must not be reported as [DROPPED], got: %q", out)
	}
}

func TestDiffReportRegression(t *testing.T) {
	out, err := runDiffReport(t, map[string]float64{"go": 40}, map[string]float64{"go": 50})
	if err == nil {
		t.Fatal("expected an error for a regressed language")
	}
	if !strings.Contains(out, "[FAIL]") {
		t.Fatalf("expected a [FAIL] line, got: %q", out)
	}
}

func TestDiffReportPass(t *testing.T) {
	out, err := runDiffReport(t, map[string]float64{"go": 55}, map[string]float64{"go": 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "[PASS]") {
		t.Fatalf("expected a [PASS] line, got: %q", out)
	}
}

// TestDiffReportOnlyCountsRealRegressions confirms a dropped/new language
// never contributes to the failure count — only an actual measured decrease
// does, even when all three cases occur in the same run.
func TestDiffReportOnlyCountsRealRegressions(t *testing.T) {
	current := map[string]float64{"go": 40, "rust": 60}
	baseline := map[string]float64{"go": 50, "typescript": 85}
	out, err := runDiffReport(t, current, baseline)
	if err == nil || !strings.Contains(err.Error(), "1") {
		t.Fatalf("expected exactly 1 regressed language reported in the error, got: %v", err)
	}
	for _, want := range []string{"[FAIL]", "[NEW]", "[DROPPED]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got: %q", want, out)
		}
	}
}

func TestStatusPrefixColumnWidth(t *testing.T) {
	cases := map[string]string{
		"NEW":     "[NEW]     ",
		"DROPPED": "[DROPPED] ",
		"FAIL":    "[FAIL]    ",
		"PASS":    "[PASS]    ",
	}
	for tag, want := range cases {
		if got := statusPrefix(tag); got != want {
			t.Errorf("statusPrefix(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestFilterByExt(t *testing.T) {
	changed := map[string][]int{
		"main.go":       {1, 2},
		"lib.rs":        {3},
		"index.ts":      {4},
		"component.tsx": {5},
	}
	got := filterByExt(changed, []string{".ts", ".tsx"})
	want := map[string][]int{"index.ts": {4}, "component.tsx": {5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterByExt = %+v, want %+v", got, want)
	}
}

// TestTSPatchPercentDoesNotClobberAcrossPackages guards the fix for a bug
// where merging every TS package's LineHits into one shared map (via
// maps.Copy, keyed by package-relative path) let two packages with the same
// relative file name silently clobber each other's hit data under Go's
// unordered map iteration. tsPatchPercent must instead resolve each
// package's contribution independently, scoped to that package's own
// repo-relative prefix.
func TestTSPatchPercentDoesNotClobberAcrossPackages(t *testing.T) {
	dir := t.TempDir()
	webDir := filepath.Join(dir, "web")
	apiDir := filepath.Join(dir, "api")
	writeLCOV := func(coverageDir, sf string, hit int) {
		t.Helper()
		if err := os.MkdirAll(coverageDir, 0o755); err != nil {
			t.Fatal(err)
		}
		data := fmt.Sprintf("SF:%s\nDA:1,%d\nend_of_record\n", sf, hit)
		if err := os.WriteFile(filepath.Join(coverageDir, "lcov.info"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Two packages, each with its own src/index.ts at the same
	// package-relative path — only distinguishable by which package prefix
	// the changed file actually falls under.
	writeLCOV(filepath.Join(webDir, "coverage"), filepath.Join(webDir, "src", "index.ts"), 1)
	writeLCOV(filepath.Join(apiDir, "coverage"), filepath.Join(apiDir, "src", "index.ts"), 0)

	tsLCOV := map[string]string{
		webDir: filepath.Join(webDir, "coverage", "lcov.info"),
		apiDir: filepath.Join(apiDir, "coverage", "lcov.info"),
	}
	changed := map[string][]int{
		"web/src/index.ts": {1},
		"api/src/index.ts": {1},
	}

	hit, total := tsPatchPercent(dir, tsLCOV, changed)
	if total != 2 {
		t.Fatalf("total = %d, want 2 (one coverable changed line per package)", total)
	}
	if hit != 1 {
		t.Fatalf("hit = %d, want 1 (web's line is hit, api's is not — a clobbered merge would report 0 or 2, not 1)", hit)
	}
}

// TestRustPatchPercentDoesNotClobberAcrossPackages mirrors
// TestTSPatchPercentDoesNotClobberAcrossPackages for rustPatchPercent — two
// discovered crates, each with a file at the same crate-relative path, must
// not clobber each other's hit data.
func TestRustPatchPercentDoesNotClobberAcrossPackages(t *testing.T) {
	dir := t.TempDir()
	crateA := filepath.Join(dir, "crates", "a")
	crateB := filepath.Join(dir, "crates", "b")
	writeLCOV := func(coverageDir, sf string, hit int) {
		t.Helper()
		if err := os.MkdirAll(coverageDir, 0o755); err != nil {
			t.Fatal(err)
		}
		data := fmt.Sprintf("SF:%s\nDA:1,%d\nend_of_record\n", sf, hit)
		if err := os.WriteFile(filepath.Join(coverageDir, "lcov.info"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeLCOV(filepath.Join(crateA, "coverage"), filepath.Join(crateA, "src", "lib.rs"), 1)
	writeLCOV(filepath.Join(crateB, "coverage"), filepath.Join(crateB, "src", "lib.rs"), 0)

	rustLCOV := map[string]string{
		crateA: filepath.Join(crateA, "coverage", "lcov.info"),
		crateB: filepath.Join(crateB, "coverage", "lcov.info"),
	}
	changed := map[string][]int{
		"crates/a/src/lib.rs": {1},
		"crates/b/src/lib.rs": {1},
	}

	hit, total := rustPatchPercent(dir, rustLCOV, changed)
	if total != 2 {
		t.Fatalf("total = %d, want 2 (one coverable changed line per crate)", total)
	}
	if hit != 1 {
		t.Fatalf("hit = %d, want 1 (crate a's line is hit, crate b's is not — a clobbered merge would report 0 or 2, not 1)", hit)
	}
}

func TestParseRustReportOverrides(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   map[string]string
	}{
		{"nil for no values", nil, nil},
		{
			name:   "bare value stored under empty key",
			values: []string{"daemon/coverage/daemon-llvm-cov.json"},
			want:   map[string]string{"": "daemon/coverage/daemon-llvm-cov.json"},
		},
		{
			name:   "keyed value",
			values: []string{"daemon=daemon/coverage/daemon-llvm-cov.json"},
			want:   map[string]string{"daemon": "daemon/coverage/daemon-llvm-cov.json"},
		},
		{
			name:   "mixed bare and keyed",
			values: []string{"daemon=daemon/coverage/daemon-llvm-cov.json", "other=other/coverage/llvm-cov.json"},
			want: map[string]string{
				"daemon": "daemon/coverage/daemon-llvm-cov.json",
				"other":  "other/coverage/llvm-cov.json",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRustReportOverrides(tc.values)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestFormatReport(t *testing.T) {
	// Sorted, so the recorded-baseline line on main doesn't reshuffle between
	// runs over Go's map iteration order.
	got := formatReport(map[string]float64{"typescript": 93.94, "go": 58.5, "rust": 85.7})
	want := "go: 58.5%, rust: 85.7%, typescript: 93.9%"
	if got != want {
		t.Errorf("formatReport = %q, want %q", got, want)
	}
	if got := formatReport(map[string]float64{}); got != "" {
		t.Errorf("formatReport(empty) = %q, want \"\"", got)
	}
}
