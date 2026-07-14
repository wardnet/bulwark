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
	for name, content := range map[string]string{
		"empty.json":  "{}",
		"filled.json": `{"go":58.5}`,
	} {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(content), 0o600); err != nil {
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
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(content), 0o600); err != nil {
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
		c4 + ".json": `{"go":77}`,
		c3 + ".json": "{}",
		c2 + ".json": `{"rust":10}`,
		c1 + ".json": `{"rust":20,"typescript":93.8}`,
	})
	run(clone, "remote", "add", "origin", origin)

	got := PriorBaselines(ctx, clone, c4, []string{"go", "rust", "typescript"}, 10)
	want := map[string]float64{"go": 77, "rust": 10, "typescript": 93.8}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PriorBaselines = %v, want %v (go from c4 itself, rust from c2, typescript from c1)", got, want)
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
	origin := seedStateBranch(t, ctx, map[string]string{"first.json": `{"go":10}`})

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
	if err := os.WriteFile(filepath.Join(writer, "concurrent.json"), []byte(`{"go":20}`), 0o600); err != nil {
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
	for _, name := range []string{"first.json", "concurrent.json", "stalerace.json"} {
		if _, err := os.Stat(filepath.Join(verify, name)); err != nil {
			t.Errorf("%s missing from %s after WriteBaseline: %v", name, BranchName, err)
		}
	}
}

// A push that never lands must surface as an error so the caller can say
// "failed to record" instead of the misleading "recorded coverage baseline"
// wardnet's main run printed while the baseline was in fact lost.
func TestWriteBaselineReportsAPushThatNeverLands(t *testing.T) {
	ctx := context.Background()
	run := gitRunner(t, ctx)
	origin := seedStateBranch(t, ctx, map[string]string{"first.json": `{"go":10}`})

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
