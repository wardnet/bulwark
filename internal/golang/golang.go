// Package golang runs gosec and govulncheck against every Go module found
// under the scan root. Both are
// installed via `go install` into a bulwark-managed, version-keyed bin
// directory (never trusting whatever gosec/govulncheck might already be on
// PATH) — the same "pin the exact toolchain, don't reuse ambient installs"
// principle as the TypeScript toolchain, just using Go's own install
// mechanism instead of npx/npm.
package golang

import (
	"context"
	"os"
	"path/filepath"

	"wardnet/bulwark/internal/detect"
	"wardnet/bulwark/internal/executil"
)

// Pinned so every invocation of bulwark uses the exact same toolchain
// regardless of what's already on the machine.
const (
	gosecVersion       = "v2.27.1"
	govulncheckVersion = "v1.6.0"

	gosecPkg       = "github.com/securego/gosec/v2/cmd/gosec@" + gosecVersion
	govulncheckPkg = "golang.org/x/vuln/cmd/govulncheck@" + govulncheckVersion
)

// Check runs gosec and govulncheck against every Go module under root,
// skipping any directory named in exclude.
//
// Both tools are module-scoped: govulncheck exits with "no go.mod file" when
// run anywhere but a module root, so running once at the scan root only ever
// worked when the module happened to sit exactly there. A monorepo that keeps
// its Go modules in subdirectories got a hard failure from govulncheck and a
// misleading pass from gosec, which walks a tree happily without a module.
func Check(ctx context.Context, root string, exclude []string) ([]executil.Result, error) {
	modDirs, err := detect.GoModuleDirs(root, exclude)
	if err != nil {
		return nil, err
	}

	var results []executil.Result
	for _, dir := range modDirs {
		results = append(results, checkModule(ctx, dir, len(modDirs) > 1)...)
	}
	return results, nil
}

// checkModule runs both tools inside a single module directory. When more
// than one module was found, the directory is appended to each result name —
// matching eslint(<dir>) — so a failure names the module it came from.
func checkModule(ctx context.Context, dir string, qualify bool) []executil.Result {
	name := func(tool string) string {
		if qualify {
			return tool + "(" + dir + ")"
		}
		return tool
	}

	var results []executil.Result

	if bin, err := ensure(ctx, "gosec", gosecVersion, gosecPkg); err != nil {
		results = append(results, executil.Result{Name: name("gosec"), Err: err})
	} else {
		// -exclude-generated skips files carrying the standard
		// "Code generated ... DO NOT EDIT." header. Findings there are not
		// actionable: the only fix is to change the generator or its input,
		// and a `#nosec` annotation would be erased by the next regeneration.
		// This matches how generated code is already treated elsewhere in the
		// pipeline — golangci-lint's `exclusions: generated` and semgrep's own
		// generated-file skip.
		r := executil.Run(ctx, dir, bin, "-exclude-generated", "./...")
		r.Name = name("gosec")
		results = append(results, r)
	}

	if bin, err := ensure(ctx, "govulncheck", govulncheckVersion, govulncheckPkg); err != nil {
		results = append(results, executil.Result{Name: name("govulncheck"), Err: err})
	} else {
		r := executil.Run(ctx, dir, bin, "./...")
		r.Name = name("govulncheck")
		results = append(results, r)
	}

	return results
}

// ensure installs pkg via `go install` into a version-keyed bulwark cache
// directory (GOBIN), so a version bump gets a fresh install instead of
// silently reusing a stale one, and returns the path to the installed binary.
func ensure(ctx context.Context, name, version, pkg string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	binDir := filepath.Join(cacheDir, "bulwark", "gobin-"+name+"-"+version)
	bin := filepath.Join(binDir, name)

	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		return "", err
	}
	r := executil.RunEnv(ctx, "", []string{"GOBIN=" + binDir}, "go", "install", pkg)
	if !r.Ok() {
		return "", r.Err
	}
	return bin, nil
}
