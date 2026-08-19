package typescript

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"wardnet/bulwark/internal/executil"
)

// ensureNPMToolchain installs one pinned npm-based tool into a bulwark-managed
// cache directory and returns that directory.
//
// The directory is keyed by a hash of the lockfile rather than by a string of
// concatenated version numbers. That difference is the point: the old key was
// hand-maintained, so adding a dependency to the toolchain meant remembering to
// extend the key too, and forgetting meant silently reusing a cache directory
// that predated the new dependency — exactly the failure that would have
// reinstated "every .ts file is skipped" when the TypeScript parser was added.
// A lockfile hash cannot be forgotten: any bump to any pinned package, direct or
// transitive, changes the lockfile and therefore the directory.
//
// npm ci, not npm install: it installs precisely what the lockfile resolved,
// fails loudly if the lockfile and package.json disagree (which is how a
// half-applied Dependabot bump surfaces), and never silently re-resolves
// transitive dependencies to something newer than what was reviewed.
func ensureNPMToolchain(ctx context.Context, name, binName string, packageJSON, packageLock []byte) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(packageLock)
	dir := filepath.Join(cacheDir, "bulwark", name+"-toolchain-"+hex.EncodeToString(sum[:])[:12])

	bin := filepath.Join(dir, "node_modules", ".bin", binName)
	if _, err := os.Stat(bin); err == nil {
		return dir, nil
	}

	// Stage into a sibling temp directory and rename into place, so an
	// interrupted install can't leave a partially-populated toolchain that the
	// next run's os.Stat above mistakes for a complete one. This mirrors what
	// internal/toolchain already does for language toolchains.
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(dir), "."+name+"-staging-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := os.WriteFile(filepath.Join(staging, "package.json"), packageJSON, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(staging, "package-lock.json"), packageLock, 0o600); err != nil {
		return "", err
	}
	if r := executil.Run(ctx, staging, "npm", "ci", "--no-audit", "--no-fund", "--silent"); !r.Ok() {
		return "", fmt.Errorf("installing pinned %s toolchain: %w\n%s", name, r.Err, r.Output)
	}

	if err := os.Rename(staging, dir); err != nil {
		// A concurrent bulwark process may have won the race and created the
		// directory already. Its content is identical — same lockfile, same
		// hash — so that is a success, not a conflict.
		if _, statErr := os.Stat(bin); statErr == nil {
			return dir, nil
		}
		// Otherwise the directory exists but has no usable binary, and rename
		// onto a non-empty directory fails with ENOTEMPTY forever. That state is
		// reachable in normal use: the bundled config is written into dir *after*
		// this rename, so anything that removes node_modules while leaving dir
		// behind (cache pruning, a manual delete under ~/.cache/bulwark) leaves a
		// directory that is permanently un-renameable-onto and permanently
		// missing its binary. The install could never recover — every later scan
		// would fail with a bare rename error. Move the husk aside and retry.
		if err := replaceStaleToolchain(staging, dir, name); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// replaceStaleToolchain swaps a broken toolchain directory for a freshly staged
// one. The stale directory is renamed aside (atomic, same parent) before being
// deleted, so a concurrent process holding it open keeps a coherent view and the
// slot is never briefly absent.
func replaceStaleToolchain(staging, dir, name string) error {
	aside, err := os.MkdirTemp(filepath.Dir(dir), "."+name+"-stale-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(aside) }()

	if err := os.Rename(dir, filepath.Join(aside, "old")); err != nil {
		return err
	}
	return os.Rename(staging, dir)
}
