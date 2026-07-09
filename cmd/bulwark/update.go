package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

// releaseBaseURL is the GitHub project URL releases are downloaded from.
// It is a variable so tests can point it at a local HTTP server.
var releaseBaseURL = "https://github.com/wardnet/bulwark"

// updateCheckTTL bounds how often the background update nudge hits GitHub.
const updateCheckTTL = 24 * time.Hour

// releaseAssetURL returns the download URL for a named asset of release ver.
func releaseAssetURL(ver, asset string) string {
	return fmt.Sprintf("%s/releases/download/v%s/%s", releaseBaseURL, ver, asset)
}

// canonicalVersion prefixes a bare version ("1.0.1") with the "v" that
// golang.org/x/mod/semver requires and reports whether the result is valid
// semver. Both the release tag (redirect target, "v" trimmed) and the injected
// build version are bare, so both flow through here.
func canonicalVersion(v string) (string, bool) {
	sv := "v" + v
	return sv, semver.IsValid(sv)
}

// updateAvailable reports whether latest is a strictly newer release than
// current, so the nudge and `bulwark update` only offer a real upgrade — never
// a sideways or downward move (e.g. a stale cache holding an older tag).
// Comparison is semantic, not lexical. An empty or unparseable latest OR current
// (a failed/never-run check, or a non-release build) is treated as "no update".
func updateAvailable(latest, current string) bool {
	l, lok := canonicalVersion(latest)
	c, cok := canonicalVersion(current)
	if !lok || !cok {
		return false
	}
	return semver.Compare(l, c) > 0
}

func newUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "update",
		Short:         "Update bulwark to the latest release",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if version == "dev" {
				return errors.New("this bulwark was built from source — update via git, not 'bulwark update'")
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			exe, err = filepath.EvalSymlinks(exe)
			if err != nil {
				return err
			}
			latest, err := latestReleaseVersion(cmd.Context(), http.DefaultClient)
			if err != nil {
				return fmt.Errorf("resolve latest release: %w", err)
			}
			// A non-release build version (not "dev", but not valid semver either)
			// can't be ordered against the latest tag. Surface it explicitly on this
			// explicit-action path rather than letting updateAvailable's silent
			// false read as "already the latest release".
			if _, ok := canonicalVersion(version); !ok {
				return fmt.Errorf("running version %q is not a recognized release version — reinstall from a GitHub release to enable updates", version)
			}
			if !updateAvailable(latest, version) {
				fmt.Printf("bulwark v%s is already the latest release\n", version)
				return nil
			}
			fmt.Printf("updating bulwark v%s -> v%s...\n", version, latest)
			if err := selfUpdate(cmd.Context(), latest, exe); err != nil {
				return err
			}
			fmt.Printf("updated %s to v%s\n", exe, latest)
			return nil
		},
	}
}

// latestReleaseVersion resolves the latest released version (without the "v"
// prefix) by reading the redirect target of the releases/latest URL, avoiding
// the rate-limited GitHub API.
func latestReleaseVersion(ctx context.Context, client *http.Client) (string, error) {
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseBaseURL+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("expected a redirect from %s, got HTTP %d", req.URL, resp.StatusCode)
	}

	// Location ends in .../releases/tag/v<version>.
	loc := resp.Header.Get("Location")
	tag := loc[strings.LastIndex(loc, "/")+1:]
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("cannot parse release tag from redirect %q", loc)
	}
	return strings.TrimPrefix(tag, "v"), nil
}

// selfUpdate downloads the raw bulwark binary for the given version, verifies
// its SHA-256 against the release's checksums.txt, and atomically replaces the
// binary at exe.
func selfUpdate(ctx context.Context, ver, exe string) error {
	asset := fmt.Sprintf("bulwark_%s_%s_%s", ver, runtime.GOOS, runtime.GOARCH)
	url := releaseAssetURL(ver, asset)

	// Stage in the same directory so the final rename is atomic.
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".bulwark-update-*")
	if err != nil {
		return fmt.Errorf("stage update next to %s (is the directory writable?): %w", exe, err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpPath) }()

	if err := downloadBinary(ctx, url, tmpPath, 0o755); err != nil {
		return err
	}
	if err := verifyChecksum(ctx, http.DefaultClient, ver, asset, tmpPath); err != nil {
		return err
	}
	// CreateTemp made the file 0600 and OpenFile's mode is ignored for existing
	// files, so this chmod — not the mode passed to downloadBinary — is what
	// makes the binary executable.
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		return fmt.Errorf("replace %s (is it writable?): %w", exe, err)
	}
	return nil
}

// downloadBinary fetches url and writes it to dst with the given mode.
func downloadBinary(ctx context.Context, url, dst string, mode os.FileMode) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d fetching %s — asset not found for %s/%s",
			resp.StatusCode, url, runtime.GOOS, runtime.GOARCH)
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// verifyChecksum fetches the release's checksums.txt and compares the named
// asset's SHA-256 against the file at path.
func verifyChecksum(ctx context.Context, client *http.Client, ver, asset, path string) error {
	url := releaseAssetURL(ver, "checksums.txt")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}
	sums, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var want string
	for line := range strings.SplitSeq(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("no checksum for %s in %s", asset, url)
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", asset, got, want)
	}
	return nil
}

// updateCheckState is the on-disk cache for the background update nudge.
type updateCheckState struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

// maybeNudgeUpdate prints a one-line notice on stderr when a newer release is
// known. It contacts GitHub at most once per updateCheckTTL, never in CI,
// non-interactive, or from-source ("dev") runs, and stays silent on any failure.
func maybeNudgeUpdate() {
	if version == "dev" || os.Getenv("CI") != "" {
		return
	}
	if fi, err := os.Stderr.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	path := filepath.Join(cacheDir, "bulwark", "update-check.json")

	var st updateCheckState
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if time.Since(st.CheckedAt) >= updateCheckTTL {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		latest, err := latestReleaseVersion(ctx, http.DefaultClient)
		// Record the attempt even when the check fails (offline, blocked) —
		// otherwise every invocation past the TTL re-pays the network timeout.
		st.CheckedAt = time.Now()
		if err == nil {
			st.Latest = latest
		}
		if data, err := json.Marshal(st); err == nil {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				_ = os.WriteFile(path, data, 0o644)
			}
		}
	}
	if updateAvailable(st.Latest, version) {
		_, _ = fmt.Fprintf(os.Stderr, "\nbulwark v%s is available (you have v%s) — run 'bulwark update'\n",
			st.Latest, version)
	}
}
