package typescript

import (
	"encoding/json"
	"strings"
	"testing"
)

// biomeCfg decodes the embedded config so assertions read against structure
// rather than substrings — a `"security": "error"` that landed in the wrong
// object would satisfy strings.Contains and gate on nothing.
func biomeCfg(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(biomeConfig, &m); err != nil {
		t.Fatalf("biome.json is not valid JSON: %v", err)
	}
	return m
}

// TestBiomeConfigEnablesOnlyTheGatedGroups pins the contract the linter choice
// promises: security and correctness gate, and nothing else does. Without
// recommended:false, Biome's default preset turns on style and suspicious too,
// and bulwark would start failing PRs over formatting-adjacent opinions it never
// agreed to enforce.
func TestBiomeConfigEnablesOnlyTheGatedGroups(t *testing.T) {
	rules, ok := biomeCfg(t)["linter"].(map[string]any)["rules"].(map[string]any)
	if !ok {
		t.Fatal("biome.json has no linter.rules object")
	}
	// `preset: "none"` rather than the older `recommended: false`: Biome's docs
	// mark `recommended` deprecated in favour of `preset`, and Dependabot now
	// bumps @biomejs/biome weekly, so the forward-compatible spelling is the one
	// to be pinned to. Verified byte-identical output on a real fixture against
	// 2.5.8, whose PresetConfig enum is ["recommended", "all", "none"].
	if rules["preset"] != "none" {
		t.Errorf(`linter.rules.preset = %v, want "none" — Biome's default preset would enable style/suspicious too`, rules["preset"])
	}
	if _, present := rules["recommended"]; present {
		t.Error("linter.rules.recommended is set; it is deprecated in favour of preset")
	}
	for _, group := range []string{"security", "correctness"} {
		if rules[group] != "error" {
			t.Errorf("linter.rules.%s = %v, want \"error\"", group, rules[group])
		}
	}
	for _, group := range []string{"style", "suspicious", "complexity", "a11y", "nursery", "performance"} {
		if _, present := rules[group]; present {
			t.Errorf("linter.rules.%s is set; bulwark gates on security and correctness only", group)
		}
	}
}

// TestBiomeConfigDisablesFormatterAndAssist guards against bulwark reporting a
// formatting diff as a security finding. Biome is a formatter as well as a
// linter, and both default to on.
func TestBiomeConfigDisablesFormatterAndAssist(t *testing.T) {
	cfg := biomeCfg(t)
	for _, key := range []string{"formatter", "assist"} {
		section, ok := cfg[key].(map[string]any)
		if !ok {
			t.Errorf("biome.json has no %s section; it defaults to enabled", key)
			continue
		}
		if section["enabled"] != false {
			t.Errorf("%s.enabled = %v, want false", key, section["enabled"])
		}
	}
}

// TestBiomeConfigIgnoresMatchesDefaultSkipDirs is the Biome counterpart of
// TestEslintConfigIgnoresMatchesDefaultSkipDirs, and guards a failure verified
// against Biome 2.5.8 directly: with these negations removed, Biome lints
// dist/ and reports findings inside a minified production bundle. The ignores
// are load-bearing, not decorative.
func TestBiomeConfigIgnoresMatchesDefaultSkipDirs(t *testing.T) {
	files, ok := biomeCfg(t)["files"].(map[string]any)
	if !ok {
		t.Fatal("biome.json has no files section")
	}
	includes, ok := files["includes"].([]any)
	if !ok {
		t.Fatal("biome.json has no files.includes list")
	}
	got := map[string]bool{}
	for _, p := range includes {
		got[p.(string)] = true
	}
	if !got["**"] {
		t.Error(`files.includes lacks "**"; the negations below only subtract from what it matches`)
	}
	for _, dir := range []string{"node_modules", "dist", "build", "target", "vendor", ".git", ".bare", ".next", "coverage"} {
		if !got["!**/"+dir] {
			t.Errorf("files.includes missing %q", "!**/"+dir)
		}
	}
}

// TestReportableBiomeGatesOnlyOnOurGroups guards the containment described on
// reportableBiome: a nested biome.json declaring "root": false is merged into
// bulwark's config by Biome, so the scanned project can inject its own rules
// into our run. Category filtering is the only thing that keeps those out of
// the verdict — and suppression/unused fires on the project's own biome-ignore
// comments for rules bulwark never enabled. The category is plural —
// `suppressions/unused` — verified against Biome 2.5.8, not guessed from docs.
func TestReportableBiomeGatesOnlyOnOurGroups(t *testing.T) {
	for _, category := range []string{
		"lint/security/noGlobalEval",
		"lint/security/noSecrets",
		"lint/correctness/noUnreachable",
	} {
		if !reportableBiome(category) {
			t.Errorf("reportableBiome(%q) = false, want true", category)
		}
	}
	// A file Biome cannot parse emits only `parse` diagnostics (category verified
	// against 2.5.8 with a deliberately broken .ts file). Filtering those out
	// leaves count == 0, which clears the error and prints "no findings" — a
	// package where nothing was linted would report as a clean pass. ESLint's
	// reportable() keeps its fatal diagnostics for exactly this reason.
	// Failure categories, none of which are rule opinions: a package where these
	// fire was not successfully linted, and filtering them out reports it as a
	// clean pass. internalError/io is the nastier one — Biome emits it with
	// summary.errors == 0, so the exit code does not give it away either.
	for _, category := range []string{"parse", "syntax", "internalError/io", "configuration", "deserialize"} {
		if !reportableBiome(category) {
			t.Errorf("reportableBiome(%q) = false; a package that was never linted would be reported as a pass", category)
		}
	}
	for _, category := range []string{
		"lint/style/useConst",
		"lint/suspicious/noExplicitAny",
		"lint/complexity/noForEach",
		"lint/nursery/someRule",
		"suppressions/unused",
		"format",
		"assist/source/organizeImports",
	} {
		if reportableBiome(category) {
			t.Errorf("reportableBiome(%q) = true, want false", category)
		}
	}
}

// TestBiomePinMatchesConfigSchema keeps the $schema URL in biome.json aligned
// with the pinned version. A stale URL is not a runtime failure — Biome does not
// fetch it — but it is what an editor validates against, so drift here silently
// makes every local edit to this file validate against the wrong schema.
func TestBiomePinMatchesConfigSchema(t *testing.T) {
	var pin struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(biomePackageJSON, &pin); err != nil {
		t.Fatalf("biome-pin/package.json: %v", err)
	}
	version := pin.Dependencies["@biomejs/biome"]
	if version == "" {
		t.Fatal("biome-pin/package.json does not pin @biomejs/biome")
	}
	schema, _ := biomeCfg(t)["$schema"].(string)
	if !strings.Contains(schema, version) {
		t.Errorf("biome.json $schema = %q but the pin is %s; update the schema URL", schema, version)
	}
}
