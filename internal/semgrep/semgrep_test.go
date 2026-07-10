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
		want          []string
	}{
		{"no token, auto ruleset", "auto", false, []string{"scan", "--config", "auto", "--error"}},
		{"no token, custom ruleset", "p/security-audit", false, []string{"scan", "--config", "p/security-audit", "--error"}},
		{"token present, auto ruleset omits --config", "auto", true, []string{"ci"}},
		{"token present, custom ruleset still passed", "p/security-audit", true, []string{"ci", "--config", "p/security-audit"}},
		{"token present, empty ruleset omits --config", "", true, []string{"ci"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildArgs(tc.rulesetConfig, tc.appToken)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("buildArgs(%q, %v) = %v, want %v", tc.rulesetConfig, tc.appToken, got, tc.want)
			}
		})
	}
}
