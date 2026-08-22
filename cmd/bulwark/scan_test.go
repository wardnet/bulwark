package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"wardnet/bulwark/internal/executil"
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

// A failing check whose findings never streamed must print them. Biome sends
// its report to a file so the JSON cannot be corrupted by its own chatter,
// which means nothing reaches the terminal on its own — printing a bare
// "[FAIL] biome(.)" left the developer to re-run the pinned toolchain by hand
// to find out what was wrong, and put nothing in the PR comment either.
func TestReportPrintsDetailForFailingChecks(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := report(cmd, []executil.Result{
		{
			Name:   "biome(.)",
			Detail: "src/bad.ts:1  lint/security/noGlobalEval  eval() is dangerous\nsrc/bad.ts:4  lint/correctness/noUnusedVariables  unused",
			Err:    errors.New("2 finding(s)"),
		},
	})
	if err == nil {
		t.Fatal("a failing check must still return an error")
	}
	for _, want := range []string{"[FAIL] biome(.)", "noGlobalEval", "eval() is dangerous", "noUnusedVariables"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report output missing %q:\n%s", want, out.String())
		}
	}
}

// action.yml's tool_result() matches "^\[(PASS|FAIL)\] <name>$" anchored at
// both ends to decide a tool's verdict. A finding whose message happens to
// contain that shape must not be able to forge one, which is what indenting
// every detail line buys.
func TestReportDetailCannotForgeAStatusLine(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	_ = report(cmd, []executil.Result{
		{Name: "biome(.)", Detail: "[PASS] biome(.)", Err: errors.New("1 finding(s)")},
	})
	statusLines := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "[PASS] ") || strings.HasPrefix(line, "[FAIL] ") {
			statusLines++
		}
	}
	if statusLines != 1 {
		t.Errorf("got %d unindented status lines, want exactly 1 — a finding forged one:\n%s", statusLines, out.String())
	}
}

// Passing checks print no detail, and a tool that streamed its own output is
// not reprinted: doing either would duplicate the log or bury the summary.
func TestReportPrintsNoDetailForPassingOrStreamingChecks(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	_ = report(cmd, []executil.Result{
		{Name: "biome(.)", Detail: "should not appear"},
		{Name: "semgrep", Output: "already streamed to the terminal", Err: errors.New("findings")},
	})
	for _, unwanted := range []string{"should not appear", "already streamed"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("report printed %q:\n%s", unwanted, out.String())
		}
	}
}
