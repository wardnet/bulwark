package semgrep

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed requirements.txt
var requirements []byte

// version is read from requirements.txt rather than written here, so that
// Dependabot has a manifest it understands and there is only one place the
// version can be wrong. A pinned security tool that nothing ever ages out is a
// scanner that quietly goes stale while still reporting [PASS].
var version = mustPinnedVersion()

// pinnedVersion extracts `semgrep==x.y.z` from the embedded requirements file.
// An unparseable file fails loudly rather than yielding "", which would become
// `pipx install semgrep==` — a confusing failure far from its cause.
func pinnedVersion(reqs []byte) (string, error) {
	for line := range strings.Lines(string(reqs)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, v, ok := strings.Cut(line, "==")
		if !ok || strings.TrimSpace(name) != "semgrep" {
			continue
		}
		v = strings.TrimSpace(v)
		// Strip a trailing inline comment; `semgrep==1.168.0  # note` would
		// otherwise reach `pipx install` verbatim, a corrupt value where the doc
		// above promises a loud failure.
		if i := strings.Index(v, "#"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		// Reject anything after the version. Unlike the Cargo parser, whose
		// quoted value delimits it, a requirements line has no terminator: an
		// environment marker (`; python_version >= "3.9"`) or a Dependabot-added
		// `--hash=sha256:...` would otherwise be handed to pipx verbatim as part
		// of the version, which is the corrupt value this parser promises never
		// to yield.
		if strings.ContainsAny(v, " \t;") {
			return "", fmt.Errorf("requirements.txt: unsupported trailing content after the semgrep version: %q", v)
		}
		if v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("requirements.txt: no pinned `semgrep==<version>` entry")
}

func mustPinnedVersion() string {
	v, err := pinnedVersion(requirements)
	if err != nil {
		panic(err)
	}
	return v
}
