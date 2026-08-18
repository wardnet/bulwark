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
//
// The versions themselves live in eslint-pin/package.json, not in Go
// constants, so Dependabot can see them and open bump PRs — a pinned security
// toolchain that nothing ever ages out is a scanner that quietly goes stale
// while still reporting [PASS]. package.json has no comments, so the
// constraints that decided those pins are recorded here instead:
//
//   - @typescript-eslint/parser is what lets the security rules see .ts/.tsx
//     at all, and `typescript` is the parser's own peer dependency — it cannot
//     read TS without it.
//   - Parser 8.63.0 declares `eslint: ^8.57 || ^9 || ^10` (so it matches the
//     eslint pin) and `typescript: >=4.8.4 <6.1.0` — which is why the
//     typescript pin is 5.x and not the 7.x now on npm latest. A Dependabot PR
//     bumping typescript across that ceiling must bump the parser too.
package typescript

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wardnet/bulwark/internal/detect"
	"wardnet/bulwark/internal/executil"
)

//go:embed eslint.config.mjs
var eslintConfig []byte

// The pin manifest, embedded so the binary is self-contained. npm ci installs
// exactly what the lockfile resolves — every transitive dependency included —
// rather than re-resolving them freshly on each cache miss.
var (
	//go:embed eslint-pin/package.json
	eslintPackageJSON []byte
	//go:embed eslint-pin/package-lock.json
	eslintPackageLock []byte
)

// Linter names which engine backs the TypeScript check for a repo. The two are
// mutually exclusive and their rule sets barely overlap, so this is a migration
// state a repo moves through, not a pair of independent switches — see
// .bulwark.yml's typescript.linter and docs/adr/0005.
type Linter string

const (
	// ESLint is the default: bulwark's pinned ESLint + eslint-plugin-security.
	ESLint Linter = "eslint"
	// Biome is opt-in, for repos that have migrated off ESLint.
	Biome Linter = "biome"
)

// Check lints every package directory under root with the configured linter,
// skipping any directory named in exclude.
func Check(ctx context.Context, root string, exclude []string, linter Linter) ([]executil.Result, error) {
	pkgDirs, err := detect.TSPackageDirs(root, exclude)
	if err != nil {
		return nil, err
	}
	// Nothing to lint: don't pay for a toolchain install to discover that.
	if len(pkgDirs) == 0 {
		return nil, nil
	}

	if linter == Biome {
		return checkBiome(ctx, pkgDirs)
	}

	toolchainDir, err := ensureToolchain(ctx)
	if err != nil {
		return nil, err
	}
	eslintBin := filepath.Join(toolchainDir, "node_modules", ".bin", "eslint")
	configPath := filepath.Join(toolchainDir, "eslint.config.mjs")

	var results []executil.Result
	for _, dir := range pkgDirs {
		res, err := lintDir(ctx, dir, eslintBin, configPath)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}

// checkBiome is Check's Biome arm, split out only to keep the two toolchains'
// setup from interleaving.
func checkBiome(ctx context.Context, pkgDirs []string) ([]executil.Result, error) {
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

// eslintNothingToLint is the message ESLint prints, alongside a non-zero exit,
// when every file under the target is ignored.
const eslintNothingToLint = "all of the files matching the glob pattern"

// eslintFile / eslintMessage mirror the subset of `eslint --format json` bulwark reads.
type eslintFile struct {
	FilePath string          `json:"filePath"`
	Messages []eslintMessage `json:"messages"`
}

type eslintMessage struct {
	RuleID   string `json:"ruleId"`
	Severity int    `json:"severity"`
	Message  string `json:"message"`
	Line     int    `json:"line"`
	Fatal    bool   `json:"fatal"`
}

// reportable decides whether a message is something bulwark should fail on.
//
// bulwark lints with its OWN standalone config, deliberately independent of
// whatever the scanned project declares. That has a consequence ESLint's exit
// code alone doesn't distinguish: a project's sources routinely carry
// `eslint-disable-next-line <its-own-plugin>/<rule>` comments, and under a
// config that never loaded that plugin ESLint raises "Definition for rule ...
// was not found" — plus "Unused eslint-disable directive" for any suppression
// whose rule we don't run. Those are complaints about the config we imposed,
// not defects in the code, and failing on them would fail every project that
// suppresses one of its own lint rules anywhere.
//
// So: report the findings from the plugin we actually brought (security/*),
// and genuine parse errors (fatal — the file couldn't be read at all, which is
// worth knowing). Ignore the rest.
//
// Note this must not be solved with --no-inline-config: that would also void
// legitimate `eslint-disable-next-line security/...` suppressions, which are
// exactly how a reviewed false positive is meant to be recorded.
func reportable(m eslintMessage) bool {
	return m.Fatal || strings.HasPrefix(m.RuleID, "security/")
}

// lintDir runs ESLint over one package and reports only bulwark's own findings.
func lintDir(ctx context.Context, dir, eslintBin, configPath string) (executil.Result, error) {
	out, err := os.CreateTemp("", "bulwark-eslint-*.json")
	if err != nil {
		return executil.Result{}, err
	}
	outPath := out.Name()
	_ = out.Close()
	defer func() { _ = os.Remove(outPath) }()

	// --format json + --output-file keeps the machine-readable report out of the
	// combined stdout/stderr stream, so parsing it can't trip over ESLint's own
	// diagnostics. No --max-warnings: we decide what counts, below.
	r := executil.Run(ctx, dir, eslintBin,
		"--config", configPath, "--format", "json", "--output-file", outPath, ".")
	r.Name = "eslint(" + dir + ")"

	// A package can legitimately hold nothing ESLint will look at — a types-only
	// package, or one whose every source file sits under an ignored path. ESLint
	// calls that a usage error and exits non-zero; it is the absence of a
	// finding, not a finding.
	if !r.Ok() && strings.Contains(r.Output, eslintNothingToLint) {
		r.Err = nil
		r.Output = "no lintable files"
		return r, nil
	}

	data, readErr := os.ReadFile(outPath) // #nosec G304 -- outPath is our own CreateTemp result, not user input
	if readErr != nil {
		// No report to read: leave ESLint's own exit status and output as-is
		// rather than inventing a verdict.
		return r, nil
	}
	var files []eslintFile
	if jsonErr := json.Unmarshal(data, &files); jsonErr != nil {
		return r, nil
	}

	var b strings.Builder
	count := 0
	for _, f := range files {
		for _, m := range f.Messages {
			if !reportable(m) {
				continue
			}
			count++
			rule := m.RuleID
			if rule == "" {
				rule = "parse-error"
			}
			fmt.Fprintf(&b, "%s:%d  %s  %s\n", f.FilePath, m.Line, rule, m.Message)
		}
	}

	if count == 0 {
		r.Err = nil
		r.Output = "no findings"
		return r, nil
	}
	r.Output = b.String()
	r.Err = fmt.Errorf("%d finding(s)", count)
	return r, nil
}

// ensureToolchain installs the pinned ESLint stack from eslint-pin/ and writes
// the bundled config alongside it.
func ensureToolchain(ctx context.Context) (string, error) {
	dir, err := ensureNPMToolchain(ctx, "eslint", "eslint", eslintPackageJSON, eslintPackageLock)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "eslint.config.mjs"), eslintConfig, 0o600); err != nil {
		return "", err
	}
	return dir, nil
}
