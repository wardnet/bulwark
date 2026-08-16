package toolchain

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wardnet/bulwark/internal/detect"
)

// The whole point of the "prefer ambient" rule: a runner that already has a
// good-enough toolchain must produce no download and no install. This pins
// the decision itself, independent of any provisioner.
func TestSatisfied(t *testing.T) {
	pinned := Requirement{Ecosystem: detect.Go, Version: "v1.26.4", Raw: "1.26.4"}
	unpinned := Requirement{Ecosystem: detect.Rust, Raw: "stable"}

	for _, tc := range []struct {
		name    string
		req     Requirement
		ambient string
		present bool
		want    bool
	}{
		{"exact match is satisfied", pinned, "v1.26.4", true, true},
		{"newer ambient is satisfied", pinned, "v1.26.5", true, true},
		{"older ambient is not", pinned, "v1.25.0", true, false},
		{"absent is not, however new the pin", pinned, "", false, false},
		// An unpinned requirement names no floor, so anything present clears it.
		{"unpinned is satisfied by anything present", unpinned, "v1.80.0", true, true},
		{"unpinned still needs something present", unpinned, "", false, false},
		// A toolchain that will not identify itself cannot be shown to
		// satisfy a pin, so it is treated as too old.
		{"unidentifiable ambient fails a pin", pinned, "", true, false},
		{"unidentifiable ambient clears no pin", unpinned, "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := satisfied(tc.req, tc.ambient, tc.present); got != tc.want {
				t.Fatalf("satisfied(%+v, %q, %v) = %v, want %v", tc.req, tc.ambient, tc.present, got, tc.want)
			}
		})
	}
}

// With every ecosystem's toolchain already good enough, Ensure must be a
// no-op: nothing added to PATH, no variables set, and — critically — no
// network access, which is what makes this test safe to run offline at all.
func TestEnsureIsANoOpWhenAmbientSatisfies(t *testing.T) {
	dir := t.TempDir()
	// A go.mod pinning something ancient, so whatever Go is running these
	// tests necessarily satisfies it.
	write(t, dir, "go.mod", "module x\n\ngo 1.16\n")

	var log bytes.Buffer
	env, err := Ensure(context.Background(), dir, []detect.Ecosystem{detect.Go}, Overrides{}, &log)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(env.PathDirs) != 0 || len(env.Vars) != 0 {
		t.Fatalf("expected an empty env for an already-satisfied toolchain, got %+v", env)
	}
	if !strings.Contains(log.String(), "using ambient go") {
		t.Errorf("Ensure should say it reused the ambient toolchain; log was %q", log.String())
	}
}

// Disabling provisioning must still diagnose. An air-gapped runner gets to
// know its toolchain is too old without bulwark trying to fix it — silence
// would be the failure mode this package exists to remove.
func TestEnsureDisabledStillReportsShortfall(t *testing.T) {
	dir := t.TempDir()
	// Far in the future, so no real toolchain can satisfy it.
	write(t, dir, "go.mod", "module x\n\ngo 99.0.0\n")

	var log bytes.Buffer
	env, err := Ensure(context.Background(), dir, []detect.Ecosystem{detect.Go}, Overrides{Disabled: true}, &log)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(env.PathDirs) != 0 || len(env.Vars) != 0 {
		t.Fatalf("provisioning was disabled but the env changed: %+v", env)
	}
	out := log.String()
	if !strings.Contains(out, "toolchain.enabled is false") {
		t.Errorf("log should name the disabling setting, got %q", out)
	}
	if !strings.Contains(out, "99.0.0") {
		t.Errorf("log should name the version that went unsatisfied, got %q", out)
	}
}

// A provisioning failure is a warning, not a scan failure: this step is
// preparation, and falling through to "whatever is on PATH" is exactly
// today's behavior. Rust with no rustup is the cheapest way to reach that
// path without touching the network.
func TestEnsureProvisioningFailureWarnsAndContinues(t *testing.T) {
	if _, err := os.Stat("/nonexistent"); err == nil {
		t.Skip("unexpected filesystem layout")
	}
	dir := t.TempDir()
	write(t, dir, "Cargo.toml", "[package]\nname = \"x\"\n")
	write(t, dir, "rust-toolchain.toml", "[toolchain]\nchannel = \"99.0.0\"\n")

	// Empty PATH: neither cargo nor rustup is findable, so provisionRust
	// fails at once with no network involved.
	t.Setenv("PATH", "")

	var log bytes.Buffer
	env, err := Ensure(context.Background(), dir, []detect.Ecosystem{detect.Rust}, Overrides{}, &log)
	if err != nil {
		t.Fatalf("Ensure returned an error; a provisioning failure must be a warning: %v", err)
	}
	if len(env.PathDirs) != 0 {
		t.Fatalf("a failed provision must contribute nothing to the env, got %+v", env)
	}
	if !strings.Contains(log.String(), "warning:") {
		t.Errorf("a failed provision must be named on the log, got %q", log.String())
	}
}

