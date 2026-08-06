package semgrep

import (
	"reflect"
	"testing"
)

func TestBuildArgs(t *testing.T) {
	cases := []struct {
		name          string
		rulesetConfig string
		appToken      bool
		baseSHA       string
		want          []string
	}{
		{"no token, auto ruleset", "auto", false, "", []string{"scan", "--config", "auto", "--error"}},
		{"no token, custom ruleset", "p/security-audit", false, "", []string{"scan", "--config", "p/security-audit", "--error"}},
		{"no token, base SHA scopes the scan to the diff", "auto", false, "abc123", []string{"scan", "--config", "auto", "--error", "--baseline-commit", "abc123"}},
		{"token present, auto ruleset omits --config", "auto", true, "", []string{"ci"}},
		{"token present, custom ruleset still passed", "p/security-audit", true, "", []string{"ci", "--config", "p/security-audit"}},
		{"token present, empty ruleset omits --config", "", true, "", []string{"ci"}},
		{"token present, base SHA ignored — semgrep ci is already diff-aware", "auto", true, "abc123", []string{"ci"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildArgs(tc.rulesetConfig, tc.appToken, tc.baseSHA)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("buildArgs(%q, %v, %q) = %v, want %v", tc.rulesetConfig, tc.appToken, tc.baseSHA, got, tc.want)
			}
		})
	}
}
