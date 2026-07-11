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

// Pinned so every invocation of bulwark, anywhere, uses the exact same
// toolchain regardless of what's cached or installed on the machine.
const (
	eslintVersion         = "10.6.0"
	pluginSecurityVersion = "4.0.1"
	// The parser that lets the security rules see .ts/.tsx at all. `typescript`
	// itself is the parser's own peer dependency — it can't read TS without it.
	// Parser 8.63.0 declares `eslint: ^8.57 || ^9 || ^10` (so it matches the
	// eslint pin above) and `typescript: >=4.8.4 <6.1.0` — which is why this is
	// TS 5.x and not the 7.x now on npm latest.
	tsParserVersion = "8.63.0"
	typescriptVer   = "5.9.3"
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
		res, err := lintDir(ctx, dir, eslintBin, configPath)
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

// ensureToolchain installs the pinned eslint + eslint-plugin-security into a
// cache directory keyed by their versions (so a version bump gets a fresh
// install instead of silently reusing a stale one), and writes the bundled
// config alongside it.
func ensureToolchain(ctx context.Context) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	// Every pinned version is in the directory name, so adding the parser
	// re-keys the cache — an existing toolchain dir from before this change
	// can't be reused, which it must not be: it has no parser, and would
	// silently go back to skipping every .ts file.
	dir := filepath.Join(cacheDir, "bulwark",
		"eslint-toolchain-"+eslintVersion+"-"+pluginSecurityVersion+"-"+tsParserVersion+"-"+typescriptVer)
	configPath := filepath.Join(dir, "eslint.config.mjs")

	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".bin", "eslint")); err != nil {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", err
		}
		if r := executil.Run(ctx, dir, "npm", "init", "-y", "--silent"); !r.Ok() {
			return "", r.Err
		}
		if r := executil.Run(ctx, dir, "npm", "install", "--no-audit", "--no-fund", "--silent",
			"eslint@"+eslintVersion,
			"eslint-plugin-security@"+pluginSecurityVersion,
			"@typescript-eslint/parser@"+tsParserVersion,
			"typescript@"+typescriptVer,
		); !r.Ok() {
			return "", r.Err
		}
	}
	if err := os.WriteFile(configPath, eslintConfig, 0o600); err != nil {
		return "", err
	}
	return dir, nil
}
