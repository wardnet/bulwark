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

var defaultSkipDirs = map[string]bool{
	"node_modules": true, "target": true, ".git": true, ".bare": true,
	"dist": true, "vendor": true, "build": true,
}

// skipSet merges extra directory names (from .bulwark.yml's exclude lists)
// onto the built-in skip set.
func skipSet(extra []string) map[string]bool {
	if len(extra) == 0 {
		return defaultSkipDirs
	}
	set := make(map[string]bool, len(defaultSkipDirs)+len(extra))
	for k := range defaultSkipDirs {
		set[k] = true
	}
	for _, e := range extra {
		set[e] = true
	}
	return set
}

// walk calls fn for every file under root, skipping directories in skip.
func walk(root string, skip map[string]bool, fn func(dirEntryName string, isDir bool)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if skip[e.Name()] {
				continue
			}
			if err := walk(root+string(os.PathSeparator)+e.Name(), skip, fn); err != nil {
				return err
			}
			continue
		}
		fn(e.Name(), false)
	}
	return nil
}

// Ecosystems reports every supported ecosystem found under root, skipping
// any directory named in exclude in addition to the built-in defaults.
func Ecosystems(root string, exclude []string) ([]Ecosystem, error) {
	found := map[Ecosystem]bool{}
	err := walk(root, skipSet(exclude), func(name string, _ bool) {
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
// so each TypeScript package can be linted independently. Directories named
// in exclude (in addition to the built-in defaults) are skipped.
func TSPackageDirs(root string, exclude []string) ([]string, error) {
	skip := skipSet(exclude)
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
			if e.IsDir() && !skip[e.Name()] {
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
