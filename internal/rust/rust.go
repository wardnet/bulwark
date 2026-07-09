// Package rust runs Rust checks: fmt, clippy, cargo-audit, cargo-deny.
//
// clippy's lint groups (pedantic/restriction) are configured by the target
// project's own Cargo.toml ([workspace.lints.clippy]), not by bulwark — this
// package only invokes the tools with -D warnings so whatever the project
// declares is enforced as an error.
package rust

import (
	"context"
	"fmt"

	"wardnet/bulwark/internal/executil"
)

// Check runs every Rust check against the Cargo workspace rooted at dir.
func Check(ctx context.Context, dir string) []executil.Result {
	results := []executil.Result{
		executil.Run(ctx, dir, "cargo", "fmt", "--check"),
		executil.Run(ctx, dir, "cargo", "clippy", "--all-targets", "--", "-D", "warnings"),
	}

	if !executil.Available("cargo-audit") {
		results = append(results, missingTool("cargo-audit",
			"cargo install cargo-audit --locked"))
	} else {
		results = append(results, executil.Run(ctx, dir, "cargo", "audit"))
	}

	if !executil.Available("cargo-deny") {
		results = append(results, missingTool("cargo-deny",
			"cargo install cargo-deny --locked"))
	} else {
		// advisories is intentionally excluded here: cargo-audit already
		// covers RustSec CVEs, and running both would double-report them.
		results = append(results, executil.Run(ctx, dir, "cargo", "deny", "check", "licenses", "bans"))
	}

	return results
}

func missingTool(name, installHint string) executil.Result {
	return executil.Result{
		Name: name,
		Err:  fmt.Errorf("%s not found on PATH — install with: %s", name, installHint),
	}
}
