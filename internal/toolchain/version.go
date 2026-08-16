package toolchain

import (
	"strings"

	"golang.org/x/mod/semver"
)

// canonical turns the many ways the three ecosystems spell a version into the
// "vMAJOR[.MINOR[.PATCH]]" form golang.org/x/mod/semver understands, or ""
// when the input names no comparable version at all.
//
// It has to be lenient because every source spells it differently and none of
// them is semver: go.mod says `go 1.26.4` and `toolchain go1.26.5`,
// rust-toolchain.toml says `1.96` or `stable`, .nvmrc says `22`, `v22.11.0` or
// `lts/*`, and `node --version` prints `v22.21.1`. A partial version is left
// partial rather than zero-padded — semver.Compare already orders "v1.26"
// before "v1.26.5", which is exactly right for a minimum ("1.26" is satisfied
// by 1.26.5) and would be wrong if padded to "v1.26.0" for an exact match.
//
// Returning "" for a non-numeric channel ("stable", "nightly", "lts/*") is
// deliberate and load-bearing: those name a moving target, not a floor, so
// there is nothing to compare an ambient toolchain against. Callers treat an
// empty requirement as "any working toolchain of this kind will do" rather
// than guessing a number — see Requirement.Unpinned.
func canonical(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "go") // `toolchain go1.26.5`, `go1.26.5`
	s = strings.TrimPrefix(s, "v")
	// Drop a channel/pre-release suffix rustup and Go both allow:
	// "1.96.0-beta.1", "1.26rc1", "1.85.0-x86_64-unknown-linux-gnu".
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "rc"); i > 0 {
		s = s[:i]
	}
	if s == "" {
		return ""
	}
	v := "v" + s
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

// olderThan reports whether have is a strictly lower version than want. Both
// are canonical() output. An unparseable or absent `have` is treated as older
// (provision rather than trust something unidentifiable); an empty `want` is
// no constraint at all, so nothing is older than it.
func olderThan(have, want string) bool {
	if want == "" {
		return false
	}
	if have == "" {
		return true
	}
	return semver.Compare(have, want) < 0
}

// maxVersion returns whichever canonical version is higher, preferring a
// non-empty one. Used to reduce several manifests — one go.mod per module,
// one rust-toolchain.toml per crate — to the single toolchain that has to
// serve all of them. Taking the max rather than the first found is what makes
// a monorepo whose modules declare different versions work: the newer
// language version can build the older module, never the reverse. AGENTS.md
// already records this rule for wardnet's two Go modules ("if they ever
// diverge this must track the newer of the two").
func maxVersion(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case semver.Compare(a, b) < 0:
		return b
	default:
		return a
	}
}

// minimumOfRange extracts the lowest version a node `engines` range admits.
//
// bulwark deliberately does not implement npm's full range grammar. The only
// question it ever asks of a range is "is the toolchain already on this
// machine too old", and that needs a floor, not set membership. So this reads
// the first version-like token and treats it as that floor, which is correct
// for every form that actually appears in an engines field (">=20",
// "^22.0.0", "~22.11", "20.x", "22 || 24", ">=20 <23") and conservative for
// anything exotic: a floor that is too low merely accepts an ambient
// toolchain that npm might reject, which is the same outcome as today's
// unconditional trust in whatever is on PATH.
//
// A leading "<" or "<=" is the one form where the first token is a ceiling
// rather than a floor, so it yields no requirement instead of a wrong one.
func minimumOfRange(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "*" {
		return ""
	}
	if strings.HasPrefix(spec, "<") {
		return ""
	}
	// Take the first version-like token. Splitting on whitespace, "||" and
	// "," and then stripping comparator characters handles both the glued
	// (">=20") and spaced (">= 20") spellings, which are equally common and
	// which a single strip of the whole string would not: the comparator can
	// be its own token.
	for _, field := range strings.FieldsFunc(spec, func(r rune) bool {
		return r == ' ' || r == '|' || r == ','
	}) {
		token := strings.TrimLeft(field, "><=^~ ")
		// "20.x" / "20.*" — semver rejects the wildcard, and the floor is the
		// version with the wildcard component dropped.
		if i := strings.IndexAny(token, "xX*"); i >= 0 {
			token = strings.TrimRight(token[:i], ".")
		}
		if v := canonical(token); v != "" {
			return v
		}
	}
	return ""
}
