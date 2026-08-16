package toolchain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"wardnet/bulwark/internal/executil"
)

// goBootstrapMin is the first Go release that can fetch another toolchain for
// itself (the GOTOOLCHAIN mechanism, Go 1.21). At or above it, bulwark never
// downloads Go: it names the version it wants and lets the go command fetch
// it through the module proxy, verified against Go's checksum database. Below
// it — or with no Go at all — there is nothing to delegate to and bulwark
// downloads a tarball itself.
const goBootstrapMin = "v1.21"

// provisionGo makes the declared Go toolchain available.
//
// The common case does no downloading at all. Any Go 1.21+ on the machine can
// fetch a newer toolchain on demand, so bulwark just sets GOTOOLCHAIN to the
// version the repo declares and lets Go do it — the official mechanism, and
// better provenance than anything bulwark could hand-roll.
//
// Setting it explicitly also closes a real, documented gap. GOTOOLCHAIN's
// default (`auto`) consults go.mod only when the go command runs inside that
// module; `go install <external tool>@<version>`, which internal/golang runs
// outside any module to fetch gosec and govulncheck, does not consult it. So
// those tools got built with whatever Go the runner shipped, and a
// govulncheck built by an older Go rejects newer source outright — the exact
// failure AGENTS.md records against wardnet's CI, worked around there by
// pinning `go-version` in every workflow. An explicit GOTOOLCHAIN fixes it at
// the source instead of in each consumer's YAML.
func provisionGo(ctx context.Context, req Requirement, ambient string, present bool) (*step, error) {
	if req.Unpinned() {
		if present {
			return nil, nil
		}
		return nil, fmt.Errorf("no Go toolchain on PATH and no version declared in any go.mod to install one by")
	}
	want := toolchainName(req.Version)

	if present && !olderThan(ambient, goBootstrapMin) {
		return &step{
			vars: []string{"GOTOOLCHAIN=" + want},
			note: fmt.Sprintf("go: using %s via GOTOOLCHAIN (declared in %s; ambient go is %s)",
				want, req.Source, display(ambient)),
		}, nil
	}

	dir, err := cacheRoot("go-" + strings.TrimPrefix(req.Version, "v"))
	if err != nil {
		return nil, err
	}
	if err := installOnce(dir, func(staging string) error {
		return downloadGo(ctx, want, staging)
	}); err != nil {
		return nil, err
	}
	return &step{
		pathDirs: []string{filepath.Join(dir, "bin")},
		// The downloaded toolchain is exactly what was asked for, so pin
		// GOTOOLCHAIN to it rather than leaving `auto` free to fetch a third
		// one on first use.
		vars: []string{"GOTOOLCHAIN=" + want},
		note: fmt.Sprintf("go: installed %s (declared in %s; %s)", want, req.Source, absentOrOld(ambient, present)),
	}, nil
}

// toolchainName renders a canonical version as a Go toolchain name.
// Go toolchains are always named to the patch level ("go1.26.0", never
// "go1.26"), so a go.mod that says only `go 1.26` is resolved to that
// minor's initial release — the oldest toolchain that satisfies it, which is
// the right reading of a minimum.
func toolchainName(version string) string {
	v := strings.TrimPrefix(version, "v")
	if strings.Count(v, ".") < 2 {
		v += strings.Repeat(".0", 2-strings.Count(v, "."))
	}
	return "go" + v
}

