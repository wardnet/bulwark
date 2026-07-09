// Package gitstate stores and retrieves coverage baselines on a dedicated
// `bulwark-state` branch, keyed by the main commit SHA they were computed
// against. This is deliberately a branch, not a commit on main: it's
// bot-owned generated cache data, not source, so it needs no PR/review
// ceremony and never pollutes main's history. Lookups are lazy — there is no
// "on merge to main" trigger; the first PR against a new main SHA computes
// and caches the baseline, every subsequent PR against that SHA reuses it.
package gitstate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wardnet/bulwark/internal/executil"
)

// BranchName is the dedicated branch coverage baselines live on.
const BranchName = "bulwark-state"

// BaseSHA resolves the commit on origin/main this branch diverged from, so
// bulwark coverage knows which baseline to compare against.
func BaseSHA(ctx context.Context, dir string) (string, error) {
	if r := executil.Run(ctx, dir, "git", "fetch", "origin", "main"); !r.Ok() {
		return "", fmt.Errorf("fetch origin main: %w", r.Err)
	}
	r := executil.Run(ctx, dir, "git", "merge-base", "HEAD", "origin/main")
	if !r.Ok() {
		return "", fmt.Errorf("git merge-base HEAD origin/main: %w", r.Err)
	}
	return strings.TrimSpace(r.Output), nil
}

// ReadBaseline returns the cached report for sha, and false if none exists
// yet (a cache miss, not an error — the caller computes and writes one).
func ReadBaseline(ctx context.Context, dir, sha string) (map[string]float64, bool, error) {
	// A missing remote branch is the expected first-ever-run state, not an
	// error: there's nothing to fetch yet.
	if r := executil.Run(ctx, dir, "git", "fetch", "origin", BranchName); !r.Ok() {
		return nil, false, nil
	}
	r := executil.Run(ctx, dir, "git", "show", "origin/"+BranchName+":"+sha+".json")
	if !r.Ok() {
		return nil, false, nil
	}
	var report map[string]float64
	if err := json.Unmarshal([]byte(r.Output), &report); err != nil {
		return nil, false, fmt.Errorf("parsing cached baseline for %s: %w", sha, err)
	}
	return report, true, nil
}

// WriteBaseline caches report for sha on the bulwark-state branch, via a
// throwaway worktree so the caller's own working tree/branch is untouched.
// A push race (another PR wrote the same SHA's baseline concurrently) is
// non-fatal — caching is an optimization, not a correctness requirement,
// since the caller already has its computed report in memory regardless.
func WriteBaseline(ctx context.Context, dir, sha string, report map[string]float64) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "bulwark-state-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	defer func() { _ = executil.Run(ctx, dir, "git", "worktree", "remove", "--force", tmp) }()

	branchExists := executil.Run(ctx, dir, "git", "ls-remote", "--exit-code", "--heads", "origin", BranchName).Ok()
	if branchExists {
		if r := executil.Run(ctx, dir, "git", "worktree", "add", "-B", BranchName, tmp, "origin/"+BranchName); !r.Ok() {
			return fmt.Errorf("worktree add %s: %w", BranchName, r.Err)
		}
	} else {
		if r := executil.Run(ctx, dir, "git", "worktree", "add", "--detach", tmp); !r.Ok() {
			return fmt.Errorf("worktree add (detached): %w", r.Err)
		}
		if r := executil.Run(ctx, tmp, "git", "checkout", "--orphan", BranchName); !r.Ok() {
			return fmt.Errorf("checkout --orphan %s: %w", BranchName, r.Err)
		}
		if r := executil.Run(ctx, tmp, "git", "rm", "-rf", "--ignore-unmatch", "."); !r.Ok() {
			return fmt.Errorf("clear orphan worktree: %w", r.Err)
		}
	}

	path := filepath.Join(tmp, sha+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	if r := executil.Run(ctx, tmp, "git", "add", sha+".json"); !r.Ok() {
		return fmt.Errorf("git add: %w", r.Err)
	}
	commitR := executil.RunEnv(ctx, tmp, []string{
		"GIT_AUTHOR_NAME=bulwark", "GIT_AUTHOR_EMAIL=bulwark@users.noreply.github.com",
		"GIT_COMMITTER_NAME=bulwark", "GIT_COMMITTER_EMAIL=bulwark@users.noreply.github.com",
	}, "git", "commit", "-m", "coverage baseline for "+sha)
	if !commitR.Ok() {
		return fmt.Errorf("git commit: %w", commitR.Err)
	}
	if r := executil.Run(ctx, tmp, "git", "push", "origin", BranchName); !r.Ok() {
		// Non-fatal: see doc comment above — caching is best-effort.
		return nil
	}
	return nil
}
