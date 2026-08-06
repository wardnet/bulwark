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

// HeadSHA resolves the commit currently checked out. `bulwark coverage`
// compares it against BaseSHA to tell a PR run (HEAD is ahead of the
// merge-base — compare against a baseline) from a main run (HEAD *is* the
// merge-base — there is nothing to compare against, but the coverage measured
// right now IS that commit's baseline, and recording it is the whole point).
func HeadSHA(ctx context.Context, dir string) (string, error) {
	r := executil.Run(ctx, dir, "git", "rev-parse", "HEAD")
	if !r.Ok() {
		return "", fmt.Errorf("git rev-parse HEAD: %w", r.Err)
	}
	return strings.TrimSpace(r.Output), nil
}

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
	// An empty baseline ("{}") is a cache miss, not a baseline of nothing.
	// Coverage.Compute silently omits any language it couldn't measure, so a
	// baseline computed on a runner missing that language's tooling comes back
	// empty — and once written, it's indistinguishable from a valid entry:
	// every later PR gets a cache *hit* on it, reports every language as [NEW],
	// and the gate enforces nothing, permanently and silently. wardnet's
	// bulwark-state branch accumulated nine of these. WriteBaseline now refuses
	// to cache an empty report in the first place; treating one as a miss here
	// heals the entries that were already written, without a manual purge.
	if len(report) == 0 {
		return nil, false, nil
	}
	return report, true, nil
}

// PriorBaselines returns, for each language in langs, that language's entry
// from the nearest prior cached baseline that has one. "Nearest" starts at
// sha ITSELF — a re-run or a concurrent per-language job may already have
// recorded a fresher entry for this very commit, which must beat any
// ancestor's — and then walks first-parent ancestors, inspecting at most
// maxDepth commits. It feeds the baseline writers' carry-forward of
// detected-but-unmeasured languages, so it is entirely best-effort: a missing
// state branch, shallow history, or an unparsable entry yields fewer (or no)
// entries, never an error — the caller records what it measured either way,
// and warns about what it couldn't fill.
func PriorBaselines(ctx context.Context, dir, sha string, langs []string, maxDepth int) map[string]float64 {
	found := make(map[string]float64, len(langs))
	if len(langs) == 0 {
		return found
	}
	if r := executil.Run(ctx, dir, "git", "fetch", "origin", BranchName); !r.Ok() {
		return found
	}
	// One ls-tree up front so only commits that actually have a cached
	// baseline cost a `git show`. --full-tree is load-bearing: dir is often a
	// subdirectory of the repo (consumers pass --dir source), and without it
	// ls-tree scopes to the cwd's path inside the ref's tree — bulwark-state
	// has no such subtree, so the listing comes back empty and carry-forward
	// silently finds nothing (`show ref:path` below is root-relative and
	// unaffected).
	ls := executil.Run(ctx, dir, "git", "ls-tree", "--full-tree", "--name-only", "origin/"+BranchName)
	if !ls.Ok() {
		return found
	}
	cached := make(map[string]bool)
	for _, name := range strings.Fields(ls.Output) {
		cached[name] = true
	}
	// rev-list from sha (not sha~1) keeps this working on a shallow checkout
	// too: even at fetch-depth 1 it can still see the same-sha entry.
	rev := executil.Run(ctx, dir, "git", "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", maxDepth), sha)
	if !rev.Ok() {
		return found
	}
	for _, commit := range strings.Fields(rev.Output) {
		if !cached[commit+".json"] {
			continue
		}
		r := executil.Run(ctx, dir, "git", "show", "origin/"+BranchName+":"+commit+".json")
		if !r.Ok() {
			continue
		}
		var report map[string]float64
		// Same poison rule as ReadBaseline: an empty (or unparsable) entry
		// carries no information worth forwarding.
		if err := json.Unmarshal([]byte(r.Output), &report); err != nil || len(report) == 0 {
			continue
		}
		for _, lang := range langs {
			if _, have := found[lang]; have {
				continue
			}
			if val, ok := report[lang]; ok {
				found[lang] = val
			}
		}
		if len(found) == len(langs) {
			break
		}
	}
	return found
}

