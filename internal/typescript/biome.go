package typescript

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wardnet/bulwark/internal/executil"
)

//go:embed biome.json
var biomeConfig []byte

// The pin manifest — see the package doc in typescript.go for why the version
// lives in package.json rather than a Go constant.
var (
	//go:embed biome-pin/package.json
	biomePackageJSON []byte
	//go:embed biome-pin/package-lock.json
	biomePackageLock []byte
)

// biomeReport mirrors the subset of `biome lint --reporter=json` bulwark reads.
//
// Two things about this shape were confirmed against Biome 2.5.8 directly
// rather than from the docs, because both are easy to guess wrong:
// location.path is a plain string relative to the directory Biome ran in (not
// an object with a `file` key), and `category` — not the message — is the only
// field that identifies which rule fired.
type biomeReport struct {
	Diagnostics []biomeDiagnostic `json:"diagnostics"`
}

type biomeDiagnostic struct {
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Category string `json:"category"`
	Location struct {
		Path  string `json:"path"`
		Start struct {
			Line   int `json:"line"`
			Column int `json:"column"`
		} `json:"start"`
	} `json:"location"`
}

// reportableBiome decides whether a diagnostic is something bulwark should fail
// on. bulwark lints with its own standalone config, so anything the scanned
// project's own configuration drags in is a complaint about the setup we
// imposed rather than a defect in the code.
//
// Only the two groups bulwark's bundled config enables count. In particular
// this drops:
//
//   - suppressions/* — Biome reports `suppressions/unused` for a `biome-ignore`
//     comment naming a rule that isn't enabled, and a project migrating to
//     Biome is full of suppressions for rules bulwark deliberately doesn't run.
//   - any lint/* group outside security and correctness. This matters more than
//     it looks: a nested biome.json in the scanned repo that declares
//     `"root": false` is *merged* into bulwark's config by Biome (verified
//     against 2.5.8), so a project can inject its own style rules into our run.
//     Filtering by category is what contains that.
//
// The same merge is a real limitation in the other direction, and there is no
// way to close it from here: a nested config could set `"security": "off"` and
// silently narrow what bulwark checks. It is called out in AGENTS.md.
//
// Everything that is *not* a rule opinion is kept: a file bulwark could not
// actually lint is worth knowing about, and dropping it is worse than a false
// positive. This is deliberately a denylist of opinion categories rather than an
// allowlist of failure categories, because the failure categories are open-ended
// and every one missed is silent. Two were found the hard way: `parse` (a .ts
// file with a syntax error) and `internalError/io` (a source file bulwark cannot
// read, which Biome reports with summary.errors == 0, so not even the exit code
// gives it away). Both produced count == 0, which clears the error and prints
// "no findings" — a package where nothing was linted reporting as a clean pass.
// An allowlist would have had to guess `configuration`, `deserialize` and
// whatever a future Biome adds; this way an unknown category fails loudly.
func reportableBiome(category string) bool {
	switch {
	case strings.HasPrefix(category, "lint/security/"),
		strings.HasPrefix(category, "lint/correctness/"):
		return true
	case strings.HasPrefix(category, "lint/"),
		strings.HasPrefix(category, "assist/"),
		strings.HasPrefix(category, "suppressions/"),
		category == "format":
		// Rule opinions bulwark never asked for: other lint groups (which a
		// nested `"root": false` config can merge in), assist and formatter
		// output, and suppressions/unused firing on the project's own
		// biome-ignore comments for rules we do not enable.
		return false
	default:
		return true
	}
}

// ensureBiome installs the pinned Biome from biome-pin/ and writes the bundled
// config alongside it.
func ensureBiome(ctx context.Context) (string, error) {
	dir, err := ensureNPMToolchain(ctx, "biome", "biome", biomePackageJSON, biomePackageLock)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "biome.json"), biomeConfig, 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

// biomeNestedRootConfig is the message Biome prints, alongside a non-zero exit
// and no report at all, when the scanned tree holds a biome.json in a
// subdirectory that doesn't declare itself non-root.
const biomeNestedRootConfig = "Found a nested root configuration"

// lintDirBiome runs Biome over one package and reports only bulwark's own
// findings.
func lintDirBiome(ctx context.Context, dir, biomeBin, configPath string) (executil.Result, error) {
	out, err := os.CreateTemp("", "bulwark-biome-*.json")
	if err != nil {
		return executil.Result{}, err
	}
	outPath := out.Name()
	_ = out.Close()
	defer func() { _ = os.Remove(outPath) }()

	// --reporter-file keeps the machine-readable report out of the combined
	// stdout/stderr stream: Biome writes its own diagnostics there, including
	// an "experimental reporter" notice, which JSON parsing must not trip over.
	r := executil.Run(ctx, dir, biomeBin, "lint",
		"--config-path", configPath, "--reporter", "json", "--reporter-file", outPath, ".")
	r.Name = "biome(" + dir + ")"

	// A nested biome.json aborting the run before anything is linted must not
	// read as a pass, and must not read as a findings failure either — it is a
	// fixable configuration conflict, so say exactly that and what fixes it.
	//
	// The abort happens only when Biome resolves configuration from the tree
	// itself. --config-path, which is always passed above, suppresses it: a
	// nested config is ignored and the lint proceeds normally (checked against
	// 2.5.8 and 2.5.10). So this branch does not fire today. It is kept
	// because it stops being unreachable the moment --config-path is dropped,
	// which is exactly what honouring a project's own Biome config would
	// require.
	if !r.Ok() && strings.Contains(r.Output, biomeNestedRootConfig) {
		r.Err = fmt.Errorf("nested biome.json conflicts with bulwark's bundled config")
		r.Detail = "Biome refused to run: a biome.json below this package is treated as a second root config.\n" +
			"Add \"root\": false to it (Biome's own requirement for nested configs), or exclude that\n" +
			"directory via typescript.exclude in .bulwark.yml."
		return r, nil
	}

	data, readErr := os.ReadFile(outPath) // #nosec G304 -- outPath is our own CreateTemp result, not user input
	if readErr != nil {
		// No report to read: leave Biome's own exit status and output as-is
		// rather than inventing a verdict.
		return r, nil
	}
	var report biomeReport
	if jsonErr := json.Unmarshal(data, &report); jsonErr != nil {
		return r, nil
	}

	var b strings.Builder
	count := 0
	for _, d := range report.Diagnostics {
		if !reportableBiome(d.Category) {
			continue
		}
		count++
		fmt.Fprintf(&b, "%s:%d  %s  %s\n", d.Location.Path, d.Location.Start.Line, d.Category, d.Message)
	}

	if count == 0 {
		r.Err = nil
		return r, nil
	}
	// Detail, not Output: Output is Biome's own stream, which already reached
	// the terminal and holds no findings, and overwriting it would discard the
	// raw log the run artifact keeps. These findings exist nowhere else, so
	// they are what report() has to print.
	r.Detail = b.String()
	r.Err = fmt.Errorf("%d finding(s)", count)
	return r, nil
}
