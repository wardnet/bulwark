package golang

import (
	"os"
	"strings"
	"testing"
)

// TestPinnedVersionsMatchGoPinModule is the drift guard for the one pair of
// pins bulwark cannot read from its manifest at runtime.
//
// Every other pinned tool (Biome, cargo-audit, cargo-deny, Semgrep) has
// its version read directly out of the package-manager manifest Dependabot
// edits, so drift is impossible by construction. Go's two can't work that way:
// gosecPkg/govulncheckPkg are const expressions that concatenate the version at
// compile time, and go-pin is a separate module whose go.mod //go:embed cannot
// reach. So the constants stay, and this test is what makes a Dependabot bump
// to go-pin/go.mod fail loudly instead of silently leaving bulwark installing
// the old version.
//
// If this test fails, a bump landed in internal/golang/go-pin/go.mod that was
// not mirrored into the constants in golang.go. Update them to match.
func TestPinnedVersionsMatchGoPinModule(t *testing.T) {
	data, err := os.ReadFile("go-pin/go.mod")
	if err != nil {
		t.Fatalf("reading go-pin/go.mod: %v", err)
	}

	for _, tc := range []struct {
		module   string
		constant string
		name     string
	}{
		{"github.com/securego/gosec/v2", gosecVersion, "gosecVersion"},
		{"golang.org/x/vuln", govulncheckVersion, "govulncheckVersion"},
	} {
		got, ok := moduleVersion(string(data), tc.module)
		if !ok {
			t.Errorf("go-pin/go.mod has no require entry for %s", tc.module)
			continue
		}
		if got != tc.constant {
			t.Errorf("%s = %q but go-pin/go.mod pins %s at %q\n"+
				"A Dependabot bump landed in go-pin/go.mod without updating golang.go — "+
				"set %s to %q.", tc.name, tc.constant, tc.module, got, tc.name, got)
		}
	}
}

// moduleVersion finds the version a go.mod requires a module at. It matches the
// module path exactly so that golang.org/x/vuln is never satisfied by a line
// for golang.org/x/vulndb.
func moduleVersion(gomod, module string) (string, bool) {
	for line := range strings.Lines(gomod) {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == module {
			return fields[1], true
		}
	}
	return "", false
}
