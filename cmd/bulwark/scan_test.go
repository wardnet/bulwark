package main

import (
	"context"
	"testing"

	"wardnet/bulwark/internal/semgrep"
)

// The "auto" path needs a real repo with an origin/main to resolve against, so
// it's left to the integration surface; what's worth pinning here is the two
// branches that decide whether git is consulted at all.
func TestResolveDiffBase(t *testing.T) {
	cases := []struct {
		name     string
		diffBase string
		appToken string
		want     string
	}{
		{"unset means scan everything", "", "", ""},
		{"literal ref passes through", "origin/release", "", "origin/release"},
		{"a token short-circuits auto — semgrep ci scopes itself", "auto", "tok", ""},
		{"a token short-circuits a literal ref too", "origin/release", "tok", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(semgrep.AppTokenEnv, tc.appToken)
			got, err := resolveDiffBase(context.Background(), t.TempDir(), tc.diffBase)
			if err != nil {
				t.Fatalf("resolveDiffBase(%q) returned %v", tc.diffBase, err)
			}
			if got != tc.want {
				t.Errorf("resolveDiffBase(%q) = %q, want %q", tc.diffBase, got, tc.want)
			}
		})
	}
}
