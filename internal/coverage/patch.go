package coverage

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/cover"

	"wardnet/bulwark/internal/executil"
)

// LineHits maps a repo-relative file path (forward-slash, matching git's own
// path convention) to a map of line number -> hit count, as reported by one
// ecosystem's coverage tooling. Bulwark treats this as the single common
// intermediate shape all three ecosystems converge on, however each one's
// native report format got there (a Go coverage profile, an lcov file).
type LineHits map[string]map[int]int

// hunkHeader matches a unified diff hunk header's new-file half, e.g.
// "@@ -12,3 +15,4 @@" -> start line 15, length 4 (length defaults to 1 when
// omitted, e.g. "@@ -12 +15 @@").
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ChangedLines returns, per repo-relative file path, the line numbers added
// or modified by dir's working tree relative to mergeBase — only files whose
// name ends in one of exts are considered. Deleted lines and unchanged
// context lines never count; `--unified=0` already drops context lines from
// the diff itself, so only "@@" headers and "+" lines need parsing. This
// deliberately does no language-aware filtering (comments, blank lines,
// imports) — that happens for free later, when these line numbers are
// intersected with a coverage report, since non-executable lines never
// appear in one.
func ChangedLines(ctx context.Context, dir, mergeBase string, exts ...string) (map[string][]int, error) {
	// -c diff.mnemonicPrefix=false pins the "+++ b/<path>" header form
	// parseUnifiedDiff expects, regardless of the caller's own git config —
	// mnemonicPrefix=true would emit "+++ w/<path>" instead, which
	// parseUnifiedDiff's "b/" strip wouldn't recognize.
	args := []string{"-c", "diff.mnemonicPrefix=false", "diff", "--unified=0", mergeBase + "..HEAD"}
	if len(exts) > 0 {
		args = append(args, "--")
		for _, ext := range exts {
			args = append(args, "*"+ext)
		}
	}
	r := executil.Run(ctx, dir, "git", args...)
	if !r.Ok() {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), r.Err)
	}
	return parseUnifiedDiff(r.Output), nil
}

func parseUnifiedDiff(diff string) map[string][]int {
	changed := map[string][]int{}
	var file string
	var nextLine, remaining int
	scanner := bufio.NewScanner(strings.NewReader(diff))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			path := strings.TrimPrefix(line, "+++ ")
			path = strings.TrimPrefix(path, "b/")
			if path == "/dev/null" {
				file = ""
				continue
			}
			file = filepath.ToSlash(path)
		case strings.HasPrefix(line, "@@ "):
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			length := 1
			if m[2] != "" {
				length, _ = strconv.Atoi(m[2])
			}
			nextLine, remaining = start, length
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			if file != "" && remaining > 0 {
				changed[file] = append(changed[file], nextLine)
				nextLine++
				remaining--
			}
		}
	}
	return changed
}

// ParseLCOV extracts per-file, per-line hit counts from an lcov trace file
// (the format cargo-llvm-cov's --lcov and Istanbul/Vitest's lcov reporter
// both emit natively): "SF:<path>", "DA:<line>,<hits>" pairs per file,
// terminated by "end_of_record". File paths are normalized relative to
// baseDir when absolute, so they line up with git's repo-relative paths.
func ParseLCOV(data []byte, baseDir string) LineHits {
	hits := LineHits{}
	var file string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "SF:"):
			file = normalizeRelPath(baseDir, strings.TrimPrefix(line, "SF:"))
			if _, ok := hits[file]; !ok {
				hits[file] = map[int]int{}
			}
		case strings.HasPrefix(line, "DA:"):
			if file == "" {
				continue
			}
			parts := strings.SplitN(strings.TrimPrefix(line, "DA:"), ",", 2)
			if len(parts) != 2 {
				continue
			}
			lineNo, err1 := strconv.Atoi(parts[0])
			count, err2 := strconv.Atoi(strings.SplitN(parts[1], ",", 2)[0])
			if err1 != nil || err2 != nil {
				continue
			}
			hits[file][lineNo] = count
		case line == "end_of_record":
			file = ""
		}
	}
	return hits
}

// ParseGoProfile extracts per-file, per-line hit counts from a Go coverage
// profile (the same file `go tool cover -func` already reads for the
// aggregate percentage). A block's hit count applies to every line in its
// [StartLine, EndLine] range — the same block-level granularity `go tool
// cover -html` itself uses, since the profile format doesn't record
// per-statement line data any finer than that.
func ParseGoProfile(path, moduleName string) (LineHits, error) {
	profiles, err := cover.ParseProfiles(path)
	if err != nil {
		return nil, err
	}
	hits := LineHits{}
	for _, p := range profiles {
		rel := strings.TrimPrefix(p.FileName, moduleName+"/")
		rel = filepath.ToSlash(rel)
		fileHits := map[int]int{}
		for _, b := range p.Blocks {
			for line := b.StartLine; line <= b.EndLine; line++ {
				if count, seen := fileHits[line]; !seen || b.Count > count {
					fileHits[line] = b.Count
				}
			}
		}
		hits[rel] = fileHits
	}
	return hits, nil
}

// normalizeRelPath converts an absolute path under baseDir (as many coverage
// tools emit) into a repo-relative, forward-slash path matching git's own
// convention. A path that's already relative (or outside baseDir) is passed
// through unchanged, only slash-normalized. baseDir is resolved to an
// absolute path first — filepath.Rel errors when one argument is absolute
// and the other isn't, and baseDir is commonly relative (bulwark's own
// --dir defaults to ".").
func normalizeRelPath(baseDir, path string) string {
	if filepath.IsAbs(path) {
		absBase, err := filepath.Abs(baseDir)
		if err == nil {
			if rel, err := filepath.Rel(absBase, path); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.ToSlash(path)
}

// PatchPercent cross-references changed with hits: hit/total counts only
// coverable lines (lines hits actually has an entry for) among those
// changed — a changed comment/blank/import line simply has no entry in hits,
// so it's excluded automatically. total == 0 means no coverable line was
// touched by this diff (e.g. the change was comments/whitespace/imports only
// or the file has no matching hits at all) — there is nothing to gate on.
func PatchPercent(changed map[string][]int, hits LineHits) (hit, total int) {
	for file, lines := range changed {
		fileHits, ok := hits[file]
		if !ok {
			continue
		}
		for _, line := range lines {
			count, ok := fileHits[line]
			if !ok {
				continue
			}
			total++
			if count > 0 {
				hit++
			}
		}
	}
	return hit, total
}
