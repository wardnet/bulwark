package coverage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseUnifiedDiffSingleHunk(t *testing.T) {
	diff := "diff --git a/main.go b/main.go\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -10,0 +11,3 @@ func Foo() {\n" +
		"+line11\n" +
		"+line12\n" +
		"+line13\n"

	got := parseUnifiedDiff(diff)
	want := map[string][]int{"main.go": {11, 12, 13}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseUnifiedDiff = %+v, want %+v", got, want)
	}
}

func TestParseUnifiedDiffMultipleHunksAndFiles(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n" +
		"--- a/a.go\n" +
		"+++ b/a.go\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n" +
		"@@ -20,0 +21,2 @@\n" +
		"+added1\n" +
		"+added2\n" +
		"diff --git a/b.go b/b.go\n" +
		"--- a/b.go\n" +
		"+++ b/b.go\n" +
		"@@ -5,0 +6 @@\n" +
		"+onlyline\n"

	got := parseUnifiedDiff(diff)
	want := map[string][]int{
		"a.go": {1, 21, 22},
		"b.go": {6},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseUnifiedDiff = %+v, want %+v", got, want)
	}
}

func TestParseUnifiedDiffNewFile(t *testing.T) {
	diff := "diff --git a/new.go b/new.go\n" +
		"new file mode 100644\n" +
		"--- /dev/null\n" +
		"+++ b/new.go\n" +
		"@@ -0,0 +1,2 @@\n" +
		"+package foo\n" +
		"+func Foo() {}\n"

	got := parseUnifiedDiff(diff)
	want := map[string][]int{"new.go": {1, 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseUnifiedDiff = %+v, want %+v", got, want)
	}
}

func TestParseUnifiedDiffDeletedFileIgnored(t *testing.T) {
	diff := "diff --git a/gone.go b/gone.go\n" +
		"deleted file mode 100644\n" +
		"--- a/gone.go\n" +
		"+++ /dev/null\n" +
		"@@ -1,2 +0,0 @@\n" +
		"-package foo\n" +
		"-func Foo() {}\n"

	got := parseUnifiedDiff(diff)
	if len(got) != 0 {
		t.Fatalf("parseUnifiedDiff for a deleted file = %+v, want empty", got)
	}
}

func TestParseLCOV(t *testing.T) {
	data := []byte(
		"SF:/repo/src/foo.rs\n" +
			"DA:1,1\n" +
			"DA:2,0\n" +
			"DA:5,3\n" +
			"end_of_record\n" +
			"SF:/repo/src/bar.rs\n" +
			"DA:10,0\n" +
			"end_of_record\n",
	)

	got := ParseLCOV(data, "/repo")
	want := LineHits{
		"src/foo.rs": {1: 1, 2: 0, 5: 3},
		"src/bar.rs": {10: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseLCOV = %+v, want %+v", got, want)
	}
}

func TestParseLCOVRelativePathsPassThrough(t *testing.T) {
	data := []byte("SF:src/foo.rs\nDA:1,4\nend_of_record\n")
	got := ParseLCOV(data, "/repo")
	want := LineHits{"src/foo.rs": {1: 4}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseLCOV = %+v, want %+v", got, want)
	}
}

func TestNormalizeRelPath(t *testing.T) {
	cases := []struct {
		name, baseDir, path, want string
	}{
		{"absolute under base", "/repo", "/repo/src/foo.rs", "src/foo.rs"},
		{"already relative", "/repo", "src/foo.rs", "src/foo.rs"},
		{"absolute outside base", "/repo", "/other/foo.rs", "/other/foo.rs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRelPath(tc.baseDir, tc.path); got != tc.want {
				t.Errorf("normalizeRelPath(%q, %q) = %q, want %q", tc.baseDir, tc.path, got, tc.want)
			}
		})
	}
}

// TestNormalizeRelPathRelativeBaseDir guards the bug bulwark actually ships
// with by default: --dir defaults to ".", and cargo-llvm-cov/Istanbul both
// commonly emit absolute SF: paths. filepath.Rel errors when one argument is
// absolute and the other isn't, so a naive `filepath.Rel(baseDir, path)`
// with baseDir="." silently falls through to returning path unchanged
// (still absolute) — which would never match git diff's repo-relative keys.
func TestNormalizeRelPathRelativeBaseDir(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(cwd, "src", "foo.rs")

	got := normalizeRelPath(".", abs)
	want := "src/foo.rs"
	if got != want {
		t.Fatalf("normalizeRelPath(%q, %q) = %q, want %q (relative baseDir must still relativize an absolute path)", ".", abs, got, want)
	}
}

