package rust

import (
	"context"
	"testing"
)

// Full end-to-end execution of cargo fmt/clippy/cargo-audit/cargo-deny isn't
// tested here: unlike Go (always available in this repo's own dev/CI
// environment), cargo and the pinned cargo-audit/cargo-deny binaries aren't
// guaranteed present in every environment running `go test ./...` (e.g. a
// contributor's machine without Rust installed). These tests exercise
// discovery/dispatch logic without invoking cargo.

func TestCheckReturnsErrorWhenDiscoveryFails(t *testing.T) {
	_, err := Check(context.Background(), "/nonexistent/path/that/does/not/exist", nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestCheckNoCrateDirsReturnsNilResults(t *testing.T) {
	results, err := Check(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if results != nil {
		t.Fatalf("got %v, want nil", results)
	}
}

func TestCrateLabel(t *testing.T) {
	cases := []struct {
		name  string
		root  string
		dir   string
		multi bool
		want  string
	}{
		{"single crate no label", "/repo", "/repo", false, ""},
		{"multi crate at root", "/repo", "/repo", true, ""},
		{"multi crate nested", "/repo", "/repo/crates/foo", true, "crates/foo: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := crateLabel(tc.root, tc.dir, tc.multi)
			if got != tc.want {
				t.Errorf("crateLabel(%q, %q, %v) = %q, want %q", tc.root, tc.dir, tc.multi, got, tc.want)
			}
		})
	}
}
