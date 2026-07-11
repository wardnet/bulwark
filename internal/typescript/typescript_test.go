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
