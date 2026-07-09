// Package golang runs gosec and govulncheck against a Go module. Both are
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

// Check runs gosec and govulncheck against the Go module rooted at dir.
func Check(ctx context.Context, dir string) []executil.Result {
	var results []executil.Result

	if bin, err := ensure(ctx, "gosec", gosecVersion, gosecPkg); err != nil {
		results = append(results, executil.Result{Name: "gosec", Err: err})
	} else {
		r := executil.Run(ctx, dir, bin, "./...")
		r.Name = "gosec"
		results = append(results, r)
	}

	if bin, err := ensure(ctx, "govulncheck", govulncheckVersion, govulncheckPkg); err != nil {
		results = append(results, executil.Result{Name: "govulncheck", Err: err})
	} else {
		r := executil.Run(ctx, dir, bin, "./...")
		r.Name = "govulncheck"
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
