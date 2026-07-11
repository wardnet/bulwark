package coverage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func writeNested(t *testing.T, dir, name, contents string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFindReportForCrateBareOverrideSoloCrate(t *testing.T) {
	dir := t.TempDir()
	crateDir := filepath.Join(dir, "daemon")
	writeNested(t, dir, filepath.Join("daemon", "coverage", "daemon-llvm-cov.json"), "{}")

	overrides := RustReportOverrides{"": "daemon/coverage/daemon-llvm-cov.json"}
	got, ok := findReportForCrate(dir, crateDir, "daemon", overrides, true, nil)
	want := filepath.Join(dir, "daemon", "coverage", "daemon-llvm-cov.json")
	if !ok || got != want {
		t.Fatalf("findReportForCrate = (%q, %v), want (%q, true)", got, ok, want)
	}
}

func TestFindReportForCrateBareOverrideIgnoredWhenNotSolo(t *testing.T) {
	dir := t.TempDir()
	crateDir := filepath.Join(dir, "daemon")
	writeNested(t, dir, filepath.Join("daemon", "coverage", "daemon-llvm-cov.json"), "{}")

	overrides := RustReportOverrides{"": "daemon/coverage/daemon-llvm-cov.json"}
	if _, ok := findReportForCrate(dir, crateDir, "daemon", overrides, false, nil); ok {
		t.Fatal("bare override must only apply when solo is true (exactly one crate discovered)")
	}
}

func TestFindReportForCrateKeyedOverrideResolvesMatchingCrateOnly(t *testing.T) {
	dir := t.TempDir()
	daemonDir := filepath.Join(dir, "daemon")
	otherDir := filepath.Join(dir, "other")
	writeNested(t, dir, filepath.Join("daemon", "report.json"), "{}")

	overrides := RustReportOverrides{"daemon": "daemon/report.json"}

	got, ok := findReportForCrate(dir, daemonDir, "daemon", overrides, false, nil)
	want := filepath.Join(dir, "daemon", "report.json")
	if !ok || got != want {
		t.Fatalf("findReportForCrate for daemon = (%q, %v), want (%q, true)", got, ok, want)
	}

	if _, ok := findReportForCrate(dir, otherDir, "other", overrides, false, nil); ok {
		t.Fatal("override keyed to daemon must not apply to a different crate")
	}
}

func TestFindReportForCrateCandidateFallbackRelativeToCrateDir(t *testing.T) {
	dir := t.TempDir()
	crateDir := filepath.Join(dir, "daemon")
	writeNested(t, dir, filepath.Join("daemon", "coverage", "llvm-cov.json"), "{}")

	got, ok := findReportForCrate(dir, crateDir, "daemon", nil, false, rustReportCandidates)
	want := filepath.Join(crateDir, "coverage", "llvm-cov.json")
	if !ok || got != want {
		t.Fatalf("findReportForCrate = (%q, %v), want (%q, true)", got, ok, want)
	}
}

func TestResolvePackageManager(t *testing.T) {
	cases := []struct {
		name      string
		lockfiles []string
		wantMgr   string
		wantOK    bool
	}{
		{"npm", []string{"package-lock.json"}, "npm", true},
		{"yarn", []string{"yarn.lock"}, "yarn", true},
		{"pnpm", []string{"pnpm-lock.yaml"}, "pnpm", true},
		{"none", nil, "", false},
		{"ambiguous both present", []string{"package-lock.json", "yarn.lock"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, lf := range tc.lockfiles {
				write(t, dir, lf, "")
			}
			mgr, ok := resolvePackageManager(dir)
			if ok != tc.wantOK || mgr != tc.wantMgr {
				t.Fatalf("resolvePackageManager = (%q, %v), want (%q, %v)", mgr, ok, tc.wantMgr, tc.wantOK)
			}
		})
	}
}

func TestTsWorkspaceRootsDedupesNestedPackages(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "yarn.lock", "")
	a := filepath.Join(dir, "packages", "a")
	b := filepath.Join(dir, "packages", "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	roots := tsWorkspaceRoots(dir, []string{a, b})
	if len(roots) != 1 || roots[0] != dir {
		t.Fatalf("got %v, want [%s] (both packages share the root lockfile)", roots, dir)
	}
}

func TestTsWorkspaceRootsIndependentNestedLockfile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "yarn.lock", "")
	nested := filepath.Join(dir, "vendor-app")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, nested, "package-lock.json", "")

	roots := tsWorkspaceRoots(dir, []string{dir, nested})
	if len(roots) != 2 || roots[0] != dir || roots[1] != nested {
		t.Fatalf("got %v, want [%s %s] (nested package has its own independent lockfile)", roots, dir, nested)
	}
}

