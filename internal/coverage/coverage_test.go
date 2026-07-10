package coverage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseGoTotalPercent(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   float64
		wantOK bool
	}{
		{
			name: "typical go tool cover -func output",
			output: "wardnet/bulwark/cmd/bulwark/main.go:11:\tmain\t\t0.0%\n" +
				"total:\t\t\t\t\t(statements)\t\t18.5%\n",
			want:   18.5,
			wantOK: true,
		},
		{"no total line", "wardnet/bulwark/cmd/bulwark/main.go:11:\tmain\t\t0.0%\n", 0, false},
		{"empty output", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseGoTotalPercent(tc.output)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("parseGoTotalPercent(%q) = (%v, %v), want (%v, %v)", tc.output, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestLlvmCovExportParsing(t *testing.T) {
	data := []byte(`{"data":[{"totals":{"lines":{"count":100,"covered":87,"percent":87.3}}}]}`)
	var export llvmCovExport
	if err := json.Unmarshal(data, &export); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(export.Data) != 1 || export.Data[0].Totals.Lines.Percent != 87.3 {
		t.Fatalf("got %+v, want percent 87.3", export)
	}
}

func TestIstanbulSummaryParsing(t *testing.T) {
	data := []byte(`{"total":{"lines":{"total":50,"covered":42,"skipped":0,"pct":84}}}`)
	var summary istanbulSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if summary.Total.Lines.Pct != 84 {
		t.Fatalf("got %+v, want pct 84", summary)
	}
}

func TestFindReportOverride(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "my-custom-report.json", "{}")

	got, ok := findReport(dir, "my-custom-report.json", []string{"never-used.json"})
	if !ok || got != filepath.Join(dir, "my-custom-report.json") {
		t.Fatalf("findReport with override = (%q, %v), want the override path", got, ok)
	}
}

func TestFindReportOverrideMissing(t *testing.T) {
	dir := t.TempDir()
	if _, ok := findReport(dir, "does-not-exist.json", nil); ok {
		t.Fatal("findReport with a missing override path should report not-found, not fall back to candidates")
	}
}

func TestFindReportCandidateSearchOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "cover.out", "mode: set\n")

	got, ok := findReport(dir, "", []string{"coverage.out", "cover.out", "c.out"})
	if !ok || got != filepath.Join(dir, "cover.out") {
		t.Fatalf("findReport = (%q, %v), want the first existing candidate (cover.out)", got, ok)
	}
}

func TestFindReportNoCandidatesExist(t *testing.T) {
	dir := t.TempDir()
	if _, ok := findReport(dir, "", []string{"coverage.out", "cover.out"}); ok {
		t.Fatal("findReport should report not-found when none of the candidates exist")
	}
}

// TestGoCoverageModeSkipDoesNotRunTests guards ModeSkip's core promise: it
// must parse an existing profile without ever invoking `go test`. A fixture
// Go module with a deliberately failing test proves this — if goCoverage
// under ModeSkip ran the tests, the run would fail/hang and this test would
// fail with it; instead it should cleanly read the pre-existing profile.
func TestGoCoverageModeSkipDoesNotRunTests(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module fixture\n\ngo 1.26\n")
	write(t, dir, "main.go", "package fixture\n\nfunc Foo() {}\n")
	write(t, dir, "main_test.go", "package fixture\n\nimport \"testing\"\n\nfunc TestFails(t *testing.T) { t.Fatal(\"this test must never run under ModeSkip\") }\n")
	write(t, dir, "coverage.out", "mode: set\nfixture/main.go:3.13,3.16 1 1\n")

	pct, _, ok := goCoverage(context.Background(), dir, "", ModeSkip, "")
	if !ok {
		t.Fatal("expected goCoverage to succeed by parsing the existing coverage.out")
	}
	if pct != 100 {
		t.Fatalf("got %v%%, want 100%% from the fixture profile", pct)
	}
}

func write(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