// goRelease is the subset of https://go.dev/dl/?mode=json bulwark reads.
type goRelease struct {
	Version string `json:"version"`
	Files   []struct {
		Filename string `json:"filename"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		Kind     string `json:"kind"`
		SHA256   string `json:"sha256"`
	} `json:"files"`
}

// downloadGo fetches a Go toolchain tarball into staging, taking the expected
// SHA-256 from Go's own release index rather than from anything alongside the
// download.
func downloadGo(ctx context.Context, version, staging string) error {
	index, err := fetch(ctx, "https://go.dev/dl/?mode=json&include=all")
	if err != nil {
		return err
	}
	var releases []goRelease
	if err := json.Unmarshal(index, &releases); err != nil {
		return err
	}
	for _, rel := range releases {
		if rel.Version != version {
			continue
		}
		for _, f := range rel.Files {
			if f.OS != runtime.GOOS || f.Arch != runtime.GOARCH || f.Kind != "archive" {
				continue
			}
			data, err := downloadVerified(ctx, "https://go.dev/dl/"+f.Filename, f.SHA256)
			if err != nil {
				return err
			}
			return extractTarGz(data, staging)
		}
		return fmt.Errorf("go toolchain %s has no %s/%s archive", version, runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Errorf("go toolchain %s is not in the release index", version)
}

// provisionRust makes the declared Rust toolchain available.
//
// Rust is the one ecosystem with an official, universally-used version
// manager that already reads the same file bulwark does: given a
// rust-toolchain.toml, rustup installs and selects that channel by itself.
// internal/rust's package doc already leans on exactly that ("clippy/fmt's
// own toolchain version is the target repo's responsibility via its own
// rust-toolchain.toml — bulwark doesn't second-guess that"), so bulwark
// provisions *rustup* and lets rustup provision Rust, rather than duplicating
// its channel resolution.
//
// What this adds over rustup's own laziness is materialising the toolchain up
// front, with the components the checks actually need. rustup would otherwise
// install it on first use, in the middle of `cargo clippy`, and a missing
// clippy component surfaces as a confusing check failure rather than as a
// setup step.
func provisionRust(ctx context.Context, req Requirement, ambient string, present bool) (*step, error) {
	if present && !olderThan(ambient, req.Version) {
		return nil, nil
	}
	if !executil.Available("rustup") {
		return nil, fmt.Errorf(
			"no cargo on PATH and rustup is not installed either; install rustup (https://rustup.rs) so bulwark can provision the %s toolchain",
			displayRaw(req))
	}
	// rustup needs a concrete channel. The repo's own word for it is the
	// right one to pass — "stable" and "1.96" are both valid channels, and
	// rustup understands them where bulwark's version comparison cannot.
	channel := req.Raw
	if channel == "" {
		channel = "stable"
	}
	// Idempotent: a channel already installed makes this a no-op that exits
	// zero, which is why it is safe to run on every scan.
	r := executil.Run(ctx, "", "rustup", "toolchain", "install", channel,
		"--profile", "minimal", "--component", "clippy", "--component", "rustfmt")
	if !r.Ok() {
		return nil, fmt.Errorf("rustup toolchain install %s: %w", channel, r.Err)
	}

	st := &step{
		note: fmt.Sprintf("rust: installed toolchain %s via rustup (declared in %s; %s)",
			channel, req.Source, absentOrOld(ambient, present)),
	}
	// Installing is not selecting. rustup picks a toolchain by reading
	// rust-toolchain.toml from the directory cargo runs in, which covers the
	// normal case for free — internal/rust and internal/coverage both run
	// cargo inside the crate directory, so the file bulwark read is the file
	// rustup reads.
	//
	// An override has no such file. The version exists only in .bulwark.yml,
	// rustup cannot see it, and without being told it would install the
	// requested channel and then go on running the old default — reporting
	// success for a toolchain nothing actually uses. RUSTUP_TOOLCHAIN is the
	// explicit selection, and it is set *only* here: applying it whenever a
	// channel was read from a manifest would override rustup's own per-crate
	// selection, which is exactly what internal/rust's "bulwark doesn't
	// second-guess that" stance says not to do, and would break a monorepo
	// whose crates pin different channels.
	if req.Overridden {
		st.vars = []string{"RUSTUP_TOOLCHAIN=" + channel}
		st.note += " and selected it via RUSTUP_TOOLCHAIN (the version came from config, so no rust-toolchain.toml names it)"
	}
	return st, nil
}

// provisionNode makes the declared Node runtime available.
//
// Node has no equivalent of GOTOOLCHAIN or rustup that can be assumed present
// — nvm/fnm/volta are all optional and mutually exclusive — so this is the
// one ecosystem bulwark downloads and unpacks itself, from nodejs.org, with
// the digest read from that release's SHASUMS256.txt rather than from the
// archive.
func provisionNode(ctx context.Context, req Requirement, ambient string, present bool) (*step, error) {
	if req.Unpinned() {
		if present {
			return nil, nil
		}
		return nil, fmt.Errorf("no Node on PATH, and neither engines.node nor .nvmrc declares a version to install")
	}
	if present && !olderThan(ambient, req.Version) {
		return nil, nil
	}
	version := nodeReleaseName(req.Version)
	dir, err := cacheRoot("node-" + strings.TrimPrefix(version, "v"))
	if err != nil {
		return nil, err
	}
	if err := installOnce(dir, func(staging string) error {
		return downloadNode(ctx, version, staging)
	}); err != nil {
		return nil, err
	}
	return &step{
		pathDirs: []string{filepath.Join(dir, "bin")},
		note: fmt.Sprintf("typescript: installed Node %s (declared in %s; %s)",
			version, req.Source, absentOrOld(ambient, present)),
	}, nil
}

// nodeReleaseName renders a canonical version as a Node release directory
// name. Node releases are always MAJOR.MINOR.PATCH, so a floor of ">=20"
// resolves to that major's first release.
func nodeReleaseName(version string) string {
	v := strings.TrimPrefix(version, "v")
	if n := strings.Count(v, "."); n < 2 {
		v += strings.Repeat(".0", 2-n)
	}
	return "v" + v
}

// downloadNode fetches a Node tarball into staging.
//
// The .tar.gz is chosen over the .tar.xz that nodejs.org also publishes
// purely because Go's standard library can decompress gzip and cannot
// decompress xz; taking the xz would mean either a new dependency or shelling
// out to a tar binary that may not exist on a minimal image.
func downloadNode(ctx context.Context, version, staging string) error {
	base := "https://nodejs.org/dist/" + version
	name := fmt.Sprintf("node-%s-%s-%s.tar.gz", version, nodeOS(), nodeArch())

	sums, err := fetch(ctx, base+"/SHASUMS256.txt")
	if err != nil {
		return err
	}
	want := ""
	for line := range strings.SplitSeq(string(sums), "\n") {
		sum, file, found := strings.Cut(strings.TrimSpace(line), "  ")
		if found && file == name {
			want = sum
			break
		}
	}
	if want == "" {
		return fmt.Errorf("node %s publishes no %s", version, name)
	}
	data, err := downloadVerified(ctx, base+"/"+name, want)
	if err != nil {
		return err
	}
	return extractTarGz(data, staging)
}

// nodeOS and nodeArch translate Go's platform names into the ones nodejs.org
// uses in its filenames. Only the pair bulwark itself is built for matters —
// linux/darwin × amd64/arm64, per .goreleaser.yml.
func nodeOS() string { return runtime.GOOS }

func nodeArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}

// step is one ecosystem's contribution to the resolved environment.
type step struct {
	pathDirs []string
	vars     []string
	note     string
}

// display renders a probed version for a message, naming the unidentifiable
// case rather than printing an empty string.
func display(version string) string {
	if version == "" {
		return "an unidentified version"
	}
	return strings.TrimPrefix(version, "v")
}

// displayRaw prefers what the manifest literally said over the canonical
// form, so a "stable" channel is reported as "stable".
func displayRaw(req Requirement) string {
	if req.Raw != "" {
		return req.Raw
	}
	return display(req.Version)
}

// absentOrOld describes why provisioning happened, which is the part a reader
// of the log actually wants: whether their runner had nothing, or had
// something too old.
func absentOrOld(ambient string, present bool) string {
	if !present {
		return "none was on PATH"
	}
	return "ambient " + display(ambient) + " is older"
}

// ensureExecutable is a defensive fix-up after extraction: archive modes are
// preserved by extractTarGz, but a tarball produced with a restrictive umask
// can still land a bin/ entry without the execute bit, which then fails at
// use with a bare "permission denied".
func ensureExecutable(binDir string) {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			_ = os.Chmod(filepath.Join(binDir, e.Name()), info.Mode().Perm()|0o111) // #nosec G302 -- restoring the execute bit on a toolchain binary is the point
		}
	}
}
