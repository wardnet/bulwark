package typescript

import (
	"strings"
	"testing"
)

// TestEslintConfigIgnoresMatchesDefaultSkipDirs guards against regressing the
// incident that prompted this test: the bundled ESLint config lacked ignores
// for several of detect.go's defaultSkipDirs, so it linted a minified
// production bundle under a nested dist/ directory.
func TestEslintConfigIgnoresMatchesDefaultSkipDirs(t *testing.T) {
	want := []string{
		"**/node_modules/**",
		"**/dist/**",
		"**/build/**",
		"**/target/**",
		"**/vendor/**",
		"**/.git/**",
		"**/.bare/**",
		"**/.next/**",
		"**/coverage/**",
	}
	for _, entry := range want {
		if !strings.Contains(string(eslintConfig), `"`+entry+`"`) {
			t.Errorf("eslint.config.mjs ignores missing %q", entry)
		}
	}
}

// TestEslintConfigLintsTypeScript guards the incident this fixed: with no
// `files:` key, ESLint's flat-config default applies and only .js/.mjs/.cjs are
// linted — so every .ts/.tsx file in a TypeScript project was silently skipped
// and the check still reported PASS. The parser matters just as much: espree
// cannot read a type annotation, so the glob alone would only trade silence for
// a parse error on every file.
func TestEslintConfigLintsTypeScript(t *testing.T) {
	cfg := string(eslintConfig)
	for _, ext := range []string{"ts", "tsx"} {
		if !strings.Contains(cfg, ext) {
			t.Errorf("eslint.config.mjs has no files: entry covering .%s — TypeScript would be silently skipped", ext)
		}
	}
	if !strings.Contains(cfg, "files:") {
		t.Error("eslint.config.mjs has no files: key; ESLint then defaults to .js/.mjs/.cjs only")
	}
	if !strings.Contains(cfg, "@typescript-eslint/parser") {
		t.Error("eslint.config.mjs does not register the TypeScript parser; espree cannot parse .ts")
	}
	if !strings.Contains(cfg, "parser:") {
		t.Error("eslint.config.mjs imports the TS parser but never sets languageOptions.parser")
	}
}

// TestReportableIgnoresForeignRuleDiagnostics guards the second half of the
// same incident. bulwark lints with its own standalone config, so a project's
// `eslint-disable-next-line <its-own-plugin>/<rule>` comments reference rules we
// never loaded, and ESLint reports "Definition for rule ... was not found".
// Failing on those would fail every project that suppresses one of its own lint
// rules anywhere — they are complaints about the config we imposed, not defects
// in the code.
func TestReportableIgnoresForeignRuleDiagnostics(t *testing.T) {
	cases := []struct {
		name string
		msg  eslintMessage
		want bool
	}{
		{
			name: "our own security finding",
			msg:  eslintMessage{RuleID: "security/detect-object-injection"},
			want: true,
		},
		{
			name: "parse error (fatal, no rule)",
			msg:  eslintMessage{Fatal: true, Message: "Parsing error: Unexpected token"},
			want: true,
		},
		{
			name: "unknown rule from the project's own plugin",
			msg:  eslintMessage{RuleID: "react-hooks/refs", Message: "Definition for rule 'react-hooks/refs' was not found."},
			want: false,
		},
		{
			name: "unused disable directive for a rule we do not run",
			msg:  eslintMessage{Message: "Unused eslint-disable directive (no problems were reported from 'no-console')."},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reportable(tc.msg); got != tc.want {
				t.Errorf("reportable(%+v) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

// TestEslintConfigKeepsInlineSuppressions is the constraint that rules out the
// tempting shortcut for the above: --no-inline-config would also silence
// legitimate `eslint-disable-next-line security/...` comments, which are exactly
// how a reviewed false positive is meant to be recorded.
func TestEslintConfigKeepsInlineSuppressions(t *testing.T) {
	if strings.Contains(string(eslintConfig), "noInlineConfig") {
		t.Error("noInlineConfig would void legitimate security/* line-level suppressions")
	}
}
