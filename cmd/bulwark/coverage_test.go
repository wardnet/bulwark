package main

import (
	"bytes"
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
