package toolchain

import "testing"

// canonical has to absorb every spelling the three ecosystems and their
// version commands use, because each of them is a real input: go.mod says
// "1.26.4", the toolchain directive says "go1.26.5", `node --version` says
// "v22.21.1", and rustup channels carry host triples.
func TestCanonical(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1.26.4", "v1.26.4"},
		{"go1.26.5", "v1.26.5"},
		{"v22.21.1", "v22.21.1"},
		{"22", "v22"},
		{"1.96", "v1.96"},
		{"  1.96.0  ", "v1.96.0"},
		// rustup channels carry a host triple; Go pre-releases carry rc/beta.
		{"1.85.0-x86_64-unknown-linux-gnu", "v1.85.0"},
		{"1.26rc1", "v1.26"},
		{"1.96.0-beta.1", "v1.96.0"},
		// Named channels and aliases name a moving target, not a floor, so
		// they must produce no comparable version rather than a guess.
		{"stable", ""},
		{"nightly", ""},
		{"lts/*", ""},
		{"", ""},
		{"garbage", ""},
	} {
		if got := canonical(tc.in); got != tc.want {
			t.Errorf("canonical(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A partial version must stay partial. "v1.26" has to order below "v1.26.5"
// so that a go.mod saying `go 1.26` is satisfied by an ambient 1.26.5 — the
// correct reading of a minimum. Zero-padding to "v1.26.0" would still compare
// correctly here but would be wrong wherever the value is used as an exact
// pin, so the invariant is worth stating.
func TestCanonicalKeepsPartialVersionsPartial(t *testing.T) {
	if got := canonical("1.26"); got != "v1.26" {
		t.Fatalf("canonical(\"1.26\") = %q, want v1.26 (unpadded)", got)
	}
	if olderThan(canonical("1.26.5"), canonical("1.26")) {
		t.Fatal("1.26.5 must satisfy a floor of 1.26")
	}
}

func TestOlderThan(t *testing.T) {
	for _, tc := range []struct {
		have, want string
		older      bool
	}{
		{"v1.25.0", "v1.26.0", true},
		{"v1.26.0", "v1.26.0", false},
		{"v1.26.5", "v1.26.0", false},
		{"v1.26.5", "v1.26", false},
		{"v1.26", "v1.26.5", true},
		// No requirement constrains nothing.
		{"v1.0.0", "", false},
		{"", "", false},
		// An unidentifiable ambient toolchain is treated as too old: it
		// cannot be shown to satisfy a pin, and provisioning is the safe
		// side of that doubt.
		{"", "v1.26.0", true},
	} {
		if got := olderThan(tc.have, tc.want); got != tc.older {
			t.Errorf("olderThan(%q, %q) = %v, want %v", tc.have, tc.want, got, tc.older)
		}
	}
}

func TestMaxVersion(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"v1.25.0", "v1.26.0", "v1.26.0"},
		{"v1.26.0", "v1.25.0", "v1.26.0"},
		{"", "v1.26.0", "v1.26.0"},
		{"v1.26.0", "", "v1.26.0"},
		{"", "", ""},
		{"v1.26", "v1.26.5", "v1.26.5"},
	} {
		if got := maxVersion(tc.a, tc.b); got != tc.want {
			t.Errorf("maxVersion(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

// engines.node is a range, and bulwark only ever needs its floor — see
// minimumOfRange for why full range semantics are deliberately not
// implemented.
func TestMinimumOfRange(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{">=20", "v20"},
		{">= 20", "v20"},
		{"^22.0.0", "v22.0.0"},
		{"~22.11", "v22.11"},
		{"20.x", "v20"},
		{"20.X", "v20"},
		{"22.*", "v22"},
		{">=20 <23", "v20"},
		{"22 || 24", "v22"},
		{"22.11.0", "v22.11.0"},
		{"*", ""},
		{"", ""},
		// A pure ceiling gives no floor. Reading "<23" as a minimum of 23
		// would provision a toolchain the range actually forbids.
		{"<23", ""},
		{"<=22", ""},
	} {
		if got := minimumOfRange(tc.in); got != tc.want {
			t.Errorf("minimumOfRange(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestToolchainNamePadsToPatch(t *testing.T) {
	// Go toolchains are always named to the patch level, so a bare `go 1.26`
	// resolves to that minor's initial release — the oldest that satisfies it.
	for _, tc := range []struct{ in, want string }{
		{"v1.26.5", "go1.26.5"},
		{"v1.26", "go1.26.0"},
		{"v2", "go2.0.0"},
	} {
		if got := toolchainName(tc.in); got != tc.want {
			t.Errorf("toolchainName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNodeReleaseNamePadsToPatch(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"v22.21.1", "v22.21.1"},
		{"v22", "v22.0.0"},
		{"v22.11", "v22.11.0"},
	} {
		if got := nodeReleaseName(tc.in); got != tc.want {
			t.Errorf("nodeReleaseName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
