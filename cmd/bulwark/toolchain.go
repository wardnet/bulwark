package main

import (
	"context"

	"github.com/spf13/cobra"

	"wardnet/bulwark/internal/config"
	"wardnet/bulwark/internal/detect"
	"wardnet/bulwark/internal/toolchain"
)

// ensureToolchains makes each detected ecosystem's language toolchain
// available at the version the repo declares, and applies the result to this
// process so every scanner and coverage command launched afterwards sees it.
//
// Called by both `scan` and `coverage`, and in both cases before any tool
// runs: `scan` shells out to cargo/go/npx, and `coverage` shells out to `go
// test`, `cargo llvm-cov` and a package's `test:coverage`. Wiring it into the
// two command entry points rather than into each internal package keeps it to
// one call site per command and means the resolution happens exactly once per
// invocation, not once per discovered module.
//
// Only *enabled* ecosystems are passed. A language turned off in .bulwark.yml
// is one whose tools will never run, so provisioning a toolchain for it would
// be a download in service of nothing — and on a repo that disabled Rust
// precisely because its runner has no Rust, an actively unhelpful one.
//
// Diagnostics go to stderr. They are preparation notes, not gate results:
// stdout carries the `[PASS]`/`[FAIL]` and `[TAG]` lines that action.yml's PR
// comment scrapes, and that scraper matches any bracketed uppercase tag
// anywhere on a line rather than anchoring to the start, so keeping unrelated
// chatter off stdout is what stops it appearing in the comment.
func ensureToolchains(ctx context.Context, cmd *cobra.Command, dir string, cfg config.Config, ecosystems []detect.Ecosystem) error {
	env, err := toolchain.Ensure(ctx, dir, ecosystems, toolchainOverrides(cfg), cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	return env.Activate()
}

// toolchainOverrides maps .bulwark.yml onto the toolchain package's input.
// The mapping is explicit rather than internal/toolchain importing
// internal/config, so config stays a leaf that every other package can depend
// on without a cycle.
func toolchainOverrides(cfg config.Config) toolchain.Overrides {
	return toolchain.Overrides{
		Disabled: !cfg.Toolchain.Enabled,
		Go:       cfg.Toolchain.Go,
		Rust:     cfg.Toolchain.Rust,
		Node:     cfg.Toolchain.Node,
		// Per-language excludes, matching how scan and coverage already scope
		// detection: a Rust-only exclude must not narrow which package.json
		// files the Node requirement is read from.
		GoExclude:   cfg.Go.Exclude,
		RustExclude: cfg.Rust.Exclude,
		TSExclude:   cfg.TypeScript.Exclude,
	}
}
