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
)

func runDiffReport(t *testing.T, current, baseline map[string]float64) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	err := diffReport(cmd, current, baseline)
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
