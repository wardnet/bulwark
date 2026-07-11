package detect

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sorted(dirs []string) []string {
	out := append([]string(nil), dirs...)
	sort.Strings(out)
	return out
}

func TestRustCrateDirsSingleCrate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Cargo.toml"), "[package]\nname = \"foo\"\n")

	dirs, err := RustCrateDirs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sorted(dirs); len(got) != 1 || got[0] != root {
		t.Fatalf("got %v, want [%s]", got, root)
	}
}

func TestRustCrateDirsWorkspaceRootOnly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Cargo.toml"), "[workspace]\nmembers = [\"a\", \"b\"]\n")
	writeFile(t, filepath.Join(root, "a", "Cargo.toml"), "[package]\nname = \"a\"\n")
	writeFile(t, filepath.Join(root, "b", "Cargo.toml"), "[package]\nname = \"b\"\n")

	dirs, err := RustCrateDirs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sorted(dirs); len(got) != 1 || got[0] != root {
		t.Fatalf("got %v, want [%s]", got, root)
	}
}

func TestRustCrateDirsIndependentCratesNoWorkspace(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a", "Cargo.toml"), "[package]\nname = \"a\"\n")
	writeFile(t, filepath.Join(root, "b", "Cargo.toml"), "[package]\nname = \"b\"\n")

	dirs, err := RustCrateDirs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := sorted([]string{filepath.Join(root, "a"), filepath.Join(root, "b")})
	if got := sorted(dirs); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRustCrateDirsNestedIndependentWorkspace(t *testing.T) {
	// Defensive edge case: real nested Cargo workspaces are disallowed by
	// cargo itself, but discovery should still not double-report.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Cargo.toml"), "[workspace]\nmembers = [\"a\"]\n")
	writeFile(t, filepath.Join(root, "a", "Cargo.toml"), "[workspace]\nmembers = [\"inner\"]\n")
	writeFile(t, filepath.Join(root, "a", "inner", "Cargo.toml"), "[package]\nname = \"inner\"\n")

	dirs, err := RustCrateDirs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sorted(dirs); len(got) != 1 || got[0] != root {
		t.Fatalf("got %v, want [%s]", got, root)
	}
}

func TestRustCrateDirsRespectsExclude(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "fixtures", "Cargo.toml"), "[package]\nname = \"fixture\"\n")

	dirs, err := RustCrateDirs(root, []string{"fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 0 {
		t.Fatalf("got %v, want none", dirs)
	}
}

func TestRustCrateDirsDefaultSkipDirsHonored(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "target", "package", "Cargo.toml"), "[package]\nname = \"vendored\"\n")

	dirs, err := RustCrateDirs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 0 {
		t.Fatalf("got %v, want none", dirs)
	}
}