func TestPatchPercent(t *testing.T) {
	changed := map[string][]int{
		"main.go": {1, 2, 3, 4},
	}
	hits := LineHits{
		"main.go": {1: 1, 2: 0, 3: 2}, // line 4 has no entry: not coverable, excluded
	}
	hit, total := PatchPercent(changed, hits)
	if hit != 2 || total != 3 {
		t.Fatalf("PatchPercent = (%d, %d), want (2, 3)", hit, total)
	}
}

func TestPatchPercentNoCoverableLines(t *testing.T) {
	changed := map[string][]int{"main.go": {1, 2}}
	hits := LineHits{"other.go": {1: 1}}
	hit, total := PatchPercent(changed, hits)
	if hit != 0 || total != 0 {
		t.Fatalf("PatchPercent = (%d, %d), want (0, 0)", hit, total)
	}
}

func TestParseGoProfile(t *testing.T) {
	dir := t.TempDir()
	profile := "mode: set\n" +
		"fixture/main.go:3.13,5.2 2 1\n" +
		"fixture/main.go:7.13,7.20 1 0\n"
	path := filepath.Join(dir, "cover.out")
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	// No main.go on disk beside the profile: an unreadable source degrades to
	// counting every line in the block, which is the pre-filter behavior.
	got, err := ParseGoProfile(path, "fixture", dir)
	if err != nil {
		t.Fatalf("ParseGoProfile: %v", err)
	}
	want := LineHits{
		"main.go": {3: 1, 4: 1, 5: 1, 7: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseGoProfile = %+v, want %+v", got, want)
	}
}

// A Go profile records blocks, not statements, so every line between a
// block's braces lands in the report — comments and blank lines included.
// Left as-is, adding a comment inside an uncovered function reads as an
// uncovered new line, and a comment-only PR scores 0% patch coverage and
// fails the gate (wardnet/inforge#216). Those lines must not reach LineHits
// at all, so PatchPercent skips them the same way it skips any line a
// coverage report never mentions.
func TestParseGoProfileExcludesCommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	src := "package main\n" + // 1
		"\n" + // 2
		"func run() {\n" + // 3
		"\t// nosemgrep: some.rule -- audited\n" + // 4  comment inside an uncovered block
		"\n" + // 5  blank inside an uncovered block
		"\tdoWork()\n" + // 6  the only executable line here
		"}\n" // 7
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := "mode: set\nfixture/main.go:3.13,7.2 1 0\n"
	path := filepath.Join(dir, "cover.out")
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ParseGoProfile(path, "fixture", dir)
	if err != nil {
		t.Fatalf("ParseGoProfile: %v", err)
	}
	want := LineHits{"main.go": {3: 0, 6: 0, 7: 0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseGoProfile = %+v, want %+v", got, want)
	}

	// The regression itself: a PR that only adds a comment (line 4) and a
	// blank (line 5) has nothing coverable in it, so patch coverage must
	// report 0/0 — no coverable lines — not 0/2 uncovered.
	hit, total := PatchPercent(map[string][]int{"main.go": {4, 5}}, got)
	if hit != 0 || total != 0 {
		t.Fatalf("PatchPercent over a comment-only diff = (%d, %d), want (0, 0)", hit, total)
	}
}

// TestChangedLinesEndToEnd exercises ChangedLines against a real throwaway
// git repo (mergeBase..HEAD), since parseUnifiedDiff's unit tests already
// cover the hunk-parsing logic on synthetic diff text.
func TestChangedLinesEndToEnd(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "main.go")
	run("commit", "-q", "-m", "base")
	base := ""
	{
		cmd := exec.Command("git", "rev-parse", "HEAD")
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		base = string(out)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc Foo() {}\n\nfunc Bar() {\n\treturn\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "main.go")
	run("commit", "-q", "-m", "add Bar")

	changed, err := ChangedLines(context.Background(), dir, trimNL(base), ".go")
	if err != nil {
		t.Fatalf("ChangedLines: %v", err)
	}
	// The diff adds a blank separator line (4) plus the three-line Bar()
	// function (5-7) — ChangedLines reports every added line verbatim, with
	// no coverable/non-coverable filtering (that happens later, in
	// PatchPercent, once these lines are intersected with a coverage report).
	want := map[string][]int{"main.go": {4, 5, 6, 7}}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("ChangedLines = %+v, want %+v", changed, want)
	}
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
