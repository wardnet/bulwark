package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// ecosystem is a language toolchain bulwark knows how to scan.
type ecosystem string

const (
	ecosystemRust ecosystem = "rust"
	ecosystemTS   ecosystem = "typescript"
	ecosystemGo   ecosystem = "go"
)

// detectEcosystems walks root looking for the manifest files that mark each
// supported ecosystem. It does not descend into vendor/build directories.
func detectEcosystems(root string) ([]ecosystem, error) {
	found := map[ecosystem]bool{}
	skip := map[string]bool{
		"node_modules": true, "target": true, ".git": true, ".bare": true,
		"dist": true, "vendor": true,
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && skip[d.Name()] {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		switch d.Name() {
		case "Cargo.toml":
			found[ecosystemRust] = true
		case "package.json":
			found[ecosystemTS] = true
		case "go.mod":
			found[ecosystemGo] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []ecosystem
	for _, e := range []ecosystem{ecosystemRust, ecosystemTS, ecosystemGo} {
		if found[e] {
			out = append(out, e)
		}
	}
	return out, nil
}

func newScanCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Run code-quality and security checks for every detected ecosystem",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ecosystems, err := detectEcosystems(dir)
			if err != nil {
				return err
			}
			if len(ecosystems) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "no supported ecosystem detected under", dir)
				return err
			}
			for _, e := range ecosystems {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "detected %s — scanner not yet implemented\n", e); err != nil {
					return err
				}
			}
			return fmt.Errorf("bulwark scan is not implemented yet")
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "root directory to scan")
	return cmd
}
