// Package detect finds which ecosystems (Rust, TypeScript, Go) are present
// under a directory tree, so bulwark scan only runs the checks that apply.
package detect

import (
	"os"
	"path/filepath"
	"strings"
)

// Ecosystem is a language toolchain bulwark knows how to scan.
type Ecosystem string

const (
	Rust       Ecosystem = "rust"
	TypeScript Ecosystem = "typescript"
	Go         Ecosystem = "go"
)

// Extensions maps each Ecosystem to the file extensions bulwark considers
// part of it. Shared by any caller that needs to scope work (e.g. patch
// coverage's diff) to one ecosystem's own files, rather than re-deriving its
// own copy of this mapping.
var Extensions = map[Ecosystem][]string{
	Go:         {".go"},
	Rust:       {".rs"},
	TypeScript: {".ts", ".tsx"},
}

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

// RustCrateDirs returns every directory under root that is the effective
// root of an independent Cargo invocation: each directory containing a
// Cargo.toml, except a nested Cargo.toml whose nearest Cargo.toml ancestor
// (within the same walk) already declares a [workspace] table — cargo
// resolves workspace membership from that ancestor, so re-running
// fmt/clippy/audit/deny in a member crate's own directory would be redundant
// with running it once at the workspace root. Directories named in exclude
// (in addition to the built-in defaults) are skipped, matching TSPackageDirs.
func RustCrateDirs(root string, exclude []string) ([]string, error) {
	skip := skipSet(exclude)
	var all []string // every dir with a Cargo.toml, parent before child
	workspaceRoots := map[string]bool{}

	var visit func(dir string) error
	visit = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		hasCargoToml := false
		for _, e := range entries {
			if !e.IsDir() && e.Name() == "Cargo.toml" {
				hasCargoToml = true
			}
		}
		if hasCargoToml {
			all = append(all, dir)
			if isWorkspaceRoot(filepath.Join(dir, "Cargo.toml")) {
				workspaceRoots[dir] = true
			}
		}
		for _, e := range entries {
			if e.IsDir() && !skip[e.Name()] {
				if err := visit(filepath.Join(dir, e.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(root); err != nil {
		return nil, err
	}

	var dirs []string
	for _, dir := range all {
		if coveredByAncestorWorkspace(dir, workspaceRoots) {
			continue
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

// isWorkspaceRoot reports whether the Cargo.toml at path declares a
// [workspace] table. This is a line-level sniff, not a full TOML parse (no
// new dependency needed for this): it matches a trimmed line equal to
// "[workspace]" or starting with "[workspace." (covering
// [workspace.package], [workspace.dependencies], etc). A read or parse
// failure is treated as "not a workspace root" rather than an error —
// discovery's job is finding directories, not validating Cargo.toml
// correctness; a malformed Cargo.toml will fail loudly once cargo actually
// runs against it.
func isWorkspaceRoot(path string) bool {
	data, err := os.ReadFile(path) // #nosec G304 -- path is resolved by bulwark's own detection walk, not user input
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "[workspace]" || strings.HasPrefix(line, "[workspace.") {
			return true
		}
	}
	return false
}

// coveredByAncestorWorkspace reports whether dir sits strictly below some
// directory in workspaceRoots — i.e. whether an ancestor Cargo.toml already
// declares [workspace], making dir's own Cargo.toml a workspace member cargo
// already resolves from that ancestor.
func coveredByAncestorWorkspace(dir string, workspaceRoots map[string]bool) bool {
	for wsRoot := range workspaceRoots {
		if wsRoot == dir {
			continue
		}
		rel, err := filepath.Rel(wsRoot, dir)
		if err != nil {
			continue
		}
		if rel != "." && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}
