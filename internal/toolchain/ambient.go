package toolchain

import (
	"context"
	"os/exec"
	"strings"

	"wardnet/bulwark/internal/detect"
)

// probe is how one ecosystem's already-installed language toolchain is found
// and identified. Split out as a struct so the decision logic in Ensure is
// one code path across all three, and so tests can substitute a probe without
// having a real toolchain on the machine.
type probe struct {
	// bin is the command whose presence means "this toolchain is installed".
	bin string
	// versionArgs asks that command to identify itself.
	versionArgs []string
	// parse extracts a canonical version from the command's output.
	parse func(output string) string
}

var probes = map[detect.Ecosystem]probe{
	detect.Go: {
		bin:         "go",
		versionArgs: []string{"version"},
		// "go version go1.26.5 linux/amd64"
		parse: func(out string) string { return fieldAfter(out, "version") },
	},
	detect.Rust: {
		// cargo, not rustc: every Rust check and the coverage path invoke
		// cargo, and a rustup install can in principle have one without the
		// other. Asking the binary bulwark actually runs is the honest probe.
		bin:         "cargo",
		versionArgs: []string{"--version"},
		// "cargo 1.96.0 (abcdef123 2025-01-01)"
		parse: func(out string) string { return nthField(out, 1) },
	},
	detect.TypeScript: {
		bin:         "node",
		versionArgs: []string{"--version"},
		// "v22.21.1"
		parse: func(out string) string { return nthField(out, 0) },
	},
}

// installed reports the canonical version of the ambient toolchain for an
// ecosystem, and whether one is present at all.
//
// "Present but unidentifiable" is reported as present with an empty version,
// which olderThan then treats as older than any requirement. That is the
// conservative reading: a toolchain that won't say what it is can't be shown
// to satisfy a pin.
func installed(ctx context.Context, p probe) (version string, present bool) {
	if _, err := exec.LookPath(p.bin); err != nil {
		return "", false
	}
	// Deliberately not executil.Run: that streams the child's output to the
	// terminal, and `go version` chatter ahead of every scan is noise. This
	// is a probe, not a check whose output the user wants.
	out, err := exec.CommandContext(ctx, p.bin, p.versionArgs...).Output() // #nosec G204 -- p.bin/versionArgs come from this file's own static probes table, not user input
	if err != nil {
		return "", true
	}
	return p.parse(string(out)), true
}

// fieldAfter returns the whitespace-separated field following the first
// occurrence of key, canonicalised.
func fieldAfter(out, key string) string {
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == key && i+1 < len(fields) {
			return canonical(fields[i+1])
		}
	}
	return ""
}

// nthField returns the nth whitespace-separated field, canonicalised.
func nthField(out string, n int) string {
	fields := strings.Fields(out)
	if n >= len(fields) {
		return ""
	}
	return canonical(fields[n])
}