func TestEnvActivatePrependsPathAndSetsVars(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("GOTOOLCHAIN", "auto")

	env := &Env{
		PathDirs: []string{"/opt/go/bin", "/opt/node/bin"},
		Vars:     []string{"GOTOOLCHAIN=go1.26.5"},
	}
	if err := env.Activate(); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	sep := string(os.PathListSeparator)
	want := "/opt/go/bin" + sep + "/opt/node/bin" + sep + "/usr/bin"
	if got := os.Getenv("PATH"); got != want {
		t.Errorf("PATH = %q, want %q", got, want)
	}
	if got := os.Getenv("GOTOOLCHAIN"); got != "go1.26.5" {
		t.Errorf("GOTOOLCHAIN = %q, want go1.26.5", got)
	}
}

// A provisioned toolchain exists precisely because the ambient one was
// missing or too old, so it has to come first on PATH — appending would
// install it and then never use it.
func TestEnvActivatePrependsRatherThanAppends(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	env := &Env{PathDirs: []string{"/opt/go/bin"}}
	if err := env.Activate(); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !strings.HasPrefix(os.Getenv("PATH"), "/opt/go/bin") {
		t.Fatalf("PATH = %q, want the provisioned dir first", os.Getenv("PATH"))
	}
}

func TestEnvActivateNilIsSafe(t *testing.T) {
	var env *Env
	if err := env.Activate(); err != nil {
		t.Fatalf("Activate on a nil Env: %v", err)
	}
}

// The zip-slip guard. bulwark unpacks these archives with the user's own
// privileges, so an entry that escapes the destination writes anywhere the
// user can.
func TestSafeJoinRejectsEscapes(t *testing.T) {
	dest := t.TempDir()
	for _, rel := range []string{
		"../escaped",
		"../../etc/passwd",
		"a/../../../etc/passwd",
	} {
		if _, err := safeJoin(dest, rel); err == nil {
			t.Errorf("safeJoin(%q) was allowed; it escapes the destination", rel)
		}
	}
	// A path that merely contains ".." but stays inside is fine.
	if _, err := safeJoin(dest, "a/b/../c"); err != nil {
		t.Errorf("safeJoin rejected a contained path: %v", err)
	}
}

// A destination that is a prefix of a sibling directory name must not be
// mistaken for containment: "/tmp/dest-evil" is not inside "/tmp/dest".
func TestSafeJoinIsNotFooledByAPrefixSibling(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	if _, err := safeJoin(dest, "../dest-evil/x"); err == nil {
		t.Fatal("safeJoin allowed a sibling directory sharing the destination's prefix")
	}
}

// A symlink whose target is an ABSOLUTE path is the escape the obvious
// containment check does not catch: filepath.Join("bin", "/etc/passwd") is
// "bin/etc/passwd", so joining the target against the link's own directory
// silently reinterprets it as relative and every such entry passes. Combined
// with a later regular-file entry at the same name — which O_CREATE would
// open *through* the symlink — an archive could overwrite any file the user
// can write. Both halves are covered here.
func TestExtractTarGzRejectsAbsoluteSymlinkTarget(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("ORIGINAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := tarGzEntries(t, []tar.Header{
		{Name: "pkg/bin/npx", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777},
		{Name: "pkg/bin/npx", Typeflag: tar.TypeReg, Mode: 0o755},
	}, map[string]string{"pkg/bin/npx": "PWNED\n"})

	if err := extractTarGz(archive, t.TempDir()); err == nil {
		t.Error("extractTarGz accepted a symlink to an absolute path outside the destination")
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ORIGINAL\n" {
		t.Fatalf("a file outside the destination was overwritten with %q", got)
	}
}

