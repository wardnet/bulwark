// Package rust runs Rust checks: fmt, clippy, cargo-audit, cargo-deny.
//
// clippy's lint groups (pedantic/restriction) are configured by the target
// project's own Cargo.toml ([workspace.lints.clippy]), not by bulwark — this
// package only invokes the tools with -D warnings so whatever the project
// declares is enforced as an error. Likewise, clippy/fmt's own toolchain
// version is the target repo's responsibility via its own rust-toolchain.toml
// (the standard rustup convention for pinning rustc/clippy/rustfmt together) —
// bulwark doesn't second-guess that. cargo-audit and cargo-deny are different:
// they're standalone cargo subcommands with no equivalent per-repo pin, so —
// like every other scanner's toolchain (gosec/govulncheck, ESLint, Semgrep) —
// bulwark pins their exact versions and installs them into a version-keyed
// cache directory rather than trusting whatever's already on PATH.
package rust

import (
	"context"
	"os"
	"path/filepath"

	"wardnet/bulwark/internal/executil"
)

// Pinned so every invocation of bulwark uses the exact same toolchain
// regardless of what's already on the machine.
const (
	cargoAuditVersion = "0.22.2"
	cargoDenyVersion  = "0.20.2"
)

// Check runs every Rust check against the Cargo workspace rooted at dir.
func Check(ctx context.Context, dir string) []executil.Result {
	results := []executil.Result{
		named("cargo fmt", executil.Run(ctx, dir, "cargo", "fmt", "--check")),
		named("cargo clippy", executil.Run(ctx, dir, "cargo", "clippy", "--all-targets", "--", "-D", "warnings")),
	}

	if bin, err := ensure(ctx, "cargo-audit", cargoAuditVersion); err != nil {
		results = append(results, executil.Result{Name: "cargo-audit", Err: err})
	} else {
		results = append(results, named("cargo-audit", executil.Run(ctx, dir, bin, "audit")))
	}

	if bin, err := ensure(ctx, "cargo-deny", cargoDenyVersion); err != nil {
		results = append(results, executil.Result{Name: "cargo-deny", Err: err})
	} else {
		// advisories is intentionally excluded here: cargo-audit already
		// covers RustSec CVEs, and running both would double-report them.
		results = append(results, named("cargo-deny", executil.Run(ctx, dir, bin, "deny", "check", "licenses", "bans")))
	}

	return results
}

// named returns r with Name overridden, so scan's report distinguishes each
// of the four Rust checks instead of every one showing as the literal binary
// name ("cargo").
func named(name string, r executil.Result) executil.Result {
	r.Name = name
	return r
}

// ensure installs cargo-<name> at the given version into a version-keyed
// bulwark cache directory (via `cargo install --root`), so a version bump
// gets a fresh install instead of silently reusing whatever's on PATH, and
// returns the path to the installed binary. cargo-audit/cargo-deny are both
// invoked as `cargo <name> ...`, but a `--root`-installed binary is named
// plainly `cargo-<name>` and must be run directly (not via `cargo <name>`,
// which only finds cargo-* binaries already on PATH).
func ensure(ctx context.Context, name, version string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(cacheDir, "bulwark", name+"-"+version)
	bin := filepath.Join(root, "bin", name)

	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", err
	}
	r := executil.Run(ctx, "", "cargo", "install", "--locked", "--root", root, "--version", version, name)
	if !r.Ok() {
		return "", r.Err
	}
	return bin, nil
}
