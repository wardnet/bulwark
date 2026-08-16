// Package toolchain makes sure the language toolchain each detected
// ecosystem needs — the Go, Rust and Node runtimes themselves — is present at
// the version the repository declares, before any scanner runs.
//
// This closes the one hole in bulwark's otherwise complete "pin the exact
// toolchain, don't reuse ambient installs" principle (see internal/golang).
// bulwark provisions everything it *runs* — gosec and govulncheck via `go
// install` into a version-keyed cache, cargo-audit/cargo-deny via `cargo
// install`, ESLint via npm, Semgrep via pipx — but until now it assumed the
// language toolchain it does that provisioning *with* was simply there. On a
// GitHub-hosted runner that holds, which is why nothing was visibly broken;
// on a self-hosted or container runner without Go it fails at `go install`,
// the version is whatever the image happens to ship rather than what the repo
// declares, and there is no shared module cache so every run re-downloads.
//
// Two rules shape the whole package:
//
//   - The version comes from what the repository already states — the `go`
//     and `toolchain` directives in every discovered go.mod, the channel in
//     rust-toolchain.toml, engines.node or .nvmrc. Never from .bulwark.yml.
//     Those files are already authoritative and tool-enforced, so a second
//     copy in bulwark's config could only agree redundantly or drift
//     silently, and a stale duplicate is worse than none because it reads as
//     authoritative. .bulwark.yml can override, which is an explicit local
//     exception rather than a parallel source of truth.
//   - An ambient toolchain that already satisfies the declared version is
//     used as-is. Downloading a toolchain that is already present and correct
//     is pure cost, and on the runners bulwark actually runs on today that is
//     the overwhelmingly common case — so the common path here does no
//     network I/O at all.
//
// Doing this in bulwark rather than in each caller's CI is what makes it work
// for a monorepo. bulwark already knows which ecosystems it detected and in
// which directories, so it reads every go.mod under the scan root rather than
// one at a fixed path — the mistake that made gt's short-lived `setup-go`
// step (19e4b77, reverted in a0ed107) a no-op for wardnet, whose modules live
// under wctl/ and sdk/wardnet-go/. It is also the only place that helps
// wardnet at all, since wardnet calls wardnet/bulwark@v1 directly rather than
// through gt.
package toolchain

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"wardnet/bulwark/internal/detect"
)

// Overrides is the .bulwark.yml-supplied part of toolchain resolution, passed
// in rather than imported so internal/config stays a leaf this package
// doesn't depend on.
type Overrides struct {
	// Disabled turns provisioning off entirely. Probing and reporting still
	// happen, so an air-gapped or fully-preprovisioned runner keeps the
	// diagnostics without the downloads.
	Disabled bool
	// Go, Rust and Node override the version read from the manifests. Empty
	// means "use what the repo declares", which is the intended state.
	Go, Rust, Node string
	// Per-language detection excludes, mirroring how scan and coverage
	// already scope excludes per language.
	GoExclude, RustExclude, TSExclude []string
}

// For returns the override for one ecosystem, or "" if none is set.
func (o Overrides) For(e detect.Ecosystem) string {
	switch e {
	case detect.Go:
		return o.Go
	case detect.Rust:
		return o.Rust
	case detect.TypeScript:
		return o.Node
	default:
		return ""
	}
}

// Env is the environment change needed to make the resolved toolchains
// usable: directories to prepend to PATH, and variables to set.
type Env struct {
	PathDirs []string
	Vars     []string
}

