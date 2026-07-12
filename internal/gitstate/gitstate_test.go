package gitstate

import (
	"context"
	"os"
	"path/filepath"
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
