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

func TestGoModuleDirsSingleModuleAtRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/foo\n")

	dirs, err := GoModuleDirs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sorted(dirs); len(got) != 1 || got[0] != root {
		t.Fatalf("got %v, want [%s]", got, root)
	}
}

// The regression this whole function exists for: a scan root that holds no
// go.mod itself, with the real modules in subdirectories. Running the tools
// at the root made govulncheck exit "no go.mod file".
func TestGoModuleDirsModulesInSubdirectories(t *testing.T) {
	root := t.TempDir()
	sdk := filepath.Join(root, "sdk", "wardnet-go")
	cli := filepath.Join(root, "wctl")
	writeFile(t, filepath.Join(sdk, "go.mod"), "module wardnet.network/go\n")
	writeFile(t, filepath.Join(cli, "go.mod"), "module example.com/wctl\n")
	writeFile(t, filepath.Join(root, "daemon", "src", "main.rs"), "fn main() {}\n")

	dirs, err := GoModuleDirs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := sorted(dirs)
	want := sorted([]string{sdk, cli})
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A nested go.mod is a separate module excluded from its parent's package
// graph, so `./...` at the parent never reaches it — both must be returned.
// This is the point of difference from RustCrateDirs, where a member crate is
// deliberately folded into its workspace root.
func TestGoModuleDirsNestedModuleIsIndependent(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "tools", "gen")
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/outer\n")
	writeFile(t, filepath.Join(nested, "go.mod"), "module example.com/outer/tools/gen\n")

	dirs, err := GoModuleDirs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := sorted(dirs)
	want := sorted([]string{root, nested})
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestGoModuleDirsRespectsExclude(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "keep")
	writeFile(t, filepath.Join(keep, "go.mod"), "module example.com/keep\n")
	writeFile(t, filepath.Join(root, "legacy", "go.mod"), "module example.com/legacy\n")

	dirs, err := GoModuleDirs(root, []string{"legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sorted(dirs); len(got) != 1 || got[0] != keep {
		t.Fatalf("got %v, want [%s]", got, keep)
	}
}

func TestGoModuleDirsDefaultSkipDirsHonored(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/foo\n")
	writeFile(t, filepath.Join(root, "vendor", "dep", "go.mod"), "module example.com/dep\n")

	dirs, err := GoModuleDirs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sorted(dirs); len(got) != 1 || got[0] != root {
		t.Fatalf("got %v, want [%s]", got, root)
	}
}

// A go.mod under testdata/ is a fixture, not a project — often deliberately
// unbuildable or pinned to a vulnerable dependency to exercise a scanner. Go's
// own package loading ignores testdata, "_"- and "."-prefixed directories, and
// discovery has to agree or the scan fails on code that never ships.
func TestGoModuleDirsSkipsGoIgnoredDirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/foo\n")
	writeFile(t, filepath.Join(root, "testdata", "broken", "go.mod"), "module example.com/fixture\n")
	writeFile(t, filepath.Join(root, "_scratch", "go.mod"), "module example.com/scratch\n")
	writeFile(t, filepath.Join(root, ".tools", "go.mod"), "module example.com/tools\n")

	dirs, err := GoModuleDirs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sorted(dirs); len(got) != 1 || got[0] != root {
		t.Fatalf("got %v, want [%s]", got, root)
	}
}