// The relative form of the same escape.
func TestExtractTarGzRejectsRelativeSymlinkEscape(t *testing.T) {
	archive := tarGzEntries(t, []tar.Header{
		{Name: "pkg/bin/npx", Typeflag: tar.TypeSymlink, Linkname: "../../../../etc/passwd", Mode: 0o777},
	}, nil)
	if err := extractTarGz(archive, t.TempDir()); err == nil {
		t.Error("extractTarGz accepted a relative symlink escaping the destination")
	}
}

// Contained symlinks are the normal case — Node's tarball links npm and npx
// into lib/node_modules — so the guard above must not reject them.
func TestExtractTarGzKeepsContainedSymlinks(t *testing.T) {
	archive := tarGzEntries(t, []tar.Header{
		{Name: "pkg/lib/node_modules/npm/bin/npm-cli.js", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "pkg/bin/npm", Typeflag: tar.TypeSymlink, Linkname: "../lib/node_modules/npm/bin/npm-cli.js", Mode: 0o777},
	}, map[string]string{"pkg/lib/node_modules/npm/bin/npm-cli.js": "#!/usr/bin/env node\n"})

	dest := t.TempDir()
	if err := extractTarGz(archive, dest); err != nil {
		t.Fatalf("extractTarGz rejected a legitimate contained symlink: %v", err)
	}
	info, err := os.Lstat(filepath.Join(dest, "bin", "npm"))
	if err != nil {
		t.Fatalf("bin/npm missing: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("bin/npm should have been created as a symlink")
	}
}

// tarGzEntries builds a gzipped tar from explicit headers, so a test can
// control entry type, order and link targets. bodies supplies content for
// regular entries, keyed by header name.
func tarGzEntries(t *testing.T, headers []tar.Header, bodies map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range headers {
		body := bodies[h.Name]
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStripLeading(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"go/bin/go", "bin/go"},
		{"node-v22.11.0-linux-x64/bin/node", "bin/node"},
		// A bare top-level entry has nothing left once its wrapper is
		// dropped, and is skipped rather than written to the destination root.
		{"go", ""},
		{"", ""},
	} {
		if got := stripLeading(tc.in); got != tc.want {
			t.Errorf("stripLeading(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExtractTarGz(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"go/bin/go":       "#!/bin/sh\necho go\n",
		"go/src/fmt/x.go": "package fmt\n",
	})
	dest := t.TempDir()
	if err := extractTarGz(archive, dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	// The wrapping directory is dropped, so bin/ lands directly in dest.
	got, err := os.ReadFile(filepath.Join(dest, "bin", "go"))
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if !strings.Contains(string(got), "echo go") {
		t.Errorf("extracted content = %q", got)
	}
	info, err := os.Stat(filepath.Join(dest, "bin", "go"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("extracted binary lost its execute bit: %v", info.Mode())
	}
}

func TestExtractTarGzRejectsEscapingEntry(t *testing.T) {
	archive := tarGz(t, map[string]string{"go/../../escaped": "owned\n"})
	dest := t.TempDir()
	if err := extractTarGz(archive, dest); err == nil {
		t.Fatal("extractTarGz accepted an entry escaping the destination")
	}
}

// installOnce is what stops an interrupted download leaving a half-populated
// directory that the next run treats as a finished install.
func TestInstallOnceIsAtomicAndSkipsExisting(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "go-1.26.5")

	// A failing install must leave nothing behind.
	err := installOnce(dir, func(staging string) error {
		if writeErr := os.WriteFile(filepath.Join(staging, "partial"), []byte("x"), 0o600); writeErr != nil {
			return writeErr
		}
		return os.ErrDeadlineExceeded
	})
	if err == nil {
		t.Fatal("installOnce should surface the install error")
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		t.Fatal("a failed install left the destination directory in place")
	}

	// A successful one lands the content.
	if err := installOnce(dir, func(staging string) error {
		return os.WriteFile(filepath.Join(staging, "ok"), []byte("y"), 0o600)
	}); err != nil {
		t.Fatalf("installOnce: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ok")); err != nil {
		t.Fatalf("successful install did not land: %v", err)
	}

	// A second call must not re-run the installer.
	called := false
	if err := installOnce(dir, func(string) error { called = true; return nil }); err != nil {
		t.Fatalf("installOnce on an existing dir: %v", err)
	}
	if called {
		t.Error("installOnce re-ran the installer for an already-present toolchain")
	}
}

// tarGz builds a gzipped tar in memory from path -> content.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
