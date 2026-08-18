package main

import (
	"testing"

	"wardnet/bulwark/internal/config"
	"wardnet/bulwark/internal/typescript"
)

// TestLinterConstantsAgree ties the two Linter vocabularies together.
//
// scan.go bridges them with an unchecked string conversion
// (typescript.Linter(cfg.TypeScript.Linter)), and typescript.Check treats
// anything that isn't Biome as ESLint. So if the two constant sets ever diverge
// — renaming config.LinterBiome's value without renaming typescript.Biome's —
// validateLinter still accepts the config, the repo believes it opted in, and
// bulwark silently runs ESLint. That is precisely the failure validateLinter
// exists to prevent, moved one layer down where nothing was watching.
//
// internal/typescript deliberately does not import internal/config (it takes a
// plain parameter rather than depending on the config package), so this
// agreement can only be checked from a package that sees both.
func TestLinterConstantsAgree(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Linter
		ts   typescript.Linter
	}{
		{"eslint", config.LinterESLint, typescript.ESLint},
		{"biome", config.LinterBiome, typescript.Biome},
	} {
		if string(tc.cfg) != string(tc.ts) {
			t.Errorf("config.Linter %q and typescript.Linter %q disagree for %s; "+
				"scan.go's string conversion would silently fall back to ESLint",
				tc.cfg, tc.ts, tc.name)
		}
	}
}

// TestBiomeLinterSurvivesConversion is the same guard stated as behavior: the
// value a repo writes in .bulwark.yml must still select Biome after crossing the
// package boundary.
func TestBiomeLinterSurvivesConversion(t *testing.T) {
	if got := typescript.Linter(config.LinterBiome); got != typescript.Biome {
		t.Errorf("typescript.Linter(config.LinterBiome) = %q, want %q — opting into Biome would silently run ESLint", got, typescript.Biome)
	}
}
