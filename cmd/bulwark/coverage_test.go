package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"wardnet/bulwark/internal/config"
	"wardnet/bulwark/internal/coverage"
	"wardnet/bulwark/internal/detect"
	"wardnet/bulwark/internal/executil"
	"wardnet/bulwark/internal/gitstate"
)

// A push to main that measures NOTHING must still record a baseline —
// carried wholesale from the nearest prior one — not early-return "no
// coverage measured". A docs-only merge measures nothing (every coverage
// producer path-filtered away, no reports for --tests=skip to read), and
// skipping the record leaves that main commit with no baseline at all: the
// first PR against it recomputes nothing in a bare worktree, reports every
// language as [NEW], and the gate enforces nothing (wardnet/wardnet#899).
func TestCoverageOnMainRecordsFullyCarriedBaselineWhenNothingMeasured(t *testing.T) {
	ctx := context.Background()
	run := func(dir string, args ...string) {
		t.Helper()
		if r := executil.Run(ctx, dir, "git", args...); !r.Ok() {
			t.Fatalf("git %v: %v\n%s", args, r.Err, r.Output)
		}
	}
	revParse := func(dir, ref string) string {
		t.Helper()
		r := executil.Run(ctx, dir, "git", "rev-parse", ref)
		if !r.Ok() {
			t.Fatalf("rev-parse %s: %v", ref, r.Err)
		}
		return strings.TrimSpace(r.Output)
	}

	origin := t.TempDir()
	run(origin, "init", "--bare", "-b", "main", ".")

	// The consumer checkout: c1 (code) then c2 (docs-only) on main, with
	// HEAD == origin/main — the record-on-main shape.
	repo := t.TempDir()
	run(repo, "init", "-b", "main", ".")
	run(repo, "config", "user.email", "t@t")
	run(repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Coverage production is described by the repo's own config file, the way
	// a real consumer does it now — not by a flag at the call site.
	if err := os.WriteFile(filepath.Join(repo, config.FileName), []byte("coverage:\n  source: report\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-m", "code")
	c1 := revParse(repo, "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("docs"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-m", "docs only")
	c2 := revParse(repo, "HEAD")
	run(repo, "remote", "add", "origin", origin)
	run(repo, "push", "origin", "main")
	run(repo, "fetch", "origin")

	// bulwark-state already carries c1's baseline.
	seed := t.TempDir()
	run(seed, "init", "-b", gitstate.BranchName, ".")
	run(seed, "config", "user.email", "t@t")
	run(seed, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(seed, c1+".json"), []byte(`{"typescript":93.8}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", "-A")
	run(seed, "commit", "-m", "baseline")
	run(seed, "remote", "add", "origin", origin)
	run(seed, "push", "origin", gitstate.BranchName)

	cmd := newCoverageCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--dir", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("coverage on main with nothing measured: %v\nstdout: %s\nstderr: %s", err, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "recorded coverage baseline") {
		t.Errorf("expected a recorded-baseline line, got stdout: %q", out.String())
	}

	run(repo, "fetch", "origin", gitstate.BranchName)
	// Keyed by tree, not commit: the tree is what the coverage describes, and
	// the identifier the commit shares with the pull request that produced it.
	tree, err := gitstate.TreeSHA(ctx, repo, c2)
	if err != nil {
		t.Fatalf("resolve tree for %s: %v", c2, err)
	}
	r := executil.Run(ctx, repo, "git", "show", "origin/"+gitstate.BranchName+":"+tree+".json")
	if !r.Ok() {
		t.Fatalf("no baseline recorded for tree %s (commit %s): %v\nstdout: %s\nstderr: %s", tree, c2, r.Err, out.String(), errOut.String())
	}
	if !strings.Contains(r.Output, "93.8") {
		t.Errorf("baseline for %s = %s, want typescript 93.8 carried from %s", tree, r.Output, c1)
	}
}

// The trap this whole change has to step around. Baseline computation runs
// in a throwaway worktree checked out at a historical main commit — a bare
// tree with no CI-produced report in it — so it must run tests no matter
// what the repo's coverage.source says. Reading `source: report` as "never
// run tests" anywhere in this path would hand every report-sourced repo an
// empty baseline, which the caller then declines to cache, leaving the gate
// permanently comparing against nothing.
//
// The fixture module has a test and no committed profile, so the only way a
// Go percentage can come back at all is if the suite actually ran.
func TestComputeBaselineAtRunsTestsEvenWhenSourceIsReport(t *testing.T) {
	ctx := context.Background()
	run := func(dir string, args ...string) {
		t.Helper()
		if r := executil.Run(ctx, dir, "git", args...); !r.Ok() {
			t.Fatalf("git %v: %v\n%s", args, r.Err, r.Output)
		}
	}

	repo := t.TempDir()
	run(repo, "init", "-b", "main", ".")
	run(repo, "config", "user.email", "t@t")
	run(repo, "config", "user.name", "t")
	for name, body := range map[string]string{
		"go.mod":       "module fixture\n\ngo 1.26\n",
		"main.go":      "package fixture\n\nfunc Foo() int { return 1 }\n",
		"main_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tif Foo() != 1 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n",
		// The very setting that must NOT reach the baseline worktree.
		config.FileName: "coverage:\n  source: report\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-m", "fixture")
	r := executil.Run(ctx, repo, "git", "rev-parse", "HEAD")
	if !r.Ok() {
		t.Fatalf("rev-parse: %v", r.Err)
	}
	sha := strings.TrimSpace(r.Output)

	cfg, err := config.Load(repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Coverage.Source != config.SourceReport {
		t.Fatalf("fixture config did not take: %+v", cfg.Coverage)
	}

	cmd := newCoverageCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	baseline, err := computeBaselineAt(ctx, cmd, repo, sha, cfg)
	if err != nil {
		t.Fatalf("computeBaselineAt: %v\nstderr: %s", err, errOut.String())
	}
	got, ok := baseline["go"]
	if !ok {
		t.Fatalf("baseline has no go entry — the worktree honoured coverage.source: report and measured nothing: %+v\nstderr: %s", baseline, errOut.String())
	}
	if got != 100 {
		t.Fatalf("go baseline = %v%%, want 100%% from actually running the fixture's test", got)
	}
}

func TestResolveSource(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sourceFlag string
		testsMode  string
		fromFile   config.Source
		want       coverage.Source
		wantErr    bool
	}{
		// The file is the normal path: no flags, config decides.
		{name: "file report", fromFile: config.SourceReport, want: coverage.SourceReport},
		{name: "file run", fromFile: config.SourceRun, want: coverage.SourceRun},
		// An explicit flag outranks the file, in both directions — a local
		// dev must be able to force a real run in a report-sourced repo.
		{name: "flag beats file", sourceFlag: "run", fromFile: config.SourceReport, want: coverage.SourceRun},
		{name: "flag report beats file run", sourceFlag: "report", fromFile: config.SourceRun, want: coverage.SourceReport},
		// The deprecated spelling, still honoured, still outranking the file.
		{name: "tests skip maps to report", testsMode: "skip", fromFile: config.SourceRun, want: coverage.SourceReport},
		{name: "tests run maps to run", testsMode: "run", fromFile: config.SourceReport, want: coverage.SourceRun},
		{name: "source wins over tests", sourceFlag: "report", testsMode: "run", fromFile: config.SourceRun, want: coverage.SourceReport},
		// "skip" is the old vocabulary and means nothing to --source.
		{name: "source rejects skip", sourceFlag: "skip", fromFile: config.SourceRun, wantErr: true},
		{name: "source rejects junk", sourceFlag: "nope", fromFile: config.SourceRun, wantErr: true},
		{name: "tests rejects report", testsMode: "report", fromFile: config.SourceRun, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})

			got, err := resolveSource(cmd, tc.sourceFlag, tc.testsMode, tc.fromFile)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveSource(%q, %q, %q) = %q, want an error", tc.sourceFlag, tc.testsMode, tc.fromFile, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSource: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveSource(%q, %q, %q) = %q, want %q", tc.sourceFlag, tc.testsMode, tc.fromFile, got, tc.want)
			}
		})
	}
}

// Both flags default to "" precisely so that "unset" is distinguishable from
// "explicitly run". With a "run" default the flag would always be populated,
// always outrank the file, and coverage.source would never be consulted at
// all — a silent failure that looks exactly like the config key not working.
func TestCoverageFlagDefaultsLeaveConfigInCharge(t *testing.T) {
	cmd := newCoverageCmd()
	for _, name := range []string{"source", "tests"} {
		if got := cmd.Flags().Lookup(name).DefValue; got != "" {
			t.Errorf("--%s defaults to %q; it must default to empty so .bulwark.yml is consulted", name, got)
		}
	}
}

func TestResolveReports(t *testing.T) {
	cfg := config.Default()
	cfg.Coverage.Go.Report = config.Reports{"": "from-file.out"}
	cfg.Coverage.Rust.Report = config.Reports{"daemon": "file-llvm.json"}
	cfg.Coverage.Rust.LCOV = config.Reports{"daemon": "file-lcov.info"}

	// Nothing on the command line: the file supplies all three.
	got := resolveReports(cfg, nil, nil, nil)
	if !reflect.DeepEqual(got.Go, coverage.ReportOverrides{"": "from-file.out"}) {
		t.Errorf("Go = %+v, want the file's value", got.Go)
	}
	if !reflect.DeepEqual(got.Rust, coverage.ReportOverrides{"daemon": "file-llvm.json"}) {
		t.Errorf("Rust = %+v, want the file's value", got.Rust)
	}
	if !reflect.DeepEqual(got.RustLCOV, coverage.ReportOverrides{"daemon": "file-lcov.info"}) {
		t.Errorf("RustLCOV = %+v, want the file's value", got.RustLCOV)
	}

	// A flag replaces its own kind wholesale and leaves the others alone.
	// Wholesale, not merged, so a flag can drop a stale keyed entry the file
	// declares rather than only ever being able to add to it.
	got = resolveReports(cfg, []string{"wctl=flag.out", "sdk=flag2.out"}, nil, nil)
	wantGo := coverage.ReportOverrides{"wctl": "flag.out", "sdk": "flag2.out"}
	if !reflect.DeepEqual(got.Go, wantGo) {
		t.Errorf("Go = %+v, want the flag's value replacing the file's entirely", got.Go)
	}
	if !reflect.DeepEqual(got.Rust, coverage.ReportOverrides{"daemon": "file-llvm.json"}) {
		t.Errorf("--go-report disturbed Rust: %+v", got.Rust)
	}
}

func TestResolveReportsEmptyConfigYieldsNil(t *testing.T) {
	got := resolveReports(config.Default(), nil, nil, nil)
	if got.Go != nil || got.Rust != nil || got.RustLCOV != nil {
		t.Fatalf("a zero-config repo should carry no overrides at all, got %+v", got)
	}
}

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

func runDiffReport(t *testing.T, tolerance float64, current, baseline map[string]float64, detected ...detect.Ecosystem) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	err := diffReport(cmd, current, baseline, tolerance, detected)
	return buf.String(), err
}

func TestDiffReportNewLanguage(t *testing.T) {
	out, err := runDiffReport(t, 0, map[string]float64{"go": 50}, map[string]float64{})
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
	out, err := runDiffReport(t, 0, map[string]float64{}, map[string]float64{"typescript": 85})
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
	out, err := runDiffReport(t, 0, map[string]float64{"rust": 86},
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
	out, err := runDiffReport(t, 0, map[string]float64{"go": 40}, map[string]float64{"go": 50})
	if err == nil {
		t.Fatal("expected an error for a regressed language")
	}
	if !strings.Contains(out, "[FAIL]") {
		t.Fatalf("expected a [FAIL] line, got: %q", out)
	}
}

// A dip smaller than the configured tolerance is measurement noise, not a
// regression — regression test for the gate failing PRs with "rust: 86.1%
// (baseline 86.1%, regressed 0.0%)", a sub-rounding-error dip, on PRs
// containing no Rust changes at all.
func TestDiffReportPassesWithinTolerance(t *testing.T) {
	out, err := runDiffReport(t, 0.1, map[string]float64{"rust": 86.05}, map[string]float64{"rust": 86.1})
	if err != nil {
		t.Fatalf("a dip within the tolerance must not fail the gate, got error: %v", err)
	}
	if !strings.Contains(out, "[PASS]") {
		t.Fatalf("expected a [PASS] line for a within-tolerance dip, got: %q", out)
	}
}

// The tolerance only absorbs noise — a dip larger than it is still a real
// regression and must fail.
func TestDiffReportFailsBeyondTolerance(t *testing.T) {
	out, err := runDiffReport(t, 0.1, map[string]float64{"rust": 85.6}, map[string]float64{"rust": 86.1})
	if err == nil {
		t.Fatal("expected an error for a dip beyond the tolerance")
	}
	if !strings.Contains(out, "[FAIL]") {
		t.Fatalf("expected a [FAIL] line for a beyond-tolerance dip, got: %q", out)
	}
}

// An exactly-at-tolerance dip must gate identically regardless of the
// operands' float64 representation: 86.2-86.1 exceeds 0.1 in raw float math
// while 86.1-86.0 does not, so a raw comparison failed one "regressed 0.1%"
// and passed an identical-looking other. The gate compares at display
// precision (regressedBeyond) so both pass.
func TestDiffReportToleranceBoundaryIsRepresentationIndependent(t *testing.T) {
	for _, pair := range [][2]float64{{86.1, 86.2}, {86.0, 86.1}, {50.0, 50.1}} {
		out, err := runDiffReport(t, 0.1, map[string]float64{"go": pair[0]}, map[string]float64{"go": pair[1]})
		if err != nil {
			t.Fatalf("a 0.1pp dip with tolerance 0.1 must pass for %v vs %v, got error: %v (out: %q)", pair[0], pair[1], err, out)
		}
	}
}

func TestRegressedBeyond(t *testing.T) {
	cases := []struct {
		name           string
		cur, base, tol float64
		want           bool
	}{
		{"improvement never regresses", 86.2, 86.1, 0, false},
		{"equal never regresses", 86.1, 86.1, 0, false},
		{"sub-display dip with zero tolerance shows 0.0%, passes", 86.06, 86.1, 0, false},
		{"visible dip with zero tolerance fails", 86.0, 86.1, 0, true},
		{"dip equal to tolerance passes (either representation)", 86.1, 86.2, 0.1, false},
		{"dip equal to tolerance passes (other representation)", 86.0, 86.1, 0.1, false},
		{"dip beyond tolerance fails", 85.9, 86.1, 0.1, true},
	}
	for _, c := range cases {
		if got := regressedBeyond(c.cur, c.base, c.tol); got != c.want {
			t.Errorf("%s: regressedBeyond(%v, %v, %v) = %v, want %v", c.name, c.cur, c.base, c.tol, got, c.want)
		}
	}
}

// Recording a within-tolerance dip verbatim would let tolerated dips
// compound: each merge lowers the baseline by up to the tolerance and the
// next PR dips again from the lower floor. The baseline writers restore such
// dips to the prior (high-water) value; only a beyond-tolerance drop — which
// was FAIL-visible on the PR that introduced it — resets the baseline.
func TestWithToleratedDipsRestored(t *testing.T) {
	record := map[string]float64{"go": 86.01, "rust": 70.0, "typescript": 90.0, "kotlin": 55.5}
	prior := map[string]float64{"go": 86.1, "rust": 71.0, "typescript": 89.5}
	got := withToleratedDipsRestored(record, prior, 0.1)
	want := map[string]float64{
		"go":         86.1, // dipped 0.09, within tolerance → restored
		"rust":       70.0, // dipped 1.0, beyond tolerance → deliberate reset, kept
		"typescript": 90.0, // improved → kept (baseline ratchets up)
		"kotlin":     55.5, // no prior → kept
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("withToleratedDipsRestored = %v, want %v", got, want)
	}
	if record["go"] != 86.01 {
		t.Fatal("input map must not be mutated")
	}
}

func TestDiffReportPass(t *testing.T) {
	out, err := runDiffReport(t, 0, map[string]float64{"go": 55}, map[string]float64{"go": 50})
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
	out, err := runDiffReport(t, 0, current, baseline)
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

// patchReportRepo builds a repo with one committed base revision and one
// commit on top of it touching the given files, returning the repo dir and
// the base SHA to use as the merge-base for patchReport.
func patchReportRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	ctx := context.Background()
	run := func(dir string, args ...string) {
		t.Helper()
		if r := executil.Run(ctx, dir, "git", args...); !r.Ok() {
			t.Fatalf("git %v: %v\n%s", args, r.Err, r.Output)
		}
	}
	repo := t.TempDir()
	run(repo, "init", "-b", "main", ".")
	run(repo, "config", "user.email", "t@t")
	run(repo, "config", "user.name", "t")
	for name, content := range files {
		p := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-m", "base")
	r := executil.Run(ctx, repo, "git", "rev-parse", "HEAD")
	if !r.Ok() {
		t.Fatalf("rev-parse: %v", r.Err)
	}
	base := strings.TrimSpace(r.Output)
	for name, content := range files {
		p := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.WriteFile(p, []byte(content+"\nfn added() {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-m", "change")
	return repo, base
}

// A detected language whose files the diff touches, with the patch gate
// enabled but no per-line source resolved, must be reported as [UNMEASURED]
// (with a stderr hint), not skipped in silence — the silent skip let
// wardnet/wardnet#957 ship a green bulwark comment while Codecov, fed the
// same lcov export, failed the very same diff's patch coverage. Like
// diffReport's [UNMEASURED], it must not fail the gate on its own.
func TestPatchReportUnmeasuredWhenSourceMissing(t *testing.T) {
	repo, base := patchReportRepo(t, map[string]string{"src/lib.rs": "fn a() {}"})

	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := patchReport(cmd, context.Background(), repo,
		coverage.PatchWanted{Rust: true}, coverage.PatchSources{}, base,
		map[string]float64{"rust": 86.4}, 0.1, []detect.Ecosystem{detect.Rust})
	if err != nil {
		t.Fatalf("an unmeasured patch gate must not fail on its own, got: %v", err)
	}
	if !strings.Contains(out.String(), "[UNMEASURED]") || !strings.Contains(out.String(), "rust patch") {
		t.Fatalf("expected an [UNMEASURED] rust patch line, got stdout: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "rust-lcov-report") {
		t.Fatalf("expected a stderr hint naming the missing wiring, got: %q", errOut.String())
	}
}

// The unmeasured report is scoped to detected ecosystems: patch gates default
// to enabled for all three languages regardless of what the repo contains, so
// a changed file of an undetected language (a stray script in a repo that
// isn't that language's ecosystem) must not produce a line for it.
func TestPatchReportUndetectedLanguageStaysSilent(t *testing.T) {
	repo, base := patchReportRepo(t, map[string]string{"tools/gen.go": "package main"})

	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := patchReport(cmd, context.Background(), repo,
		coverage.PatchWanted{Go: true, Rust: true}, coverage.PatchSources{}, base,
		map[string]float64{"rust": 86.4}, 0.1, []detect.Ecosystem{detect.Rust})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out.String(), "go patch") || strings.Contains(errOut.String(), "go ") {
		t.Fatalf("undetected go must stay silent, got stdout: %q stderr: %q", out.String(), errOut.String())
	}
}

// A diff that touches none of a detected language's files has nothing to
// gate — no [UNMEASURED] line, no warning, even with its source unresolved.
func TestPatchReportNoChangedLinesStaysSilent(t *testing.T) {
	repo, base := patchReportRepo(t, map[string]string{"README.md": "docs"})

	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := patchReport(cmd, context.Background(), repo,
		coverage.PatchWanted{Rust: true}, coverage.PatchSources{}, base,
		map[string]float64{"rust": 86.4}, 0.1, []detect.Ecosystem{detect.Rust})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.String() != "" || errOut.String() != "" {
		t.Fatalf("expected silence for a diff with no rust changes, got stdout: %q stderr: %q", out.String(), errOut.String())
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
			got := parseReportOverrides(tc.values)
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
