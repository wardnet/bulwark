package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wardnet/bulwark/internal/detect"
)

func write(t *testing.T, dir, rel, contents string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireOne(t *testing.T, root string, e detect.Ecosystem, ov Overrides) Requirement {
	t.Helper()
	reqs, err := Requirements(root, []detect.Ecosystem{e}, ov)
	if err != nil {
		t.Fatalf("Requirements: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("got %d requirements, want 1", len(reqs))
	}
	return reqs[0]
}

func TestGoRequirementReadsGoDirective(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module x\n\ngo 1.26.4\n")

	got := requireOne(t, dir, detect.Go, Overrides{})
	if got.Version != "v1.26.4" {
		t.Errorf("Version = %q, want v1.26.4", got.Version)
	}
	if got.Source != "go.mod" {
		t.Errorf("Source = %q, want go.mod", got.Source)
	}
}

// The toolchain directive names a specific toolchain and is normally higher
// than the `go` directive's language minimum; the higher of the two is what a
// build actually needs.
func TestGoRequirementPrefersHigherToolchainDirective(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module x\n\ngo 1.26.4\n\ntoolchain go1.26.5\n")

	if got := requireOne(t, dir, detect.Go, Overrides{}).Version; got != "v1.26.5" {
		t.Errorf("Version = %q, want v1.26.5 (the toolchain directive)", got)
	}
}

// The regression gt's reverted setup-go step would have shipped: looking for
// go.mod at exactly one path finds nothing in a monorepo whose modules live in
// subdirectories, and the toolchain silently stays whatever the image had.
// Every module is read, and the highest version wins — the newer toolchain can
// build the older module, never the reverse.
func TestGoRequirementSpansEveryModuleAndTakesTheHighest(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "README.md", "no go.mod at the scan root")
	write(t, filepath.Join(dir, "wctl"), "go.mod", "module a\n\ngo 1.25.0\n")
	write(t, filepath.Join(dir, "sdk", "wardnet-go"), "go.mod", "module b\n\ngo 1.26.4\n")

	got := requireOne(t, dir, detect.Go, Overrides{})
	if got.Version != "v1.26.4" {
		t.Errorf("Version = %q, want v1.26.4 (the newer of the two modules)", got.Version)
	}
	if got.Source != filepath.Join("sdk", "wardnet-go")+"/go.mod" {
		t.Errorf("Source = %q, want it to name the module the version came from", got.Source)
	}
}

func TestGoRequirementUnpinnedWithoutDirective(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module x\n")

	if got := requireOne(t, dir, detect.Go, Overrides{}); !got.Unpinned() {
		t.Errorf("a go.mod with no go directive should be unpinned, got %+v", got)
	}
}

// A malformed manifest degrades to unpinned rather than failing the scan:
// bulwark's job is to make the toolchain more likely to be right, and
// refusing to scan over a broken go.mod is worse than scanning with PATH.
func TestGoRequirementToleratesUnparseableGoMod(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "this is not a go.mod at all {{{\n")

	got := requireOne(t, dir, detect.Go, Overrides{})
	if !got.Unpinned() {
		t.Errorf("an unparseable go.mod should be unpinned, got %+v", got)
	}
}

func TestRustRequirementReadsToml(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Cargo.toml", "[package]\nname = \"x\"\n")
	write(t, dir, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.96\"\ncomponents = [\"clippy\"]\n")

	got := requireOne(t, dir, detect.Rust, Overrides{})
	if got.Version != "v1.96" {
		t.Errorf("Version = %q, want v1.96", got.Version)
	}
	if got.Raw != "1.96" {
		t.Errorf("Raw = %q, want the literal channel 1.96", got.Raw)
	}
}

func TestRustRequirementReadsLegacyBareFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Cargo.toml", "[package]\nname = \"x\"\n")
	write(t, dir, "rust-toolchain", "1.94.0\n")

	if got := requireOne(t, dir, detect.Rust, Overrides{}).Version; got != "v1.94.0" {
		t.Errorf("Version = %q, want v1.94.0", got)
	}
}

// rustup itself prefers rust-toolchain.toml over the legacy file; bulwark
// must agree, or it would report a version rustup will not actually use.
func TestRustRequirementTomlWinsOverLegacyFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Cargo.toml", "[package]\nname = \"x\"\n")
	write(t, dir, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.96\"\n")
	write(t, dir, "rust-toolchain", "1.80.0\n")

	if got := requireOne(t, dir, detect.Rust, Overrides{}).Version; got != "v1.96" {
		t.Errorf("Version = %q, want v1.96 from rust-toolchain.toml", got)
	}
}

// A named channel is unpinned — there is no number to compare an ambient
// toolchain against — but the raw word is kept so it can be handed to rustup,
// which does understand it.
func TestRustRequirementNamedChannelIsUnpinnedButRemembered(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Cargo.toml", "[package]\nname = \"x\"\n")
	write(t, dir, "rust-toolchain.toml", "[toolchain]\nchannel = \"stable\"\n")

	got := requireOne(t, dir, detect.Rust, Overrides{})
	if !got.Unpinned() {
		t.Errorf("a stable channel should be unpinned, got %+v", got)
	}
	if got.Raw != "stable" {
		t.Errorf("Raw = %q, want stable so rustup can be handed it verbatim", got.Raw)
	}
}