// Activate applies the environment to the current process, so every
// subsequent executil.Run — which inherits os.Environ() and resolves binaries
// through PATH — picks up the resolved toolchains.
//
// Mutating the process environment is deliberate rather than threading an Env
// through every scanner's signature. bulwark is a single-shot CLI that runs
// one scan and exits; this is precisely what a shell `export PATH=...` ahead
// of it would do, and it keeps the change to one call site instead of every
// Check and Compute function. Ensure returns the Env rather than applying it
// itself so the decision stays visible at the call site and tests can inspect
// what would happen without touching global state.
func (e *Env) Activate() error {
	if e == nil {
		return nil
	}
	for _, kv := range e.Vars {
		key, value, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	if len(e.PathDirs) == 0 {
		return nil
	}
	// Prepended, not appended: a provisioned toolchain exists precisely
	// because the ambient one was missing or too old, so it has to win.
	parts := append(append([]string{}, e.PathDirs...), os.Getenv("PATH"))
	return os.Setenv("PATH", strings.Join(parts, string(os.PathListSeparator)))
}

// Ensure resolves, and where necessary provisions, a language toolchain for
// every detected ecosystem, returning the environment that makes them usable.
//
// Provisioning failures are reported to w and do not fail the scan. That is a
// deliberate asymmetry with the rest of bulwark's gates: this step is
// preparation, not a check, and today's behavior — trust whatever is on PATH
// — is exactly what falling through to leaves. Turning a working scan on a
// GitHub-hosted runner into a hard failure because a toolchain download hit a
// network blip would be a regression, and if the toolchain really is absent
// the very next step fails loudly and specifically ("cargo: executable file
// not found"). What must not happen is failing silently, so every skip,
// substitution and failure is named on w.
func Ensure(ctx context.Context, root string, ecosystems []detect.Ecosystem, ov Overrides, w io.Writer) (*Env, error) {
	reqs, err := Requirements(root, ecosystems, ov)
	if err != nil {
		return nil, err
	}

	env := &Env{}
	for _, req := range reqs {
		p, ok := probes[req.Ecosystem]
		if !ok {
			continue
		}
		ambient, present := installed(ctx, p)

		if satisfied(req, ambient, present) {
			if err := logf(w, "%s: using ambient %s %s (%s)\n",
				req.Ecosystem, p.bin, display(ambient), declaredBy(req)); err != nil {
				return nil, err
			}
			// Satisfied is not the same as nothing to do. Go still needs its
			// toolchain pinned so that installing an external tool cannot
			// silently switch away from the version just verified — see
			// pinAmbientGo. This costs no download and is why the branch
			// doesn't simply `continue`.
			if req.Ecosystem == detect.Go {
				if st := pinAmbientGo(req); st != nil {
					env.Vars = append(env.Vars, st.vars...)
					if err := logf(w, "%s\n", st.note); err != nil {
						return nil, err
					}
				}
			}
			continue
		}
		if ov.Disabled {
			if err := logf(w, "warning: %s toolchain %s, and toolchain.enabled is false — continuing with what is on PATH\n",
				req.Ecosystem, shortfall(req, ambient, present)); err != nil {
				return nil, err
			}
			continue
		}

		st, err := provision(ctx, req, ambient, present)
		if err != nil {
			if logErr := logf(w, "warning: could not provision the %s toolchain (%s): %v — continuing with what is on PATH\n",
				req.Ecosystem, shortfall(req, ambient, present), err); logErr != nil {
				return nil, logErr
			}
			continue
		}
		if st == nil {
			continue
		}
		for _, dir := range st.pathDirs {
			ensureExecutable(dir)
		}
		env.PathDirs = append(env.PathDirs, st.pathDirs...)
		env.Vars = append(env.Vars, st.vars...)
		if st.note != "" {
			if err := logf(w, "%s\n", st.note); err != nil {
				return nil, err
			}
		}
	}
	return env, nil
}

// provision dispatches to the per-ecosystem provisioner. Each one differs in
// kind, not just in URL: Go delegates to its own GOTOOLCHAIN mechanism, Rust
// delegates to rustup, and only Node is downloaded and unpacked by bulwark.
func provision(ctx context.Context, req Requirement, ambient string, present bool) (*step, error) {
	switch req.Ecosystem {
	case detect.Go:
		return provisionGo(ctx, req, ambient, present)
	case detect.Rust:
		return provisionRust(ctx, req, ambient, present)
	case detect.TypeScript:
		return provisionNode(ctx, req, ambient, present)
	default:
		return nil, nil
	}
}

// satisfied reports whether the ambient toolchain is good enough to use
// as-is. An unpinned requirement is satisfied by any present toolchain — the
// repo named no floor, so there is nothing to be too old for.
func satisfied(req Requirement, ambient string, present bool) bool {
	if !present {
		return false
	}
	if req.Unpinned() {
		return true
	}
	return !olderThan(ambient, req.Version)
}

// declaredBy renders the requirement side of a message as a complete
// parenthetical, because the three cases don't share a sentence shape: a
// pinned version is something the ambient toolchain *satisfies*, a named
// channel is something it merely *matches in kind*, and an absent
// declaration is the notable fact all by itself — that last one is worth
// saying out loud rather than papering over, since "no version declared" is
// often a gap the reader can go close.
func declaredBy(req Requirement) string {
	if req.Unpinned() {
		if req.Raw != "" {
			return fmt.Sprintf("%s declares the %q channel", req.Source, req.Raw)
		}
		return "no version declared by this repo"
	}
	return fmt.Sprintf("satisfies %s from %s", displayRaw(req), req.Source)
}

// shortfall renders why the ambient toolchain was not good enough.
func shortfall(req Requirement, ambient string, present bool) string {
	if !present {
		return "is not installed"
	}
	if req.Unpinned() {
		return "is unusable"
	}
	return fmt.Sprintf("is %s, older than the declared %s", display(ambient), displayRaw(req))
}

func logf(w io.Writer, format string, args ...any) error {
	if w == nil {
		return nil
	}
	_, err := fmt.Fprintf(w, format, args...)
	return err
}
