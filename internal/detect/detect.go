// Package detect finds which ecosystems (Rust, TypeScript, Go) are present
// under a directory tree, so bulwark scan only runs the checks that apply.
package detect

import "os"

// Ecosystem is a language toolchain bulwark knows how to scan.
type Ecosystem string

const (
	Rust       Ecosystem = "rust"
	TypeScript Ecosystem = "typescript"
	Go         Ecosystem = "go"
)

var skipDirs = map[string]bool{
	"node_modules": true, "target": true, ".git": true, ".bare": true,
	"dist": true, "vendor": true, "build": true,
}

// walk calls fn for every file under root, skipping vendor/build directories.
func walk(root string, fn func(dirEntryName string, isDir bool)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if skipDirs[e.Name()] {
				continue
			}
			if err := walk(root+string(os.PathSeparator)+e.Name(), fn); err != nil {
				return err
			}
			continue
		}
		fn(e.Name(), false)
	}
	return nil
}

// Ecosystems reports every supported ecosystem found under root.
func Ecosystems(root string) ([]Ecosystem, error) {
	found := map[Ecosystem]bool{}
	err := walk(root, func(name string, _ bool) {
		switch name {
		case "Cargo.toml":
			found[Rust] = true
		case "package.json":
			found[TypeScript] = true
		case "go.mod":
			found[Go] = true
		}
	})
	if err != nil {
		return nil, err
	}
	var out []Ecosystem
	for _, e := range []Ecosystem{Rust, TypeScript, Go} {
		if found[e] {
			out = append(out, e)
		}
	}
	return out, nil
}

// TSPackageDirs returns every directory under root containing a package.json,
// so each TypeScript package can be linted independently.
func TSPackageDirs(root string) ([]string, error) {
	var dirs []string
	var visit func(dir string) error
	visit = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		hasPkg := false
		for _, e := range entries {
			if !e.IsDir() && e.Name() == "package.json" {
				hasPkg = true
			}
		}
		if hasPkg {
			dirs = append(dirs, dir)
		}
		for _, e := range entries {
			if e.IsDir() && !skipDirs[e.Name()] {
				if err := visit(dir + string(os.PathSeparator) + e.Name()); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(root); err != nil {
		return nil, err
	}
	return dirs, nil
}
