package toolchain

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"wardnet/bulwark/internal/detect"
)

// Requirement is the language toolchain one detected ecosystem needs, read
// from what the repository already declares.
//
// Version is deliberately not sourced from .bulwark.yml. Every one of these
// files is already the authoritative, tool-enforced statement of the version
// — `go build` honours go.mod, rustup honours rust-toolchain.toml, npm
// honours engines.node — so a second copy in bulwark's config could only ever
// agree redundantly or disagree silently. A stale duplicate is worse than no
// duplicate: it reads as authoritative. .bulwark.yml can override (see
// config.Toolchain), which is a different thing — an explicit, local
// exception rather than a parallel source of truth.
type Requirement struct {
	Ecosystem detect.Ecosystem
	// Version is canonical() output ("v1.26.4"), or "" when the repo pins no
	// comparable version — a `stable` rust channel, an `lts/*` .nvmrc, or no
	// manifest statement at all.
	Version string
	// Raw is what the manifest literally said, for messages. Reporting
	// "1.26.4" rather than "v1.26.4" keeps bulwark's output greppable against
	// the file the reader will go look at.
	Raw string
	// Source names the file and field the version came from, so a surprising
	// requirement can be traced without guessing which of several manifests
	// won.
	Source string
	// Overridden records that .bulwark.yml supplied the version rather than a
	// manifest. It matters for Rust: rustup selects a toolchain by reading
	// rust-toolchain.toml from the directory cargo runs in, so a version that
	// exists only in bulwark's config is one rustup cannot see and has to be
	// told about explicitly.
	Overridden bool
}

// Unpinned reports whether the repo stated no comparable version, in which
// case any working toolchain of this kind satisfies it and bulwark provisions
// only if none is present at all.
func (r Requirement) Unpinned() bool { return r.Version == "" }

// Requirements resolves what each detected ecosystem needs. Ecosystems the
// caller didn't detect are skipped entirely, so a Go-only repo never reads a
// package.json and never provisions Node.
//
// A manifest that can't be read or parsed yields an unpinned requirement
// rather than an error: bulwark's job here is to make the toolchain more
// likely to be right, and refusing to scan a repo because its .nvmrc is
// malformed would be a worse outcome than scanning it with whatever is on
// PATH — exactly today's behavior.
func Requirements(root string, ecosystems []detect.Ecosystem, cfg Overrides) ([]Requirement, error) {
	var out []Requirement
	for _, e := range ecosystems {
		var req Requirement
		var err error
		switch e {
		case detect.Go:
			req, err = goRequirement(root, cfg.GoExclude)
		case detect.Rust:
			req, err = rustRequirement(root, cfg.RustExclude)
		case detect.TypeScript:
			req, err = nodeRequirement(root, cfg.TSExclude)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		if override := cfg.For(e); override != "" {
			v := canonical(override)
			// A version bulwark cannot parse is a hard config error, not a
			// silent downgrade. Assigning it unconditionally would set
			// Version to "" and discard what the manifest correctly said —
			// turning a typo like "1.26.x" into "any toolchain will do", and
			// then reporting "no version declared" about a repo whose go.mod
			// declares one. That reads as bulwark being broken rather than as
			// the config being wrong.
			//
			// Rust is exempt: rustup channels are legitimately non-numeric
			// ("stable", "nightly", "1.96-x86_64-unknown-linux-gnu"), and
			// rustup is the authority on which of those are real, not
			// bulwark. Such an override stays unpinned and is handed through
			// verbatim.
			if v == "" && e != detect.Rust {
				return nil, fmt.Errorf(
					"toolchain.%s in .bulwark.yml: %q is not a version bulwark can compare against an installed toolchain",
					overrideKey(e), override)
			}
			req.Version = v
			req.Raw = override
			req.Source = "toolchain." + overrideKey(e) + " in .bulwark.yml"
			req.Overridden = true
		}
		out = append(out, req)
	}
	return out, nil
}

// overrideKey names an ecosystem as it is spelled under `toolchain:` in
// .bulwark.yml. TypeScript's key is `node`, because what is being overridden
// is the Node runtime, not the TypeScript compiler — the ecosystem's name and
// its toolchain's name are the one place these diverge.
func overrideKey(e detect.Ecosystem) string {
	if e == detect.TypeScript {
		return "node"
	}
	return string(e)
}

// goRequirement reads the `go` and `toolchain` directives from every module
// under root and returns the highest.
//
// Both directives matter and they mean different things: `go` is the minimum
// language version the module's source requires, `toolchain` names a specific
// toolchain to run. Taking the max of the two across all modules gives the one
// toolchain that can build every module in the tree.
//
// Reading every module rather than a single go.mod at the root is the whole
// point. gt's reverted `setup-go` step looked in exactly one location, which
// would have found nothing at all in wardnet, whose modules live under
// `wctl/` and `sdk/wardnet-go/` rather than at the scan root.
func goRequirement(root string, exclude []string) (Requirement, error) {
	req := Requirement{Ecosystem: detect.Go}
	dirs, err := detect.GoModuleDirs(root, exclude)
	if err != nil {
		return req, err
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, "go.mod")
		data, err := os.ReadFile(path) // #nosec G304 -- path comes from bulwark's own module-discovery walk, not user input
		if err != nil {
			continue
		}
		// modfile is golang.org/x/mod's own parser, already a direct
		// dependency (cmd/bulwark/update.go uses x/mod/semver). Hand-rolling
		// a line sniff here — as detect.isWorkspaceRoot does for Cargo.toml —
		// would be reimplementing a parser that ships in the module graph and
		// that Go itself uses.
		f, err := modfile.Parse(path, data, nil)
		if err != nil {
			continue
		}
		for _, raw := range []string{goDirective(f), toolchainDirective(f)} {
			if v := canonical(raw); maxVersion(req.Version, v) != req.Version {
				req.Version, req.Raw = v, strings.TrimPrefix(raw, "go")
				rel, relErr := filepath.Rel(root, dir)
				if relErr != nil || rel == "." {
					rel = ""
				} else {
					rel += "/"
				}
				req.Source = rel + "go.mod"
			}
		}
	}
	return req, nil
}