// WriteBaseline caches report for sha on the bulwark-state branch, via a
// throwaway worktree so the caller's own working tree/branch is untouched.
//
// The branch is shared and busy — every CI run on the repo may push to it —
// so a non-fast-forward rejection is a routine event, not an edge case: the
// local origin/bulwark-state tracking ref is as stale as the job's checkout
// (wardnet's scan runs for minutes between the two), and any concurrent run
// that lands a baseline in that window advances the remote. Each attempt
// therefore fetches the fresh remote ref immediately before staging, and a
// rejected push is retried from that fresh ref rather than swallowed. A push
// that never lands is returned as an error — the caller decides whether
// that's fatal, but it must never be reported as recorded (wardnet's main
// run printed "recorded coverage baseline" while the push had been rejected,
// and its PRs gated against nothing).
func WriteBaseline(ctx context.Context, dir, sha string, report map[string]float64) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	const attempts = 3
	var lastErr error
	for range attempts {
		if lastErr = pushBaseline(ctx, dir, sha, data); lastErr == nil {
			return nil
		}
	}
	return fmt.Errorf("pushing baseline for %s to %s (%d attempts): %w", sha, BranchName, attempts, lastErr)
}

// pushBaseline is one fetch → stage → commit → push attempt.
func pushBaseline(ctx context.Context, dir, sha string, data []byte) error {
	tmp, err := os.MkdirTemp("", "bulwark-state-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	defer func() { _ = executil.Run(ctx, dir, "git", "worktree", "remove", "--force", tmp) }()

	// A local branch unique to this invocation (derived from the unique temp
	// dir), never the shared BranchName itself: git refuses to have the same
	// branch checked out in two worktrees at once, and this repo may well
	// have several worktrees already (bulwark's own gt bare-repo layout, or
	// concurrent CI jobs sharing a checkout) — two concurrent WriteBaseline
	// calls must not race on a shared local branch name. The remote branch
	// is still named BranchName; only the local staging name differs, pushed
	// via refspec below.
	staging := "bulwark-state-staging-" + filepath.Base(tmp)

	branchExists := executil.Run(ctx, dir, "git", "ls-remote", "--exit-code", "--heads", "origin", BranchName).Ok()
	if branchExists {
		// Refresh origin/<BranchName> right before staging on it: the tracking
		// ref left behind by the job's checkout (or a prior ReadBaseline) can
		// be minutes stale, and a staging branch built on a stale ref pushes
		// non-fast-forward and is rejected.
		if r := executil.Run(ctx, dir, "git", "fetch", "origin", BranchName); !r.Ok() {
			return fmt.Errorf("fetch %s: %w", BranchName, r.Err)
		}
		if r := executil.Run(ctx, dir, "git", "worktree", "add", "-b", staging, tmp, "origin/"+BranchName); !r.Ok() {
			return fmt.Errorf("worktree add %s: %w", BranchName, r.Err)
		}
	} else {
		if r := executil.Run(ctx, dir, "git", "worktree", "add", "--detach", tmp); !r.Ok() {
			return fmt.Errorf("worktree add (detached): %w", r.Err)
		}
		if r := executil.Run(ctx, tmp, "git", "checkout", "--orphan", staging); !r.Ok() {
			return fmt.Errorf("checkout --orphan %s: %w", staging, r.Err)
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
	// Nothing staged means the fetched branch already carries this exact
	// content — a concurrent run recorded the same SHA's baseline first. The
	// desired state is on the remote; committing here would fail ("nothing to
	// commit"), so this is success, not an error. Different content (notably a
	// poisoned `{}` entry being healed by a real report) stages a change and
	// proceeds to overwrite as usual.
	if executil.Run(ctx, tmp, "git", "diff", "--cached", "--quiet").Ok() {
		return nil
	}
	commitR := executil.RunEnv(ctx, tmp, []string{
		"GIT_AUTHOR_NAME=bulwark", "GIT_AUTHOR_EMAIL=bulwark@users.noreply.github.com",
		"GIT_COMMITTER_NAME=bulwark", "GIT_COMMITTER_EMAIL=bulwark@users.noreply.github.com",
	}, "git", "commit", "-m", "coverage baseline for "+sha)
	if !commitR.Ok() {
		return fmt.Errorf("git commit: %w", commitR.Err)
	}
	if r := executil.Run(ctx, tmp, "git", "push", "origin", staging+":refs/heads/"+BranchName); !r.Ok() {
		return fmt.Errorf("git push: %w", r.Err)
	}
	return nil
}
