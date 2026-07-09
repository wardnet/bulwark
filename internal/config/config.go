// Package config loads .bulwark.yml, an optional, purely opt-out config file:
// bulwark's default (no file present) is to scan everything it detects with
// every check enabled. The file can only narrow that — disable a language's
// checks entirely, exclude specific paths from ecosystem/package detection,
// or override Semgrep's ruleset — not tune severity or suppress individual
// findings (that's what a fix-up pass + #nosec/nosemgrep annotations in the
// scanned repo itself are for).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// FileName is the config file bulwark looks for at the scan root.
const FileName = ".bulwark.yml"

// Language is the opt-out surface for one of the three supported ecosystems.
type Language struct {
	Enabled bool     `yaml:"enabled"`
	Exclude []string `yaml:"exclude"`
}

// Semgrep is the opt-out/override surface for the Semgrep check.
type Semgrep struct {
	Enabled bool   `yaml:"enabled"`
	Config  string `yaml:"config"`
}

// Config is bulwark's full, resolved configuration for one scan.
type Config struct {
	Rust       Language `yaml:"rust"`
	TypeScript Language `yaml:"typescript"`
	Go         Language `yaml:"go"`
	Semgrep    Semgrep  `yaml:"semgrep"`
}

// Default returns bulwark's zero-config behavior: every language and Semgrep
// enabled, no excludes, Semgrep's ruleset set to "auto".
func Default() Config {
	return Config{
		Rust:       Language{Enabled: true},
		TypeScript: Language{Enabled: true},
		Go:         Language{Enabled: true},
		Semgrep:    Semgrep{Enabled: true, Config: "auto"},
	}
}

// Load reads .bulwark.yml from root if present, merging it onto Default().
// A missing file is not an error — it's the common case. Merge semantics:
// yaml.Unmarshal only overwrites fields explicitly present in the file, so a
// section omitted entirely (or a key omitted within a present section) keeps
// its Default() value rather than being zeroed.
func Load(root string) (Config, error) {
	cfg := Default()
	path := filepath.Join(root, FileName)
	data, err := os.ReadFile(path) // #nosec G304 -- root is the CLI's own --dir flag, supplied by whoever runs bulwark, not untrusted remote input
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}
