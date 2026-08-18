package semgrep

import "testing"

// TestPinnedVersionParses guards the manifest→runtime path: Dependabot edits
// requirements.txt, and a bump whose shape this parser can't read must fail
// here rather than becoming `pipx install semgrep==`.
func TestPinnedVersionParses(t *testing.T) {
	v, err := pinnedVersion(requirements)
	if err != nil {
		t.Fatalf("pinnedVersion: %v", err)
	}
	if v == "" {
		t.Fatal("pinnedVersion is empty")
	}
	if v != version {
		t.Errorf("pinnedVersion = %q but package-level version = %q", v, version)
	}
}

func TestPinnedVersionSkipsCommentsAndBlanks(t *testing.T) {
	if _, err := pinnedVersion([]byte("# semgrep==9.9.9\n\n")); err == nil {
		t.Error("pinnedVersion read a version out of a comment")
	}
}

func TestPinnedVersionRequiresAPin(t *testing.T) {
	if _, err := pinnedVersion([]byte("semgrep\n")); err == nil {
		t.Error("pinnedVersion accepted an unpinned requirement")
	}
}

// TestPinnedVersionRejectsTrailingContent covers the shapes a requirements line
// can legitimately grow that the Cargo parser is immune to (its quoted value
// delimits the version, this one has no terminator): an environment marker, or a
// hash Dependabot appends. Both would otherwise reach `pipx install semgrep==`
// verbatim.
func TestPinnedVersionRejectsTrailingContent(t *testing.T) {
	for _, line := range []string{
		"semgrep==1.168.0 ; python_version >= \"3.9\"\n",
		"semgrep==1.168.0 --hash=sha256:abc123\n",
	} {
		if v, err := pinnedVersion([]byte(line)); err == nil {
			t.Errorf("pinnedVersion(%q) returned %q; want an error", line, v)
		}
	}
}
