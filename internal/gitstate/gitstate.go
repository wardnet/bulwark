// Package gitstate stores and retrieves coverage baselines on a dedicated
// `bulwark-state` branch, keyed by the TREE they were computed against.
//
// The tree, not the commit, because coverage is a property of content and
// because the tree is the one identifier a pull request shares with the commit
// it becomes: GitHub builds a pull request as refs/pull/N/merge, and a squash
// merge lands a commit carrying that same tree. Keyed by commit those two are
// unrelated, so a number measured on a pull request — by the pipeline that
// knows how to measure it — was thrown away, and the next pull request fell
// back to recomputing it in a bare worktree. Commit-keyed entries are still
// read, so state written before this keeps resolving. This is deliberately a branch, not a commit on main: it's
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

// stateDir is the directory inside BranchName that baselines are written to
// and read from, and it is keyed to the coverage metric rather than to the
// file format. A baseline is only comparable to one produced by the same
// metric: today's figure is a language's units' summed line counts, and
// diffing it against a figure computed some other way measures the change of
// definition, not a change in coverage — which surfaces as a step change of
// several points that the gate reads as a regression.
//
// So any change to how a language's percentage is derived must bump this
// constant. Entries at the superseded directory are then simply never found,
// every consumer takes one clean cache miss, and the gate recovers by
// recording afresh.
//
// A directory rather than a marker inside the file: the entries stay a plain
// language -> percentage object, which is what makes them readable by hand on
// the branch, and the superseded ones stay in place to be inspected rather
// than overwritten.
const stateDir = "v2"

// StatePath is the path, inside BranchName, of the baseline for key (a tree
// or commit SHA).
func StatePath(key string) string {
	return stateDir + "/" + key + ".json"
}

// TreeSHA resolves the tree a commit points at.
//
// Baselines are keyed by tree rather than by commit because the tree is what
// coverage is actually a property of, and because it is the one identifier
// shared by a pull request and the commit it becomes. GitHub builds a pull
// request as refs/pull/N/merge — the merged result — and a squash merge lands a
// commit with that same tree, so the number measured on the pull request is
// already the number for the main commit it produces. Keyed by commit SHA those
// two are unrelated, and the measurement is thrown away.
func TreeSHA(ctx context.Context, dir, commit string) (string, error) {
	r := executil.Run(ctx, dir, "git", "rev-parse", commit+"^{tree}")
	if !r.Ok() {
		return "", fmt.Errorf("git rev-parse %s^{tree}: %w", commit, r.Err)
	}
	return strings.TrimSpace(r.Output), nil
}

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
func ReadBaseline(ctx context.Context, dir string, keys ...string) (map[string]float64, bool, error) {
	// A missing remote branch is the expected first-ever-run state, not an
	// error: there's nothing to fetch yet.
	if r := executil.Run(ctx, dir, "git", "fetch", "origin", BranchName); !r.Ok() {
		return nil, false, nil
	}
	// Keys are tried in order: the tree first, then the commit SHA. The SHA is
	// only a fallback for baselines written before keying moved to trees, so
	// existing state keeps resolving instead of every repository recomputing on
	// the first run after the change.
	var r executil.Result
	found := ""
	for _, key := range keys {
		if key == "" {
			continue
		}
		r = executil.Run(ctx, dir, "git", "show", "origin/"+BranchName+":"+StatePath(key))
		if r.Ok() {
			found = key
			break
		}
	}
	if !r.Ok() {
		return nil, false, nil
	}
	var report map[string]float64
	if err := json.Unmarshal([]byte(r.Output), &report); err != nil {
		return nil, false, fmt.Errorf("parsing cached baseline for %s: %w", found, err)
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
	// unaffected). -r is load-bearing for the same reason: baselines live in
	// a subdirectory of the branch, and a non-recursive listing names that
	// directory rather than the entries inside it.
	ls := executil.Run(ctx, dir, "git", "ls-tree", "-r", "--full-tree", "--name-only", "origin/"+BranchName)
	if !ls.Ok() {
		return found
	}
	cached := make(map[string]bool)
	for _, name := range strings.Fields(ls.Output) {
		cached[name] = true
	}
	// rev-list from sha (not sha~1) keeps this working on a shallow checkout
	// too: even at fetch-depth 1 it can still see the same-sha entry.
	// Commit AND tree for each entry, in one walk: baselines are keyed by tree
	// now, and by commit for anything written before that. Asking git for both
	// costs nothing here and avoids a rev-parse per commit.
	rev := executil.Run(ctx, dir, "git", "log", "--first-parent",
		fmt.Sprintf("--max-count=%d", maxDepth), "--format=%H %T", sha)
	if !rev.Ok() {
		return found
	}
	for _, line := range strings.Split(strings.TrimSpace(rev.Output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		key := ""
		// Tree first, so a commit that has both resolves to the same entry
		// ReadBaseline would pick.
		for i := len(fields) - 1; i >= 0; i-- {
			if cached[StatePath(fields[i])] {
				key = fields[i]
				break
			}
		}
		if key == "" {
			continue
		}
		r := executil.Run(ctx, dir, "git", "show", "origin/"+BranchName+":"+StatePath(key))
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

	rel := StatePath(sha)
	path := filepath.Join(tmp, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	if r := executil.Run(ctx, tmp, "git", "add", rel); !r.Ok() {
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
