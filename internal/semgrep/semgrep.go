// Package semgrep runs Semgrep against a directory tree. Semgrep is a
// separate Python-packaged binary (not Rust or Go tooling), so unlike gosec/
// govulncheck it's installed via pipx rather than a language-native install
// command; bulwark still ensures the pinned version is what's actually
// installed (not just "something called semgrep exists on PATH"), for the
// same toolchain-reproducibility reason as everything else it runs.
package semgrep

import (
	"context"
	"strings"

	"wardnet/bulwark/internal/executil"
)

// Pinned so every invocation of bulwark uses the exact same Semgrep version
// regardless of what's already on the machine.
const version = "1.168.0"

// Check runs Semgrep against dir using the given ruleset config (e.g. "auto",
// or a custom registry ref/path from .bulwark.yml), failing on any finding.
func Check(ctx context.Context, dir, rulesetConfig string) executil.Result {
	if r := ensure(ctx); !r.Ok() {
		return r
	}
	return executil.Run(ctx, dir, "semgrep", "scan", "--config", rulesetConfig, "--error")
}

// ensure installs the pinned Semgrep version via pipx unless it's already
// installed at exactly that version.
func ensure(ctx context.Context) executil.Result {
	if executil.Available("semgrep") {
		v := executil.Run(ctx, "", "semgrep", "--version")
		if v.Ok() && strings.TrimSpace(v.Output) == version {
			return executil.Result{Name: "semgrep"}
		}
	}
	r := executil.Run(ctx, "", "pipx", "install", "--force", "semgrep=="+version)
	// Override Name: executil.Run sets it to the literal binary invoked
	// ("pipx"), but a failure here means the Semgrep check itself never ran —
	// report() should say so, not "pipx".
	r.Name = "semgrep"
	return r
}