func TestTsWorkspaceRootsOmitsPkgWithNoLockfile(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(pkg, 0o750); err != nil {
		t.Fatal(err)
	}

	roots := tsWorkspaceRoots(dir, []string{pkg})
	if len(roots) != 0 {
		t.Fatalf("got %v, want none (no lockfile anywhere in ancestry)", roots)
	}
}

// fakeBin writes an executable shell script named name into binDir that
// appends "name args..." to sentinelLog, then exits with exitCode. Callers
// prepend binDir to PATH via t.Setenv so executil.Run resolves name to this
// script instead of any real binary — the standard way to assert an install
// command was (or wasn't) invoked without depending on real
// npm/yarn/pnpm/corepack being present in the test environment.
func fakeBin(t *testing.T, binDir, name string, exitCode int, sentinelLog string) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\necho \"%s $*\" >> %s\nexit %d\n", name, sentinelLog, exitCode)
	if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil { //nolint:gosec // intentionally executable test fixture
		t.Fatal(err)
	}
}

func TestTsInstallRunsNpmCiForNpmLockfile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "package-lock.json", "")

	binDir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "sentinel.log")
	fakeBin(t, binDir, "npm", 0, sentinel)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tsInstall(context.Background(), []string{root}, "")

	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("expected npm to be invoked, sentinel log missing: %v", err)
	}
	if !strings.Contains(string(data), "npm ci") {
		t.Fatalf("sentinel log = %q, want it to contain %q", data, "npm ci")
	}
}

func TestTsInstallRunsYarnWithCorepackForYarnLockfile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "yarn.lock", "")

	binDir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "sentinel.log")
	fakeBin(t, binDir, "corepack", 0, sentinel)
	fakeBin(t, binDir, "yarn", 0, sentinel)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tsInstall(context.Background(), []string{root}, "")

	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("expected corepack and yarn to be invoked: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d invocation(s), want 2 (corepack enable, then yarn install): %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "corepack enable") {
		t.Errorf("first invocation = %q, want it to contain %q", lines[0], "corepack enable")
	}
	if !strings.Contains(lines[1], "yarn install --immutable") {
		t.Errorf("second invocation = %q, want it to contain %q", lines[1], "yarn install --immutable")
	}
}

func TestTsInstallCorepackFailureNonFatal(t *testing.T) {
	root := t.TempDir()
	write(t, root, "yarn.lock", "")

	binDir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "sentinel.log")
	fakeBin(t, binDir, "corepack", 1, sentinel)
	fakeBin(t, binDir, "yarn", 0, sentinel)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tsInstall(context.Background(), []string{root}, "")

	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("expected yarn to still run despite corepack failing: %v", err)
	}
	if !strings.Contains(string(data), "yarn install --immutable") {
		t.Fatalf("sentinel log = %q, want yarn install to have run", data)
	}
}

func TestTsInstallOverrideUsesShellVerbatim(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "installed.marker")

	tsInstall(context.Background(), []string{root}, "touch "+marker)

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected override command to run via a real shell, marker missing: %v", err)
	}
}

func TestTsInstallNoLockfileNoOverrideRunsNothing(t *testing.T) {
	root := t.TempDir()

	binDir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "sentinel.log")
	fakeBin(t, binDir, "npm", 0, sentinel)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tsInstall(context.Background(), []string{root}, "")

	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("expected no install command to run when there's no lockfile and no override")
	}
}

func TestTsInstallAmbiguousLockfilesRunsNothing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "package-lock.json", "")
	write(t, root, "yarn.lock", "")

	binDir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "sentinel.log")
	fakeBin(t, binDir, "npm", 0, sentinel)
	fakeBin(t, binDir, "yarn", 0, sentinel)
	fakeBin(t, binDir, "corepack", 0, sentinel)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tsInstall(context.Background(), []string{root}, "")

	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("expected no install command to run for an ambiguous multi-lockfile root")
	}
}

func TestTsInstallDedupesAcrossWorkspaceRoots(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package-lock.json", "")
	a := filepath.Join(dir, "packages", "a")
	b := filepath.Join(dir, "packages", "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	binDir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "sentinel.log")
	fakeBin(t, binDir, "npm", 0, sentinel)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	roots := tsWorkspaceRoots(dir, []string{a, b})
	tsInstall(context.Background(), roots, "")

	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("expected npm to be invoked: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d invocation(s), want exactly 1 (deduped across the shared workspace root): %v", len(lines), lines)
	}
}

func write(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
