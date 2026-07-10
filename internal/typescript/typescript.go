// Package typescript runs ESLint + eslint-plugin-security against every
// detected TypeScript package using a toolchain bulwark bundles and pins
// itself, independent of the target package's own devDependencies. This
// avoids the failure mode where a package's lint script references eslint
// but never actually declares it as a dependency.
//
// The pinned eslint + eslint-plugin-security versions are installed once into
// a bulwark-managed cache directory (not via npx's ephemeral install) and the
// bundled config is written into that same directory — co-locating them is
// required so the config's `import "eslint-plugin-security"` resolves; a
// config staged in an unrelated temp directory can't see npx's ephemeral
// node_modules and fails with ERR_MODULE_NOT_FOUND.
package typescript

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"

	"wardnet/bulwark/internal/detect"
	"wardnet/bulwark/internal/executil"
)

//go:embed eslint.config.mjs
var eslintConfig []byte

// Pinned so every invocation of bulwark, anywhere, uses the exact same
// toolchain regardless of what's cached or installed on the machine.
const (
	eslintVersion         = "10.6.0"
	pluginSecurityVersion = "4.0.1"
)

// Check lints every package directory under root, skipping any directory
// named in exclude.
func Check(ctx context.Context, root string, exclude []string) ([]executil.Result, error) {
	pkgDirs, err := detect.TSPackageDirs(root, exclude)
	if err != nil {
		return nil, err
	}

	toolchainDir, err := ensureToolchain(ctx)
	if err != nil {
		return nil, err
	}
	eslintBin := filepath.Join(toolchainDir, "node_modules", ".bin", "eslint")
	configPath := filepath.Join(toolchainDir, "eslint.config.mjs")

	var results []executil.Result
	for _, dir := range pkgDirs {
		results = append(results, executil.Run(ctx, dir, eslintBin,
			"--config", configPath, "--max-warnings", "0", "."))
	}
	return results, nil
}

// ensureToolchain installs the pinned eslint + eslint-plugin-security into a
// cache directory keyed by their versions (so a version bump gets a fresh
// install instead of silently reusing a stale one), and writes the bundled
// config alongside it.
func ensureToolchain(ctx context.Context) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cacheDir, "bulwark", "eslint-toolchain-"+eslintVersion+"-"+pluginSecurityVersion)
	configPath := filepath.Join(dir, "eslint.config.mjs")

	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".bin", "eslint")); err != nil {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", err
		}
		if r := executil.Run(ctx, dir, "npm", "init", "-y", "--silent"); !r.Ok() {
			return "", r.Err
		}
		if r := executil.Run(ctx, dir, "npm", "install", "--no-audit", "--no-fund", "--silent",
			"eslint@"+eslintVersion, "eslint-plugin-security@"+pluginSecurityVersion); !r.Ok() {
			return "", r.Err
		}
	}
	if err := os.WriteFile(configPath, eslintConfig, 0o600); err != nil {
		return "", err
	}
	return dir, nil
}
