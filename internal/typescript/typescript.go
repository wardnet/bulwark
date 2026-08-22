// Package typescript runs Biome against every detected TypeScript package
// using a toolchain bulwark bundles and pins itself, independent of the target
// package's own devDependencies. This avoids the failure mode where a
// package's lint script references a linter it never declares as a dependency.
//
// Biome is the only engine. It parses TypeScript with its own Rust parser and
// depends on no compiler package, which is why bulwark's TypeScript linting
// carries no `typescript` pin and cannot be broken by one: the ESLint stack it
// replaced needed @typescript-eslint/parser to see .ts at all, the parser
// needed the `typescript` package as a peer, and that peer range is a moving
// ceiling a repo's own TypeScript version eventually crosses. See
// docs/adr/0008-biome-as-the-only-typescript-linter.md, and 0005 for the
// opt-in that preceded it.
//
// The pinned version lives in biome-pin/package.json, not in a Go constant, so
// Dependabot can see it and open bump PRs — a pinned security toolchain that
// nothing ever ages out is a scanner that quietly goes stale while still
// reporting [PASS].
package typescript

import (
	"context"
	"path/filepath"

	"wardnet/bulwark/internal/detect"
	"wardnet/bulwark/internal/executil"
)

// Check lints every package directory under root, skipping any directory
// named in exclude.
func Check(ctx context.Context, root string, exclude []string) ([]executil.Result, error) {
	pkgDirs, err := detect.TSPackageDirs(root, exclude)
	if err != nil {
		return nil, err
	}
	// Nothing to lint: don't pay for a toolchain install to discover that.
	if len(pkgDirs) == 0 {
		return nil, nil
	}

	// One toolchain install for the whole run, then one lint per package.
	toolchainDir, err := ensureBiome(ctx)
	if err != nil {
		return nil, err
	}
	biomeBin := filepath.Join(toolchainDir, "node_modules", ".bin", "biome")
	configPath := filepath.Join(toolchainDir, "biome.json")

	var results []executil.Result
	for _, dir := range pkgDirs {
		res, err := lintDirBiome(ctx, dir, biomeBin, configPath)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}
