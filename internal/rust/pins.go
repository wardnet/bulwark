package rust

import (
	_ "embed"
	"fmt"
	"strings"
)

// One manifest per tool: cargo-audit and cargo-deny cannot resolve in a shared
// dependency graph (cargo-deny's krates pins petgraph =0.8.1, cargo-audit's
// cargo-lock pulls 0.8.2), and a manifest that cannot resolve is one Dependabot
// silently errors on — the exact "pin nothing ages out" failure this arrangement
// exists to prevent.
var (
	//go:embed cargo-audit-pin/Cargo.toml
	cargoAuditManifest []byte
	//go:embed cargo-deny-pin/Cargo.toml
	cargoDenyManifest []byte
)

// cargoAuditVersion and cargoDenyVersion are read from cargo-pin/Cargo.toml
// rather than written here, so that Dependabot has a manifest it understands
// and there is only one place a version can be wrong. A pinned security tool
// that nothing ever ages out is a scanner that quietly goes stale while still
// reporting [PASS].
var (
	cargoAuditVersion = mustPinnedVersion(cargoAuditManifest, "cargo-audit")
	cargoDenyVersion  = mustPinnedVersion(cargoDenyManifest, "cargo-deny")
)

// pinnedVersion extracts `crate = "=x.y.z"` from the embedded manifest's
// [dependencies] table.
//
// Two lines of TOML do not justify a TOML dependency, but they do justify being
// strict on two counts. An unparseable manifest must fail loudly rather than
// yield "", which would turn into `cargo install cargo-audit@` — a confusing
// failure far from its cause, or worse, an install of whatever version is
// latest. And the scan must be scoped to [dependencies]: the manifest also
// carries `version = "0.0.0"` and `name = "..."` in its [package] table, so a
// section-blind parser answers a lookup with unrelated package metadata.
func pinnedVersion(manifest []byte, crate string) (string, error) {
	inDeps := false
	for line := range strings.Lines(string(manifest)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inDeps = line == "[dependencies]"
			continue
		}
		if !inDeps {
			continue
		}
		name, rest, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != crate {
			continue
		}
		v := strings.TrimSpace(rest)
		// Strip a trailing inline comment before unquoting. Without this,
		// `cargo-audit = "=0.22.2" # note` survives as `0.22.2" # note` and is
		// handed to `cargo install --version` — a corrupt value where the doc
		// above promises a loud failure. This repo already appends `# nosemgrep:`
		// comments to config lines elsewhere, so it is not a hypothetical shape.
		if i := strings.Index(v, "#"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		unquoted, ok := strings.CutPrefix(v, `"`)
		if !ok {
			return "", fmt.Errorf("%s: version for %s is not a quoted string", crate, crate)
		}
		v, ok = strings.CutSuffix(unquoted, `"`)
		if !ok {
			return "", fmt.Errorf("%s: version for %s has no closing quote", crate, crate)
		}
		// The `=` prefix is cargo's exact-version requirement operator; bulwark
		// wants the bare version to hand to `cargo install --version`.
		v = strings.TrimPrefix(v, "=")
		if v == "" {
			return "", fmt.Errorf("%s has an empty version", crate)
		}
		return v, nil
	}
	return "", fmt.Errorf("no [dependencies] entry for %s", crate)
}

func mustPinnedVersion(manifest []byte, crate string) string {
	v, err := pinnedVersion(manifest, crate)
	if err != nil {
		panic(err)
	}
	return v
}
