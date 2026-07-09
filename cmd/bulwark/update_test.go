package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRelease serves the GitHub release URLs selfUpdate and
// latestReleaseVersion depend on: the releases/latest redirect, a raw bulwark
// binary, and checksums.txt. checksum overrides the real digest when non-empty.
func fakeRelease(t *testing.T, ver string, binary []byte, checksum string) *httptest.Server {
	t.Helper()
	asset := fmt.Sprintf("bulwark_%s_%s_%s", ver, runtime.GOOS, runtime.GOARCH)
	if checksum == "" {
		sum := sha256.Sum256(binary)
		checksum = hex.EncodeToString(sum[:])
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v"+ver, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/v"+ver+"/"+asset, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(binary)
	})
	mux.HandleFunc("/releases/download/v"+ver+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", checksum, asset)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	orig := releaseBaseURL
	releaseBaseURL = srv.URL
	t.Cleanup(func() { releaseBaseURL = orig })
	return srv
}

// TestUpdateAvailable guards the version-ordering of the update nudge and the
// `bulwark update` guard: an update is offered only when the resolved latest is
// STRICTLY newer than the running version. Regression for the stale-cache bug
// where a cached older tag nudges a newer build to "update".
func TestUpdateAvailable(t *testing.T) {
	cases := []struct {
		name        string
		latest, cur string
		want        bool
	}{
		{"newer patch", "1.0.1", "1.0.0", true},
		{"newer minor", "1.1.0", "1.0.9", true},
		{"newer major", "2.0.0", "1.9.9", true},
		{"same version", "1.0.1", "1.0.1", false},
		{"older patch (stale cache)", "1.0.0", "1.0.1", false},
		{"older major", "0.9.9", "1.0.0", false},
		{"empty latest (check failed/never ran)", "", "1.0.1", false},
		{"unparseable current (non-release build)", "1.0.1", "garbage", false},
		{"unparseable latest", "garbage", "1.0.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := updateAvailable(tc.latest, tc.cur); got != tc.want {
				t.Errorf("updateAvailable(%q, %q) = %v, want %v", tc.latest, tc.cur, got, tc.want)
			}
		})
	}
}

func TestLatestReleaseVersion(t *testing.T) {
	fakeRelease(t, "1.9.0", []byte("bin"), "")

	got, err := latestReleaseVersion(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatalf("latestReleaseVersion: %v", err)
	}
	if got != "1.9.0" {
		t.Fatalf("got %q, want %q", got, "1.9.0")
	}
}

func TestLatestReleaseVersionNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	orig := releaseBaseURL
	releaseBaseURL = srv.URL
	t.Cleanup(func() { releaseBaseURL = orig })

	if _, err := latestReleaseVersion(context.Background(), http.DefaultClient); err == nil {
		t.Fatal("expected error when releases/latest does not redirect")
	}
}

func TestSelfUpdateReplacesBinary(t *testing.T) {
	newBinary := []byte("new bulwark binary")
	fakeRelease(t, "1.9.0", newBinary, "")

	exe := filepath.Join(t.TempDir(), "bulwark")
	if err := os.WriteFile(exe, []byte("old bulwark binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := selfUpdate(context.Background(), "1.9.0", exe); err != nil {
		t.Fatalf("selfUpdate: %v", err)
	}

	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newBinary) {
		t.Fatalf("binary not replaced: got %q", got)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode = %v, want 0755", fi.Mode().Perm())
	}
	// The staging temp file must not be left behind.
	entries, err := os.ReadDir(filepath.Dir(exe))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bulwark-update-") {
			t.Fatalf("staging file left behind: %s", e.Name())
		}
	}
}

func TestSelfUpdateChecksumMismatch(t *testing.T) {
	fakeRelease(t, "1.9.0", []byte("new bulwark binary"), strings.Repeat("0", 64))

	exe := filepath.Join(t.TempDir(), "bulwark")
	original := []byte("old bulwark binary")
	if err := os.WriteFile(exe, original, 0o755); err != nil {
		t.Fatal(err)
	}

	err := selfUpdate(context.Background(), "1.9.0", exe)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got %v", err)
	}
	got, readErr := os.ReadFile(exe)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatal("binary was replaced despite checksum mismatch")
	}
}

func TestUpdateCmdRefusesDevBuild(t *testing.T) {
	// Test binaries always run with the default version; fail loudly if that
	// assumption ever breaks, since this is the refusal path's only coverage.
	if version != "dev" {
		t.Fatalf("test binary has version %q, expected dev", version)
	}
	cmd := newUpdateCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "built from source") {
		t.Fatalf("expected built-from-source refusal, got %v", err)
	}
}

// TestUpdateCmdRejectsUnrecognizedVersion: a non-"dev" build whose version is not
// valid semver (a hand-built binary) must fail `bulwark update` with a clear
// message rather than silently reporting "already the latest release".
func TestUpdateCmdRejectsUnrecognizedVersion(t *testing.T) {
	orig := version
	version = "not-a-release"
	t.Cleanup(func() { version = orig })

	// A release must resolve so control reaches the version-validity check.
	fakeRelease(t, "9.9.9", []byte("bin"), "")

	cmd := newUpdateCmd()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "not a recognized release version") {
		t.Fatalf("expected unrecognized-version error, got %v", err)
	}
}
