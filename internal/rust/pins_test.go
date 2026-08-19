package rust

import "testing"

// TestPinnedVersionsParse guards the manifest→runtime path itself. Dependabot
// edits these manifests, so a bump that changed a line's shape (an inline table
// for a feature flag, a trailing comment) must fail here rather than silently
// yielding a corrupt value that reaches `cargo install --version`.
func TestPinnedVersionsParse(t *testing.T) {
	for _, tc := range []struct {
		manifest []byte
		crate    string
	}{
		{cargoAuditManifest, "cargo-audit"},
		{cargoDenyManifest, "cargo-deny"},
	} {
		v, err := pinnedVersion(tc.manifest, tc.crate)
		if err != nil {
			t.Errorf("pinnedVersion(%q): %v", tc.crate, err)
			continue
		}
		if v == "" || v[0] < '0' || v[0] > '9' {
			t.Errorf("pinnedVersion(%q) = %q; want a bare version with cargo's `=` operator stripped", tc.crate, v)
		}
	}
}

// TestPinManifestsAreSeparate guards the reason there are two manifests at all:
// cargo-audit and cargo-deny cannot resolve in one dependency graph, so a
// well-meaning consolidation would produce a manifest cargo errors on and
// Dependabot silently skips.
func TestPinManifestsAreSeparate(t *testing.T) {
	if _, err := pinnedVersion(cargoAuditManifest, "cargo-deny"); err == nil {
		t.Error("cargo-deny is declared in cargo-audit-pin; the two must not share a resolution graph")
	}
	if _, err := pinnedVersion(cargoDenyManifest, "cargo-audit"); err == nil {
		t.Error("cargo-audit is declared in cargo-deny-pin; the two must not share a resolution graph")
	}
}

func TestPinnedVersionMissingCrateIsAnError(t *testing.T) {
	if _, err := pinnedVersion(cargoAuditManifest, "cargo-nope"); err == nil {
		t.Error("pinnedVersion accepted a crate the manifest does not pin")
	}
}

// TestPinnedVersionIgnoresPackageMetadata guards a real ambiguity: the manifest
// also carries `version = "0.0.0"` and `name = "..."` in its [package] table,
// and a section-blind parser happily returns those.
func TestPinnedVersionIgnoresPackageMetadata(t *testing.T) {
	if v, err := pinnedVersion(cargoAuditManifest, "version"); err == nil {
		t.Errorf("pinnedVersion matched the [package] version field, returning %q", v)
	}
}

// TestPinnedVersionStripsInlineComment covers the shape that previously slipped
// through the "fail loudly" promise: only the leading quote was stripped, so the
// version carried the comment along into `cargo install --version`.
func TestPinnedVersionStripsInlineComment(t *testing.T) {
	manifest := []byte("[dependencies]\ncargo-audit = \"=1.2.3\" # pinned deliberately\n")
	v, err := pinnedVersion(manifest, "cargo-audit")
	if err != nil {
		t.Fatalf("pinnedVersion: %v", err)
	}
	if v != "1.2.3" {
		t.Errorf("pinnedVersion = %q, want %q", v, "1.2.3")
	}
}

func TestPinnedVersionRejectsUnquotedValue(t *testing.T) {
	if _, err := pinnedVersion([]byte("[dependencies]\ncargo-audit = 1.2.3\n"), "cargo-audit"); err == nil {
		t.Error("pinnedVersion accepted an unquoted version")
	}
}
