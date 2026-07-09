package coverage

import (
	"encoding/json"
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