func TestTomlChannelIgnoresComments(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"[toolchain]\nchannel = \"1.96\"\n", "1.96"},
		{"[toolchain]\n# channel = \"9.99\"\nchannel = \"1.96\"\n", "1.96"},
		{"channel = '1.96' # pinned\n", "1.96"},
		{"[toolchain]\nprofile = \"minimal\"\n", ""},
		{"", ""},
	} {
		if got := tomlChannel(tc.in); got != tc.want {
			t.Errorf("tomlChannel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNodeRequirementReadsEngines(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"x","engines":{"node":">=22.11.0"}}`)

	got := requireOne(t, dir, detect.TypeScript, Overrides{})
	if got.Version != "v22.11.0" {
		t.Errorf("Version = %q, want v22.11.0", got.Version)
	}
	if !strings.Contains(got.Source, "engines.node") {
		t.Errorf("Source = %q, want it to name engines.node", got.Source)
	}
}

func TestNodeRequirementFallsBackToNvmrc(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"x"}`)
	write(t, dir, ".nvmrc", "22.11.0\n")

	if got := requireOne(t, dir, detect.TypeScript, Overrides{}).Version; got != "v22.11.0" {
		t.Errorf("Version = %q, want v22.11.0 from .nvmrc", got)
	}
}

// A monorepo conventionally keeps one .nvmrc at the top rather than one per
// package, so the scan root is consulted even when no package declares
// anything.
func TestNodeRequirementUsesRootNvmrcForNestedPackages(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".nvmrc", "v22.11.0\n")
	write(t, filepath.Join(dir, "web"), "package.json", `{"name":"web"}`)

	if got := requireOne(t, dir, detect.TypeScript, Overrides{}).Version; got != "v22.11.0" {
		t.Errorf("Version = %q, want v22.11.0 from the root .nvmrc", got)
	}
}

// One Node runtime has to serve every package, so the highest floor any of
// them declares is the one that must be installed.
func TestNodeRequirementTakesHighestAcrossPackages(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a"), "package.json", `{"name":"a","engines":{"node":">=20"}}`)
	write(t, filepath.Join(dir, "b"), "package.json", `{"name":"b","engines":{"node":">=22"}}`)

	if got := requireOne(t, dir, detect.TypeScript, Overrides{}).Version; got != "v22" {
		t.Errorf("Version = %q, want v22 (the highest floor)", got)
	}
}

func TestOverrideBeatsManifest(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module x\n\ngo 1.26.4\n")

	got := requireOne(t, dir, detect.Go, Overrides{Go: "1.27.0"})
	if got.Version != "v1.27.0" {
		t.Errorf("Version = %q, want the override v1.27.0", got.Version)
	}
	if !strings.Contains(got.Source, ".bulwark.yml") {
		t.Errorf("Source = %q, want it to say the value came from config, not the manifest", got.Source)
	}
}

// A typo'd override must be a loud config error, never a silent downgrade.
// Assigning an unparseable value would blank Version and discard what the
// manifest correctly said — turning "1.26.x" into "any toolchain will do",
// and then reporting "no version declared" about a repo whose go.mod declares
// one. That reads as bulwark being broken rather than the config being wrong.
func TestBadOverrideIsAnErrorNotASilentDowngrade(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module x\n\ngo 1.26.4\n")

	_, err := Requirements(dir, []detect.Ecosystem{detect.Go}, Overrides{Go: "1.26.x"})
	if err == nil {
		t.Fatal("an unparseable toolchain.go override was accepted")
	}
	if !strings.Contains(err.Error(), "toolchain.go") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

// TypeScript's override key is `node`, since what is overridden is the Node
// runtime rather than the TypeScript compiler. The error has to say the key
// the user would actually go and edit.
func TestBadNodeOverrideNamesTheNodeKey(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"x"}`)

	_, err := Requirements(dir, []detect.Ecosystem{detect.TypeScript}, Overrides{Node: "lts/*"})
	if err == nil {
		t.Fatal("an unparseable toolchain.node override was accepted")
	}
	if !strings.Contains(err.Error(), "toolchain.node") {
		t.Errorf("error should name toolchain.node, got: %v", err)
	}
}

// Rust is exempt: rustup channels are legitimately non-numeric, and rustup —
// not bulwark — is the authority on which names are real. Such an override
// stays unpinned and is passed through verbatim.
func TestRustOverrideAcceptsANamedChannel(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Cargo.toml", "[package]\nname = \"x\"\n")

	got := requireOne(t, dir, detect.Rust, Overrides{Rust: "nightly"})
	if !got.Unpinned() {
		t.Errorf("a nightly override should be unpinned, got %+v", got)
	}
	if got.Raw != "nightly" {
		t.Errorf("Raw = %q, want nightly passed through for rustup", got.Raw)
	}
	if !got.Overridden {
		t.Error("an override must be marked Overridden so rustup can be told about it explicitly")
	}
}

// A version read from a manifest is not Overridden: rustup finds
// rust-toolchain.toml by itself, and forcing RUSTUP_TOOLCHAIN in that case
// would override rustup's own per-crate selection.
func TestManifestSourcedRequirementIsNotMarkedOverridden(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Cargo.toml", "[package]\nname = \"x\"\n")
	write(t, dir, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.96\"\n")

	if got := requireOne(t, dir, detect.Rust, Overrides{}); got.Overridden {
		t.Error("a manifest-sourced requirement must not be marked Overridden")
	}
}

// Requirements is scoped to what the caller detected: a Go-only repo must
// never read a package.json or provision Node.
func TestRequirementsOnlyCoversRequestedEcosystems(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module x\n\ngo 1.26.4\n")
	write(t, dir, "package.json", `{"name":"x","engines":{"node":">=22"}}`)

	reqs, err := Requirements(dir, []detect.Ecosystem{detect.Go}, Overrides{})
	if err != nil {
		t.Fatalf("Requirements: %v", err)
	}
	if len(reqs) != 1 || reqs[0].Ecosystem != detect.Go {
		t.Fatalf("got %+v, want only the go requirement", reqs)
	}
}