func goDirective(f *modfile.File) string {
	if f.Go == nil {
		return ""
	}
	return f.Go.Version
}

func toolchainDirective(f *modfile.File) string {
	if f.Toolchain == nil {
		return ""
	}
	return f.Toolchain.Name
}

// rustRequirement reads the channel from each discovered crate/workspace
// root's rust-toolchain.toml (or the legacy bare rust-toolchain file) and
// returns the highest pinned one.
//
// This extends, rather than contradicts, internal/rust's existing stance that
// "clippy/fmt's own toolchain version is the target repo's responsibility via
// its own rust-toolchain.toml". It still is — bulwark reads that file rather
// than overriding it, and only makes sure the channel it names is actually
// installed instead of assuming rustup will lazily fetch it mid-check.
func rustRequirement(root string, exclude []string) (Requirement, error) {
	req := Requirement{Ecosystem: detect.Rust}
	dirs, err := detect.RustCrateDirs(root, exclude)
	if err != nil {
		return req, err
	}
	for _, dir := range dirs {
		raw, source := rustChannel(root, dir)
		if raw == "" {
			continue
		}
		v := canonical(raw)
		// A named channel ("stable", "nightly") canonicalises to "" — record
		// it as the raw requirement so messages can name it, but leave
		// Version empty so it compares as unpinned.
		if req.Version == "" && req.Raw == "" {
			req.Raw, req.Source = raw, source
		}
		if maxVersion(req.Version, v) != req.Version {
			req.Version, req.Raw, req.Source = v, raw, source
		}
	}
	return req, nil
}

// rustChannel returns the channel string declared for one crate directory.
// rust-toolchain.toml wins over the legacy bare rust-toolchain file, matching
// rustup's own precedence.
func rustChannel(root, dir string) (channel, source string) {
	rel := func(name string) string {
		r, err := filepath.Rel(root, filepath.Join(dir, name))
		if err != nil {
			return name
		}
		return r
	}
	if data, err := os.ReadFile(filepath.Join(dir, "rust-toolchain.toml")); err == nil { // #nosec G304 -- dir comes from bulwark's own crate-discovery walk, not user input
		if c := tomlChannel(string(data)); c != "" {
			return c, rel("rust-toolchain.toml")
		}
	}
	// The legacy form is the whole file: a bare channel string, no TOML.
	if data, err := os.ReadFile(filepath.Join(dir, "rust-toolchain")); err == nil { // #nosec G304 -- dir comes from bulwark's own crate-discovery walk, not user input
		if c := strings.TrimSpace(string(data)); c != "" && !strings.Contains(c, "\n") {
			return c, rel("rust-toolchain")
		}
	}
	return "", ""
}

// tomlChannel pulls `channel = "1.96"` out of a rust-toolchain.toml.
//
// A line-level sniff, not a TOML parse, and deliberately so — this mirrors
// detect.isWorkspaceRoot's treatment of Cargo.toml. The file's entire schema
// is a single [toolchain] table, `channel` is the only key bulwark reads, and
// adding a TOML dependency to read one string would be the larger cost. A
// file too exotic for this yields no channel, which degrades to unpinned
// rather than to a wrong answer.
func tomlChannel(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if before, _, found := strings.Cut(line, "#"); found {
			line = strings.TrimSpace(before)
		}
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != "channel" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// packageJSONEngines is the subset of package.json this package reads.
type packageJSONEngines struct {
	Engines struct {
		Node string `json:"node"`
	} `json:"engines"`
}

// nodeRequirement reads the Node version from each detected TypeScript
// package's `engines.node`, falling back to a .nvmrc, and returns the highest
// floor any of them asks for.
//
// engines.node is a range rather than a version, so only its lower bound is
// meaningful here — see minimumOfRange for why a floor is all this needs.
// .nvmrc is checked at both the package directory and the scan root, since a
// monorepo conventionally keeps one .nvmrc at the top rather than one per
// package.
func nodeRequirement(root string, exclude []string) (Requirement, error) {
	req := Requirement{Ecosystem: detect.TypeScript}
	dirs, err := detect.TSPackageDirs(root, exclude)
	if err != nil {
		return req, err
	}
	consider := func(raw, source string) {
		if raw == "" {
			return
		}
		v := minimumOfRange(raw)
		if req.Raw == "" {
			req.Raw, req.Source = raw, source
		}
		if maxVersion(req.Version, v) != req.Version {
			req.Version, req.Raw, req.Source = v, raw, source
		}
	}
	relTo := func(dir, name string) string {
		r, err := filepath.Rel(root, filepath.Join(dir, name))
		if err != nil {
			return name
		}
		return r
	}
	for _, dir := range dirs {
		if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil { // #nosec G304 -- dir comes from bulwark's own package-discovery walk, not user input
			var pkg packageJSONEngines
			if json.Unmarshal(data, &pkg) == nil {
				consider(pkg.Engines.Node, relTo(dir, "package.json")+" (engines.node)")
			}
		}
		consider(nvmrc(dir), relTo(dir, ".nvmrc"))
	}
	consider(nvmrc(root), ".nvmrc")
	return req, nil
}

// nvmrc reads a .nvmrc, whose entire content is the version or alias.
func nvmrc(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ".nvmrc")) // #nosec G304 -- dir comes from bulwark's own package-discovery walk, not user input
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
