package gitstate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"wardnet/bulwark/internal/executil"
)

// An empty baseline must read back as a cache MISS, not as a baseline of
// nothing. coverage.Compute silently omits any language whose tooling it
// couldn't run, so a runner missing (say) cargo-llvm-cov computes `{}` — and
// once that lands on bulwark-state it is indistinguishable from a real entry:
// every later PR hits it, reports every language as [NEW], and the gate
// enforces nothing, silently and forever. wardnet accumulated nine of these.
// Treating `{}` as a miss is what heals the already-written ones without a
// manual purge of the branch.
func TestReadBaselineTreatsEmptyAsCacheMiss(t *testing.T) {
	ctx := context.Background()
	origin := t.TempDir()
	clone := t.TempDir()

	run := func(dir string, args ...string) {
		t.Helper()
		if r := executil.Run(ctx, dir, "git", args...); !r.Ok() {
			t.Fatalf("git %v: %v", args, r.Err)
		}
	}

	// A bare origin carrying a bulwark-state branch with one empty and one
	// populated baseline — the exact shape wardnet's branch is in.
	run(origin, "init", "--bare", "-b", "main", ".")
	seed := t.TempDir()
	run(seed, "init", "-b", BranchName, ".")
	run(seed, "config", "user.email", "t@t")
	run(seed, "config", "user.name", "t")
	for key, content := range map[string]string{
		"empty":  "{}",
		"filled": `{"go":58.5}`,
	} {
		path := filepath.Join(seed, filepath.FromSlash(StatePath(key)))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run(seed, "add", "-A")
	run(seed, "commit", "-m", "baselines")
	run(seed, "remote", "add", "origin", origin)
	run(seed, "push", "origin", BranchName)

	run(clone, "init", "-b", "main", ".")
	run(clone, "remote", "add", "origin", origin)

	if _, hit, err := ReadBaseline(ctx, clone, "empty"); err != nil || hit {
		t.Errorf("ReadBaseline on an empty {} baseline: hit=%v err=%v, want a cache miss", hit, err)
	}

	report, hit, err := ReadBaseline(ctx, clone, "filled")
	if err != nil {
		t.Fatalf("ReadBaseline: %v", err)
	}
	if !hit || report["go"] != 58.5 {
		t.Errorf("ReadBaseline on a real baseline = (%v, hit=%v), want ({go:58.5}, hit=true)", report, hit)
	}
}

// gitRunner returns a t.Fatal-ing git helper bound to ctx, mirroring the
// inline helper the test above uses.
func gitRunner(t *testing.T, ctx context.Context) func(dir string, args ...string) {
	t.Helper()
	return func(dir string, args ...string) {
		t.Helper()
		if r := executil.Run(ctx, dir, "git", args...); !r.Ok() {
			t.Fatalf("git %v: %v\n%s", args, r.Err, r.Output)
		}
	}
}

// seedStateBranch creates a bare origin whose bulwark-state branch carries the
// given files, and returns the origin path.
func seedStateBranch(t *testing.T, ctx context.Context, files map[string]string) string {
	t.Helper()
	run := gitRunner(t, ctx)
	origin := t.TempDir()
	run(origin, "init", "--bare", "-b", "main", ".")
	seed := t.TempDir()
	run(seed, "init", "-b", BranchName, ".")
	run(seed, "config", "user.email", "t@t")
	run(seed, "config", "user.name", "t")
	for key, content := range files {
		path := filepath.Join(seed, filepath.FromSlash(StatePath(key)))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run(seed, "add", "-A")
	run(seed, "commit", "-m", "baselines")
	run(seed, "remote", "add", "origin", origin)
	run(seed, "push", "origin", BranchName)
	return origin
}

// PriorBaselines feeds the baseline writers' carry-forward: when a partial
// run (path-filtered jobs, bare baseline worktree) measures only some of the
// detected languages, the unmeasured ones keep their entry from the nearest
// prior baseline instead of silently vanishing from the gate. Each language
// must come from the nearest commit that has it — starting at sha ITSELF (a
// re-run or a concurrent per-language job may already have recorded a fresher
// entry for this very commit, which must beat any ancestor's), then
// first-parent ancestors — skipping empty `{}` entries (poison, same as
// ReadBaseline) and honoring maxDepth.
func TestPriorBaselinesNearestCommitWinsPerLanguage(t *testing.T) {
	ctx := context.Background()
	run := gitRunner(t, ctx)

	// A clone with a real main history c1 -> c2 -> c3 -> c4 (HEAD).
	clone := t.TempDir()
	run(clone, "init", "-b", "main", ".")
	run(clone, "config", "user.email", "t@t")
	run(clone, "config", "user.name", "t")
	shas := make([]string, 0, 4)
	for i := range 4 {
		if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte{byte('a' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		run(clone, "add", "-A")
		run(clone, "commit", "-m", "c")
		r := executil.Run(ctx, clone, "git", "rev-parse", "HEAD")
		if !r.Ok() {
			t.Fatalf("rev-parse: %v", r.Err)
		}
		shas = append(shas, strings.TrimSpace(r.Output))
	}
	c1, c2, c3, c4 := shas[0], shas[1], shas[2], shas[3]

	// bulwark-state has baselines for c4 itself (a concurrent job's fresh
	// entry), c3 (empty — must be skipped), c2, and c1.
	origin := seedStateBranch(t, ctx, map[string]string{
		c4: `{"go":77}`,
		c3: "{}",
		c2: `{"rust":10}`,
		c1: `{"rust":20,"typescript":93.8}`,
	})
	run(clone, "remote", "add", "origin", origin)

	// Run from a SUBDIRECTORY of the repo, not its root: consumers point
	// --dir at a subfolder (wardnet uses --dir source), and every git
	// plumbing call here must be cwd-independent. `git ls-tree <ref>` without
	// --full-tree silently scopes to the cwd's path inside the ref's tree —
	// bulwark-state has no such subtree, so the lookup found nothing and
	// carry-forward silently no-opped on wardnet's real CI (PR #899's rerun).
	subdir := filepath.Join(clone, "sub")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	got := PriorBaselines(ctx, subdir, c4, []string{"go", "rust", "typescript"}, 10)
	want := map[string]float64{"go": 77, "rust": 10, "typescript": 93.8}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PriorBaselines from a repo subdirectory = %v, want %v (go from c4 itself, rust from c2, typescript from c1)", got, want)
	}

	// maxDepth counts commits inspected starting at sha, so depth 2 only
	// reaches c4 and c3 (empty, skipped): rust/typescript stay unfilled.
	if got := PriorBaselines(ctx, clone, c4, []string{"go", "rust", "typescript"}, 2); !reflect.DeepEqual(got, map[string]float64{"go": 77}) {
		t.Errorf("PriorBaselines with maxDepth=2 = %v, want just c4's go entry", got)
	}

	// Nothing needed means nothing looked up (and certainly nothing returned).
	if got := PriorBaselines(ctx, clone, c4, nil, 10); len(got) != 0 {
		t.Errorf("PriorBaselines with no needed languages = %v, want empty", got)
	}

	// Best-effort everywhere: no bulwark-state branch at all is no priors,
	// not an error.
	bare := t.TempDir()
	run(bare, "init", "--bare", "-b", "main", ".")
	orphan := t.TempDir()
	run(orphan, "init", "-b", "main", ".")
	run(orphan, "remote", "add", "origin", bare)
	if got := PriorBaselines(ctx, orphan, c4, []string{"go"}, 10); len(got) != 0 {
		t.Errorf("PriorBaselines with no state branch = %v, want empty", got)
	}
}

// The exact race that lost wardnet's main-run baseline: the caller's local
// origin/bulwark-state tracking ref went stale (checkout fetched it at job
// start; the scan then ran for minutes while a concurrent run pushed another
// baseline), so a staging branch created from that stale ref pushes
// non-fast-forward and is rejected. WriteBaseline must fetch the fresh remote
// ref (and retry a genuinely concurrent push), not commit on top of stale
// state and silently lose the baseline.
func TestWriteBaselinePushesOverAStaleTrackingRef(t *testing.T) {
	ctx := context.Background()
	run := gitRunner(t, ctx)
	origin := seedStateBranch(t, ctx, map[string]string{"first": `{"go":10}`})

	// The caller's repo: fetches bulwark-state once, then the remote advances.
	clone := t.TempDir()
	run(clone, "init", "-b", "main", ".")
	run(clone, "remote", "add", "origin", origin)
	run(clone, "fetch", "origin", BranchName)

	// A concurrent run records a different SHA's baseline in the meantime.
	writer := t.TempDir()
	run(writer, "clone", "-b", BranchName, origin, ".")
	run(writer, "config", "user.email", "t@t")
	run(writer, "config", "user.name", "t")
	concurrent := filepath.Join(writer, filepath.FromSlash(StatePath("concurrent")))
	if err := os.MkdirAll(filepath.Dir(concurrent), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(concurrent, []byte(`{"go":20}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run(writer, "add", "-A")
	run(writer, "commit", "-m", "coverage baseline for concurrent")
	run(writer, "push", "origin", BranchName)

	if err := WriteBaseline(ctx, clone, "stalerace", map[string]float64{"go": 30}); err != nil {
		t.Fatalf("WriteBaseline over a stale tracking ref: %v", err)
	}

	// Both the concurrent write and ours must be on the remote branch.
	verify := t.TempDir()
	run(verify, "clone", "-b", BranchName, origin, ".")
	for _, key := range []string{"first", "concurrent", "stalerace"} {
		if _, err := os.Stat(filepath.Join(verify, filepath.FromSlash(StatePath(key)))); err != nil {
			t.Errorf("%s missing from %s after WriteBaseline: %v", StatePath(key), BranchName, err)
		}
	}
}

// A push that never lands must surface as an error so the caller can say
// "failed to record" instead of the misleading "recorded coverage baseline"
// wardnet's main run printed while the baseline was in fact lost.
func TestWriteBaselineReportsAPushThatNeverLands(t *testing.T) {
	ctx := context.Background()
	run := gitRunner(t, ctx)
	origin := seedStateBranch(t, ctx, map[string]string{"first": `{"go":10}`})

	// Reject every push from here on.
	hook := filepath.Join(origin, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	clone := t.TempDir()
	run(clone, "init", "-b", "main", ".")
	run(clone, "remote", "add", "origin", origin)
	run(clone, "fetch", "origin", BranchName)

	if err := WriteBaseline(ctx, clone, "rejected", map[string]float64{"go": 30}); err == nil {
		t.Error("WriteBaseline returned nil even though the push was rejected and the baseline never landed")
	}
}

// The property the whole tree-keying change rests on, tested directly rather
// than assumed: a squash merge lands a commit whose tree is the merged tree the
// pull request was built from. That is what lets a measurement taken on a pull
// request serve as the baseline for the commit it becomes.
//
// Verified against a real merge before this was written — tumika#25's gate
// recorded tree 783e0a44… and the squash commit that landed carried the same
// tree object — but a repository is cheap to build here, and this keeps the
// assumption honest if git's behaviour ever shifts.
func TestSquashMergePreservesTheMergedTree(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-m", "base")

	run(repo, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-m", "one")
	if err := os.WriteFile(filepath.Join(repo, "c.txt"), []byte("more\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-m", "two")

	// What GitHub builds for a pull request: refs/pull/N/merge, the merged
	// result of the branch into its base.
	run(repo, "checkout", "main")
	run(repo, "merge", "--no-ff", "-m", "merge ref", "feature")
	mergeTree, err := TreeSHA(ctx, repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// What a squash merge lands: one commit on main carrying the same content.
	run(repo, "reset", "--hard", "HEAD~1")
	run(repo, "merge", "--squash", "feature")
	run(repo, "commit", "-m", "squashed")
	squashTree, err := TreeSHA(ctx, repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	if mergeTree != squashTree {
		t.Fatalf("merge tree %s != squash tree %s — a pull request's measurement "+
			"would not describe the commit it becomes, and tree-keyed baselines "+
			"would silently never hit", mergeTree, squashTree)
	}
}

// Tree keying exists so a measurement taken on a pull request is still there
// when that tree becomes main. Read must therefore find an entry written under
// the tree, and must still find pre-existing entries written under a commit
// SHA — or every repository recomputes on the first run after the change.
func TestReadBaselinePrefersTheTreeAndFallsBackToTheCommit(t *testing.T) {
	ctx := context.Background()
	run := func(dir string, args ...string) {
		t.Helper()
		if r := executil.Run(ctx, dir, "git", args...); !r.Ok() {
			t.Fatalf("git %v: %v\n%s", args, r.Err, r.Output)
		}
	}

	origin := t.TempDir()
	run(origin, "init", "--bare", "-b", "main", ".")

	repo := t.TempDir()
	run(repo, "init", "-b", "main", ".")
	run(repo, "config", "user.email", "t@t")
	run(repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-m", "c")
	run(repo, "remote", "add", "origin", origin)
	run(repo, "push", "-u", "origin", "main")

	head, err := HeadSHA(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := TreeSHA(ctx, repo, head)
	if err != nil {
		t.Fatal(err)
	}

	// Only a commit-keyed entry, as written before this change.
	if err := WriteBaseline(ctx, repo, head, map[string]float64{"go": 11}); err != nil {
		t.Fatal(err)
	}
	got, hit, err := ReadBaseline(ctx, repo, tree, head)
	if err != nil || !hit {
		t.Fatalf("ReadBaseline(tree, commit) hit=%v err=%v, want the legacy commit entry found", hit, err)
	}
	if got["go"] != 11 {
		t.Errorf("go = %v, want the commit-keyed 11", got["go"])
	}

	// Now a tree-keyed entry as well: it must win, because it is the one a
	// pull request records and the one a later main commit shares.
	if err := WriteBaseline(ctx, repo, tree, map[string]float64{"go": 77}); err != nil {
		t.Fatal(err)
	}
	got, hit, err = ReadBaseline(ctx, repo, tree, head)
	if err != nil || !hit {
		t.Fatalf("ReadBaseline hit=%v err=%v, want the tree entry found", hit, err)
	}
	if got["go"] != 77 {
		t.Errorf("go = %v, want the tree-keyed 77 to take precedence over the commit-keyed 11", got["go"])
	}
}

// The premise of versioning stateDir is that an entry recorded under a
// superseded metric is never read as the current one. A baseline sitting at
// the branch root — where entries predating the version-keyed layout live —
// must therefore be a clean cache miss, not a hit whose number means
// something else.
func TestReadBaselineIgnoresEntriesOutsideTheStateDir(t *testing.T) {
	ctx := context.Background()
	run := gitRunner(t, ctx)

	origin := t.TempDir()
	run(origin, "init", "--bare", "-b", "main", ".")
	seed := t.TempDir()
	run(seed, "init", "-b", BranchName, ".")
	run(seed, "config", "user.email", "t@t")
	run(seed, "config", "user.name", "t")
	// Deliberately at the branch root, not under StatePath's directory.
	if err := os.WriteFile(filepath.Join(seed, "deadbeef.json"), []byte(`{"go":58.5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", "-A")
	run(seed, "commit", "-m", "baseline under the superseded layout")
	run(seed, "remote", "add", "origin", origin)
	run(seed, "push", "origin", BranchName)

	clone := t.TempDir()
	run(clone, "init", "-b", "main", ".")
	run(clone, "remote", "add", "origin", origin)

	if report, hit, err := ReadBaseline(ctx, clone, "deadbeef"); err != nil || hit {
		t.Errorf("ReadBaseline = (%v, hit=%v, err=%v), want a cache miss — the entry is outside %s", report, hit, err, StatePath(""))
	}
	// The carry-forward walk must agree: an entry it cannot compare against is
	// not one to carry forward from either.
	if got := PriorBaselines(ctx, clone, "deadbeef", []string{"go"}, 5); len(got) != 0 {
		t.Errorf("PriorBaselines = %v, want nothing carried from an entry outside the state dir", got)
	}
}
