// Package semgrep runs Semgrep against a directory tree. Semgrep is a
// separate Python-packaged binary (not Rust or Go tooling), so unlike gosec/
// govulncheck it's installed via pipx rather than a language-native install
// command; bulwark still ensures the pinned version is what's actually
// installed (not just "something called semgrep exists on PATH"), for the
// same toolchain-reproducibility reason as everything else it runs.
package semgrep

import (
	"context"
	"os"
	"strings"

	"wardnet/bulwark/internal/executil"
)

// Pinned so every invocation of bulwark uses the exact same Semgrep version
// regardless of what's already on the machine.
const version = "1.168.0"

// AppTokenEnv is the environment variable Semgrep itself reads for AppSec
// Platform authentication. bulwark doesn't invent its own — reusing
// Semgrep's own variable means a caller (e.g. the wardnet/bulwark GitHub
// Action) only has to plumb one secret through, and `semgrep ci` still works
// exactly as documented if invoked directly outside of bulwark too.
const AppTokenEnv = "SEMGREP_APP_TOKEN" // #nosec G101 -- this is an env var NAME, not a credential value

// Check runs Semgrep against dir using the given ruleset config (e.g. "auto",
// or a custom registry ref/path from .bulwark.yml), failing on any finding.
//
// When SEMGREP_APP_TOKEN is set in the environment, this runs `semgrep ci`
// instead of `semgrep scan` — Semgrep's own diff-aware CI mode, which both
// scopes findings to what the current change actually introduced and
// uploads results to the Semgrep AppSec Platform dashboard, in one
// invocation. Without a token (the common case for local dev), behavior is
// unchanged: a plain `semgrep scan` against the configured ruleset.
func Check(ctx context.Context, dir, rulesetConfig string) executil.Result {
	if r := ensure(ctx); !r.Ok() {
		return r
	}
	return executil.Run(ctx, dir, "semgrep", buildArgs(rulesetConfig, os.Getenv(AppTokenEnv) != "")...)
}

// buildArgs decides the semgrep subcommand and flags: `ci` (diff-aware,
// uploads to the AppSec Platform) when appToken is set, otherwise a plain
// `scan`. `ci` mode omits --config entirely for the "auto" sentinel, since
// semgrep ci already applies the org's configured platform policy by
// default — passing "--config auto" would override that with the plain
// community ruleset instead of layering on top of it.
func buildArgs(rulesetConfig string, appToken bool) []string {
	if appToken {
		args := []string{"ci"}
		if rulesetConfig != "" && rulesetConfig != "auto" {
			args = append(args, "--config", rulesetConfig)
		}
		return args
	}
	return []string{"scan", "--config", rulesetConfig, "--error"}
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
